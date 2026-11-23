package bridge

import (
	"context"
	"database/sql"
	"errors"
	"im-agent/internal/codex"
	"im-agent/internal/store"
	"time"
)

// 使用短操作队列检查当前轮次，不等待同会话长任务结束。
func (b *Bridge) scheduleInbound(item store.QueuedInbound) error {
	if item.Message.Address.Channel != "feishu" || b.controlJobs == nil {
		return b.scheduleNormalInbound(item)
	}
	mirror, err := b.store.FeishuInputSession(item.Message.Address)
	if err == nil {
		return b.scheduleFeishuInput(mirror, item)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, busy := b.steerScheduled.LoadOrStore(item.EventID, true); busy {
		return nil
	}
	err = b.controlJobs.Push("steer:"+item.ThreadID, func(parent context.Context) {
		defer b.steerScheduled.Delete(item.EventID)
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		s, err := b.store.ByThread(item.ThreadID)
		if err != nil {
			return
		}
		adapter := b.channels[s.Address.Connection]
		if b.inboundAccess(s, item.Message) != nil {
			return
		}
		turn, err := b.engineForThread(s.ThreadID).ActiveControlTurn(ctx, s.ThreadID)
		if err != nil || turn == "" {
			_ = b.scheduleNormalInbound(item)
			return
		}
		claimed, err := b.store.ReserveSteer(s, item.EventID, turn)
		if err != nil || !claimed {
			return
		}
		text := item.Message.Text
		if len(item.Message.Attachments) > 0 {
			text, err = b.receiveAttachments(ctx, adapter, s, item.Message)
			if err != nil {
				_ = b.store.FinishSteer(item.EventID, "failed", "附件处理失败，补充尚未提交")
				return
			}
		}
		if err = b.inboundAccess(s, item.Message); err != nil {
			_ = b.store.FinishSteer(item.EventID, "failed", "会话或权限已改变，补充尚未提交")
			return
		}
		// 复用输入来源去重，避免补充消息被再次镜像回原飞书群。
		files, originErr := b.engineForThread(s.ThreadID).LiveRolloutFiles(ctx, s.ThreadID)
		if originErr == nil && len(files) > 0 {
			path := files[len(files)-1]
			_, offset, e := b.engineForThread(s.ThreadID).LiveRolloutInfo(ctx, s.ThreadID, path)
			if e == nil {
				original, rendered := codex.DesktopInputForms(text)
				originErr = b.store.RegisterMirrorOrigin(s.ThreadID, item.EventID, path, offset, store.Hash(original), store.Hash(rendered))
			} else {
				originErr = e
			}
		}
		if originErr != nil {
			_ = b.store.FinishSteer(item.EventID, "failed", "补充来源无法保存，尚未提交")
			return
		}
		err = b.engineForThread(s.ThreadID).SteerExactTurn(ctx, s.ThreadID, turn, text)
		if err != nil {
			_ = b.store.FinishSteer(item.EventID, "unknown", "补充回执未确认，请核对 Codex 当前轮次；不会自动重新提交")
		} else {
			_ = b.store.FinishSteer(item.EventID, "submitted", "已补充到当前轮次；最终回复沿用原轮次同步，不再单独排队执行")
		}
		_ = b.enqueueSessionStatus(s.ThreadID, true, item.EventID)
	})
	if err != nil {
		b.steerScheduled.Delete(item.EventID)
	}
	return err
}
