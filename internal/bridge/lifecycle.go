package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"im-agent/internal/im"
)

type execution struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// beginExecution 与关闭意图共用互斥锁，已关闭/排队中的消息不能恢复已归档会话。
func (b *Bridge) beginExecution(parent context.Context, thread string) (context.Context, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, e := b.store.ByThread(thread)
	if e != nil {
		return nil, nil, e
	}
	if s.Status != "active" {
		return nil, nil, errors.New("会话关闭中或已归档，不再执行排队消息")
	}
	if b.active[thread] != nil {
		return nil, nil, errors.New("同一 Codex 会话已有执行任务")
	}
	ctx, cancel := context.WithCancel(parent)
	run := &execution{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	b.active[thread] = run
	return ctx, func() { cancel(); b.mu.Lock(); delete(b.active, thread); close(run.done); b.mu.Unlock() }, nil
}
func (b *Bridge) requestClose(thread, source string, archived, closed bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if session, e := b.store.ByThread(thread); e == nil && session.Status == "detached" {
		return nil
	}
	if e := b.store.RequestClosure(thread, source, archived, closed); e != nil {
		return e
	}
	if active := b.active[thread]; active != nil {
		active.cancel()
	}
	for _, cancelReport := range b.reportOperations[thread] {
		cancelReport()
	}
	select {
	case b.closingWake <- struct{}{}:
	default:
	}
	return nil
}
func (b *Bridge) conversationClosed(ctx context.Context, event im.ConversationClosed) error {
	adapter := b.channels[event.Address.Connection]
	if adapter == nil || adapter.Identity().Channel != event.Address.Channel || event.Address.ChatID == "" {
		return errors.New("关闭事件来源不匹配")
	}
	shared, e := b.isSharedChat(event.Address)
	if e != nil {
		return e
	}
	if shared || event.Address.IsTopic() {
		return nil
	} // 不把共享群解散推断为所有话题归档。
	s, e := b.store.ByChat(event.Address)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	// 平台确实已解散这个绑定群，无需把“操作者在白名单”当成状态事实的前提。
	// 只有 SDK 的已验证解散事件或明确状态查询能进入这里，聊天文本无法调用此接口。
	return b.requestClose(s.ThreadID, "im", false, true)
}
func (b *Bridge) watchArchiveEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.codex.Done():
			return
		case id := <-b.codex.ArchivedEvents():
			if id == "" {
				continue
			}
			_, e := b.store.ByThread(id)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				b.setLifecycleWarning("处理 Codex 归档通知失败，稍后归档列表扫描补偿")
				continue
			}
			if e = b.requestClose(id, "codex", true, false); e != nil {
				b.setLifecycleWarning("持久化归档通知失败，稍后扫描补偿")
			}
		}
	}
}
func (b *Bridge) setLifecycleWarning(s string) {
	b.lifecycleStatusMu.Lock()
	b.lifecycleWarning = s
	b.lifecycleStatusMu.Unlock()
}
func (b *Bridge) LifecycleWarning() string {
	b.lifecycleStatusMu.Lock()
	defer b.lifecycleStatusMu.Unlock()
	return b.lifecycleWarning
}
func (b *Bridge) RetryClosure(thread string) error {
	if e := b.store.RetryClosure(thread); e != nil {
		return e
	}
	select {
	case b.closingWake <- struct{}{}:
	default:
	}
	return nil
}
func (b *Bridge) syncClosures(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	scanAt := time.Time{}
	scanIndex := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(scanAt) {
			b.reconcileClosureFacts(ctx, &scanIndex)
			scanAt = time.Now().Add(30 * time.Second)
		}
		jobs, e := b.store.ClosureRows(true)
		if e != nil {
			b.setLifecycleWarning("读取待关闭任务失败")
		} else {
			for _, job := range jobs {
				if ctx.Err() != nil {
					return
				}
				op, cancel := context.WithTimeout(ctx, 45*time.Second)
				e = b.closeOne(op, job.ThreadID)
				cancel()
				if e != nil {
					if saveErr := b.store.ClosureFailed(job.ThreadID, e.Error()); saveErr != nil {
						b.setLifecycleWarning("保存关闭重试状态失败")
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-b.closingWake:
		}
	}
}
func (b *Bridge) reconcileClosureFacts(ctx context.Context, scanIndex *int) {
	// 只使用明确的 archived=true 列表，不因为活跃列表漏项/分页/权限错误判断为归档。
	op, cancel := context.WithTimeout(ctx, 40*time.Second)
	archived, e := b.codex.Archived(op)
	cancel()
	if e != nil {
		b.setLifecycleWarning("Codex 归档列表暂不可读，未推断或删除任何群聊")
	} else {
		b.setLifecycleWarning("")
		for _, thread := range archived {
			if ctx.Err() != nil {
				return
			}
			s, e := b.store.ByThread(thread.ID)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				b.setLifecycleWarning("读取归档映射失败")
				continue
			}
			if s.Status == "archived" {
				continue
			}
			if e = b.requestClose(thread.ID, "codex", true, false); e != nil {
				b.setLifecycleWarning("记录归档同步任务失败")
			}
		}
	}
	sessions, e := b.store.LifecycleSessions()
	if e != nil {
		b.setLifecycleWarning("读取会话列表失败")
		return
	}
	if len(sessions) == 0 {
		return
	}
	// 限量轮询，补偿桥接离线期间的群解散；不扫描任意用户群，也不无限占用平台配额。
	count := len(sessions)
	if count > 20 {
		count = 20
	}
	start := *scanIndex % len(sessions)
	failedChecks := 0
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			return
		}
		s := sessions[(start+i)%len(sessions)]
		if s.Address.IsTopic() {
			continue
		}
		shared, sharedErr := b.isSharedChat(s.Address)
		if sharedErr != nil {
			failedChecks++
			continue
		}
		if shared {
			continue
		}
		a := b.channels[s.Address.Connection]
		lifecycle, ok := a.(im.LifecycleAdapter)
		if !ok {
			continue
		}
		op, cancel := context.WithTimeout(ctx, 12*time.Second)
		state, e := lifecycle.ConversationState(op, s.Address)
		cancel()
		if e != nil {
			failedChecks++
			continue
		}
		if state == im.ConversationGone {
			if e = b.requestClose(s.ThreadID, "im", false, true); e != nil {
				b.setLifecycleWarning("记录群解散状态失败")
			}
		}
	}
	if failedChecks > 0 {
		b.setLifecycleWarning(fmt.Sprintf("%d 个群的状态查询失败，未将权限/网络错误当成解散。请检查群信息权限和群解散事件订阅。", failedChecks))
	}
	*scanIndex = (start + count) % len(sessions)
}
func (b *Bridge) closeOne(ctx context.Context, thread string) error {
	job, e := b.store.Closure(thread)
	if e != nil {
		return e
	}
	if job.State != "pending" {
		return nil
	}
	b.mu.Lock()
	active := b.active[thread]
	if active != nil {
		active.cancel()
	}
	b.mu.Unlock()
	if active != nil {
		select {
		case <-ctx.Done():
			return errors.New("等待活动任务停止超时，未解散群聊")
		case <-active.done:
		}
	}
	if !job.CodexArchived {
		// 支持归档请求已成功但响应丢失的情况；先确认实际状态，避免盲目重复归档。
		archived, e := b.codex.Archived(ctx)
		if e != nil {
			return errors.New("无法确认 Codex 归档状态，等待重试")
		}
		found := false
		for _, t := range archived {
			if t.ID == thread {
				found = true
				break
			}
		}
		if !found {
			// 已解散独立自建群采用带持久回执保护的桌面优先归档；其他关闭路径不扩权。
			target, readErr := b.store.ByThread(thread)
			if readErr != nil {
				return readErr
			}
			if job.ChatClosed && target.Managed && !target.Address.IsTopic() && target.Address.RootMessageID == "" {
				e = b.archiveClosedConversation(ctx, thread)
			} else {
				e = b.codex.Archive(ctx, thread)
			}
			if e != nil {
				if job.ChatClosed {
					return fmt.Errorf("群已确认解散，Codex 会话仍待归档：%w", e)
				}
				return fmt.Errorf("Codex 归档失败，未解散群聊：%w", e)
			}
		}
		if e = b.store.ConfirmClosure(thread, true); e != nil {
			return errors.New("Codex 已归档，但保存确认状态失败")
		}
	}
	// 网络等待期间可能先收到解散事件，再读取一次单调状态。
	job, e = b.store.Closure(thread)
	if e != nil {
		return e
	}
	s, e := b.store.ByThread(thread)
	if e != nil {
		return e
	}
	if s.Address.IsTopic() {
		if !s.Managed {
			return errors.New("话题没有客户端创建证明，不更新入口")
		}
		if e = b.updateTopicPresentation(ctx, s, b.topicName(s), "archived"); e != nil {
			return e
		}
		return b.store.FinishTopicClosure(thread)
	}
	shared, e := b.isSharedChat(s.Address)
	if e != nil {
		return e
	}
	if shared {
		return errors.New("目标是受保护的共享群，禁止自动解散；请检查旧群级绑定")
	}
	if job.ChatClosed {
		return b.store.FinishClosure(thread, false)
	}
	if !job.Managed {
		return b.store.FinishClosure(thread, true)
	}
	adapter := b.channels[s.Address.Connection]
	if adapter == nil {
		return errors.New("未配置原 IM 连接，等待恢复连接后同步")
	}
	lifecycle, ok := adapter.(im.LifecycleAdapter)
	if !ok || !adapter.Capabilities().CloseConversation {
		return errors.New("原 IM 适配器不支持关闭会话，未尝试删除")
	}
	state, e := lifecycle.ConversationState(ctx, s.Address)
	if e != nil {
		return fmt.Errorf("群状态查询失败，未推断已解散：%w", e)
	}
	switch state {
	case im.ConversationGone: // 明确事实，允许补偿丢失的回调。
	case im.ConversationOpen:
		if e = lifecycle.CloseConversation(ctx, s.Address); e != nil {
			return fmt.Errorf("解散群失败，等待重试：%w", e)
		}
	default:
		return errors.New("IM 群状态不明确，未执行解散")
	}
	if e = b.store.ConfirmClosure(thread, false); e != nil {
		return errors.New("群已解散，但保存确认状态失败")
	}
	return b.store.FinishClosure(thread, false)
}
