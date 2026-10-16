package bridge

import (
	"context"
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
			if adapter == nil || !b.allowed(msg.Address.Connection, msg.UserID) {
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

func (b *Bridge) scheduleInbound(item store.QueuedInbound) error {
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
		return err
	}
	if session.Status != "active" || store.Hash(session.Address.Key()) != item.AddressKey {
		return diagnostics.UserError("排队请求的目标会话或地址已改变，不跨会话转发")
	}
	adapter := b.channels[session.Address.Connection]
	if adapter == nil {
		return diagnostics.UserError("请求对应 IM 连接不存在")
	}
	if err = b.jobs.Push(session.Address.Key(), func(jobCtx context.Context) {
		defer b.inboundScheduled.Delete(item.EventID)
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
			b.messageDiagnostic(notice.Status, notice.Text())
			b.log.Error("Codex 请求处理未完成", "chat", session.Address.ChatID, "stage", notice.Stage)
			_ = b.sendTextLimited(jobCtx, adapter, session.Address, notice.Text(), item.EventID)
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
