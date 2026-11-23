package bridge

import (
	"context"
	"errors"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/diagnostics"
	"im-agent/internal/store"
)

// 输入和最终回复分别由持久收件队列与飞书镜像投递，不改变原钉钉绑定。
func (b *Bridge) scheduleFeishuInput(s store.Session, item store.QueuedInbound) error {
	key := store.Hash(s.Address.Key())
	if s.ThreadID != item.ThreadID || key != item.AddressKey || s.Status != "active" || !b.messageAllowed(b.channels[s.Address.Connection], item.Message) {
		return b.rejectInbound(item.EventID, item.AddressKey, "原飞书群绑定或发送者权限已变化")
	}
	if _, busy := b.steerScheduled.LoadOrStore(item.EventID, true); busy {
		return nil
	}
	err := b.controlJobs.Push("steer:"+s.ThreadID, func(parent context.Context) {
		defer b.steerScheduled.Delete(item.EventID)
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		claimed, err := b.store.BeginQueuedInbound(item.EventID, s.ThreadID, key)
		if err != nil || !claimed {
			return
		}
		if ctx.Err() != nil {
			return
		}
		b.submitFeishuInput(ctx, s, item)
	})
	if err != nil {
		b.steerScheduled.Delete(item.EventID)
	}
	return err
}

func (b *Bridge) submitFeishuInput(ctx context.Context, s store.Session, item store.QueuedInbound) {
	msg := item.Message
	a := b.channels[s.Address.Connection]
	beforeWrite := true
	submit := func() (string, error) {
		text, err := b.receiveAttachments(ctx, a, s, msg)
		if err != nil {
			return "", err
		}
		engine := b.engineForThread(s.ThreadID)
		if engine == nil {
			return "", errors.New("会话执行连接未就绪")
		}
		return engine.TurnWithHooks(ctx, s.ThreadID, text, codex.TurnHooks{
			ExternalObserver: true, AllowSteer: true, RequestID: item.EventID,
			BeforeSubmit: func(path string, offset int64) error {
				current, err := b.store.FeishuInputSession(s.Address)
				if err != nil {
					return err
				}
				if current.ThreadID != s.ThreadID || current.Status != "active" || !b.messageAllowed(a, msg) {
					return errors.New("原会话绑定或发送者权限已变化")
				}
				if path == "" {
					return errors.New("目标会话日志暂不可用")
				}
				original, rendered := codex.DesktopInputForms(text)
				if err := b.store.EventStage(item.EventID, s.Address.Key(), "submitting", ""); err != nil {
					return err
				}
				if err := b.store.RegisterMirrorOrigin(s.ThreadID, item.EventID, path, offset, store.Hash(original), store.Hash(rendered)); err != nil {
					return err
				}
				beforeWrite = false
				return nil
			},
		})
	}
	turn, err := submit()
	if err == nil {
		if e := b.store.FinishFeishuInput(item.EventID, "mirror_submitted", "done", turn); e != nil {
			b.log.Warn("保存飞书输入提交回执失败", "event", item.EventID)
		}
		return
	}
	stage, status, hint := "submission_unconfirmed", "uncertain", "提交回执待核对，请先查看原会话。"
	if beforeWrite {
		stage, status, hint = "not_sent", "failed", "尚未提交："+diagnostics.Error(err)
	}
	if e := b.store.FinishFeishuInput(item.EventID, stage, status, hint); e != nil {
		b.log.Warn("飞书输入状态保存失败", "event", item.EventID)
	}
	if ctx.Err() == nil {
		_, _ = a.SendText(ctx, s.Address, hint, item.EventID+":submit-status")
	}
}
