package bridge

import (
	"context"
	"time"

	"im-agent/internal/im"
	"im-agent/internal/store"
)

// 只通知存在本机待恢复快照，绝不把草稿内容发送到IM，也不重放模型请求。
func (b *Bridge) enqueueDraftRecoveryNotice(parent context.Context, adapter im.Adapter, session store.Session, msg im.Message) {
	if b.statusCardJobs == nil || parent.Err() != nil {
		return
	}
	_ = b.statusCardJobs.Push(session.ThreadID, func(ctx context.Context) {
		if ctx.Err() != nil || parent.Err() != nil {
			return
		}
		current, err := b.store.ByThread(session.ThreadID)
		if err != nil || current.Status != "active" || current.Address.Key() != session.Address.Key() {
			return
		}
		noticeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, _ = adapter.SendText(noticeCtx, session.Address,
			"桌面草稿已有本机加密快照，但未确认自动恢复。请在 IM Agent「任务与附件 → 草稿恢复」查看。此提醒不代表飞书问题提交失败，请核对任务结果，不要直接重发。",
			msg.ID+":desktop-draft-recovery")
	})
}
