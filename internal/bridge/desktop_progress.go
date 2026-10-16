package bridge

import (
	"context"
	"time"

	"im-agent/internal/im"
	"im-agent/internal/store"
)

type desktopProgress struct {
	stage string
	hint  string
}

// 每请求仅一个观察者、一格快照；不是模型执行队列，不会提交或重放问题。
// 连续进度过快时合并为最新快照，回调不等待数据库、网络或观察者。
func (b *Bridge) observeDesktopProgress(parent context.Context, adapter im.Adapter, session store.Session, msg im.Message) (func(string, string), func()) {
	ctx, cancel := context.WithCancel(parent)
	updates := make(chan desktopProgress, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		retry := time.NewTicker(time.Second)
		defer retry.Stop()
		waitNotified := map[string]bool{}
		var pending *desktopProgress
		warned := false
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-updates:
				pending, warned = &update, false
			case <-retry.C:
				// 仅重试未成功的最新阶段记录，不重试模型或文字通知。
			}
		drain:
			for {
				select {
				case update := <-updates:
					pending, warned = &update, false
				default:
					break drain
				}
			}
			if ctx.Err() != nil {
				return
			}
			if pending == nil {
				continue
			}
			update := *pending
			writeCtx, finishWrite := context.WithTimeout(ctx, time.Second)
			changed, err := b.store.EventWaitingStage(writeCtx, msg.ID, session.Address.Key(), update.stage)
			finishWrite()
			if err != nil {
				if ctx.Err() == nil && !warned {
					b.log.Warn("等待阶段暂未记录，将仅重试最新观察状态", "event", msg.ID, "stage", update.stage)
					warned = true
				}
				continue
			}
			pending = nil
			if !changed || ctx.Err() != nil {
				continue
			}
			b.messageDiagnostic(update.stage, update.hint)
			_ = b.enqueueSessionStatus(session.ThreadID, false, msg.ID)
			if (update.stage == "waiting_draft" || update.stage == "waiting_desktop") && !waitNotified[update.stage] {
				waitNotified[update.stage] = true
				b.enqueueDesktopWaitNotice(ctx, adapter, session, msg, update)
			}
		}
	}()
	publish := func(stage, hint string) {
		if ctx.Err() != nil {
			return
		}
		update := desktopProgress{stage: stage, hint: hint}
		select {
		case updates <- update:
		default:
			// 当前调用链串行回调；丢弃未消费的旧快照，只保留最新原因。
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- update:
			default:
			}
		}
	}
	stop := func() {
		cancel()
		<-done
	}
	return publish, stop
}

// 补充文字走既有独立展示队列，观察者收拢不等待令牌锁或平台网络。
// 排队期间请求结束或原因已变化则跳过；未送达不重试，不影响模型执行。
func (b *Bridge) enqueueDesktopWaitNotice(ctx context.Context, adapter im.Adapter, session store.Session, msg im.Message, update desktopProgress) {
	if b.statusCardJobs == nil || ctx.Err() != nil {
		return
	}
	_ = b.statusCardJobs.Push(session.ThreadID, func(displayCtx context.Context) {
		if ctx.Err() != nil || displayCtx.Err() != nil {
			return
		}
		view, err := b.store.ReadSessionRequestStatus(session.ThreadID, msg.ID)
		if err != nil || view.Session.Status != "active" || view.Session.Address.Key() != session.Address.Key() || view.Task.Status != "processing" || view.Task.Stage != update.stage {
			return
		}
		if ctx.Err() != nil || displayCtx.Err() != nil {
			return
		}
		noticeCtx, finishNotice := context.WithTimeout(ctx, 2*time.Second)
		defer finishNotice()
		_, _ = adapter.SendText(noticeCtx, session.Address, update.hint, msg.ID+":desktop-wait:"+update.stage)
	})
}
