package bridge

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"im-agent/internal/diagnostics"
	"im-agent/internal/queue"
	"im-agent/internal/store"
)

// 队列仅作为运行时加速器；存储中的 queued 才是可靠的待执行来源。
func (b *Bridge) syncInboundQueue(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		received, readErr := b.store.ReceivedInbounds()
		if readErr != nil {
			b.log.Warn("读取未处理收件部分失败", "error", diagnostics.Error(readErr))
		}
		for _, msg := range received {
			if ctx.Err() != nil {
				return
			}
			adapter := b.channels[msg.Address.Connection]
			if adapter == nil || !b.messageAllowed(adapter, msg) {
				if err := b.rejectInbound(msg.ID, store.Hash(msg.Address.Key()), "恢复收件时原发送者或连接权限已失效，未执行旧输入"); err != nil {
					b.log.Warn("保存失效收件状态失败", "error", diagnostics.Error(err))
				}
				continue
			}
			if _, retired, err := b.store.RetiredTarget(msg.Address); err != nil {
				continue
			} else if retired {
				_ = b.rejectInbound(msg.ID, store.Hash(msg.Address.Key()), "原收件入口已迁移，未把旧输入送往新入口")
				continue
			}
			lock := b.imageIntakeLock(msg)
			lock.Lock()
			current, checkErr := b.store.ReceivedInbound(msg)
			if checkErr == nil && current {
				if err := b.processClaimedInbound(ctx, adapter, msg); err != nil {
					b.log.Warn("恢复已收件请求失败，保留原记录", "error", diagnostics.Error(err))
				}
			}
			lock.Unlock()
		}
		pending, err := b.store.QueuedInbounds()
		if err != nil {
			b.log.Warn("读取持久待调度请求部分失败", "error", diagnostics.Error(err))
		}
		for _, item := range pending {
			if ctx.Err() != nil {
				return
			}
			if err := b.scheduleInbound(item); err != nil {
				if errors.Is(err, queue.ErrFull) {
					break // 队列满，容量释放后继续。
				}
				b.log.Warn("本地请求暂无法调度，不阻塞其它消息", "event", item.EventID, "error", diagnostics.Error(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bridge) scheduleNormalInbound(item store.QueuedInbound) error {
	if _, loaded := b.inboundScheduled.LoadOrStore(item.EventID, true); loaded {
		return nil
	}
	clear := true
	defer func() {
		if clear {
			b.inboundScheduled.Delete(item.EventID)
		}
	}()
	session, err := b.store.ByThread(item.ThreadID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return b.rejectInbound(item.EventID, item.AddressKey, "原排队会话不存在，未执行旧输入")
		}
		return err
	}
	if session.Status != "active" || store.Hash(session.Address.Key()) != item.AddressKey {
		return b.rejectInbound(item.EventID, item.AddressKey, "排队请求的目标会话或地址已改变，不跨会话转发")
	}
	if err = b.inboundAccess(session, item.Message); err != nil {
		var denied diagnostics.UserError
		if errors.As(err, &denied) {
			return b.rejectInbound(item.EventID, item.AddressKey, string(denied))
		}
		return err
	}
	adapter := b.channels[session.Address.Connection]
	if adapter == nil {
		return diagnostics.UserError("请求对应 IM 连接不存在")
	}
	if err = b.jobs.Push(session.Address.Key(), func(jobCtx context.Context) {
		defer b.inboundScheduled.Delete(item.EventID)
		if jobCtx.Err() != nil {
			return // 普通停止保留明确未提交的队列，不能当作主动撤权。
		}
		if err := b.inboundAccess(session, item.Message); err != nil {
			var denied diagnostics.UserError
			if errors.As(err, &denied) {
				if saveErr := b.rejectInbound(item.EventID, item.AddressKey, string(denied)); saveErr != nil {
					b.log.Warn("保存排队请求失效状态失败", "error", diagnostics.Error(saveErr))
				}
			}
			return
		}
		// 停止/重启与用户取消后，已入队的内存快照不可再次提交。
		current, checkErr := b.store.BeginQueuedInbound(item.EventID, session.ThreadID, item.AddressKey)
		if checkErr != nil || !current {
			return
		}
		if e := b.runTurn(jobCtx, adapter, session, item.Message); e != nil {
			notice, recorded := b.recordRequestFailure(item.EventID, session.Address.Key(), e)
			if !recorded {
				return
			}
			if adapter.Capabilities().PersonalInbox {
				if err := b.store.PutPersonalFailure(session, item.Message, "处理未完成："+notice.Text()); err == nil {
					if notify, err := b.store.ClaimPersonalNotice(item.EventID); err == nil && notify {
						b.notifyPersonalReview(jobCtx)
					}
				}
			}
			b.messageDiagnostic(notice.Status, notice.Text())
			b.log.Error("Codex 请求处理未完成", "chat", session.Address.ChatID, "stage", notice.Stage)
			if !adapter.Capabilities().PersonalInbox {
				_ = b.sendTextLimited(jobCtx, adapter, session.Address, notice.Text(), item.EventID)
			}
		} else {
			_ = b.store.EventStage(item.EventID, session.Address.Key(), "completed", "")
			b.messageDiagnostic("completed", "已完成一次任务回复")
			_ = b.store.EventDone(item.EventID, "done")
			_ = b.store.FinishImageDraft(item.EventID, "done")
		}
	}); err != nil {
		return err
	}
	clear = false
	return nil
}
