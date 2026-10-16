package bridge

import (
	"context"
	"database/sql"
	"errors"

	"im-agent/internal/diagnostics"
	"im-agent/internal/store"
)

// 仅供群已明确解散的独立自建群调用；不接受聊天文本中的任意会话ID。
// closeOne先核对归档事实，本方法再检查未知发送记录，绝不未知后切换服务重发。
func (b *Bridge) archiveClosedConversation(ctx context.Context, thread string) error {
	s, e := b.store.ByThread(thread)
	if e != nil {
		return e
	}
	job, e := b.store.Closure(thread)
	if e != nil {
		return e
	}
	if s.Status != "closing" || !s.Managed || s.Address.IsTopic() || s.Address.RootMessageID != "" || !job.ChatClosed || job.State != "pending" || job.CodexArchived {
		return errors.New("群关闭状态或会话绑定发生变化，未发送兼容归档")
	}
	shared, e := b.isSharedChat(s.Address)
	if e != nil {
		return e
	}
	if shared {
		return errors.New("共享群禁止调用独立群归档入口")
	}
	previous, e := b.store.ClosureArchiveAttempt(thread)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if e == nil {
		if previous.AddressKey != store.Hash(s.Address.Key()) {
			return errors.New("归档目标地址已变化，未继续旧归档尝试")
		}
		switch previous.State {
		case "confirmed":
			return nil // 已有成功回执，只补交本地关闭确认。
		case "sending", "unknown":
			return diagnostics.UserError("群已解散，归档结果未确认；正在只读核对归档列表。不会重复发送或切换通道，请在桌面核对，必要时手动归档")
		case "rejected": // 只有明确未发送或明确拒绝才可在原退避后重试。
		default:
			return errors.New("归档尝试状态无效，未发送")
		}
	}
	plan, e := b.codex.PrepareClosedChatArchive(ctx, thread)
	if e != nil {
		return e
	}
	defer plan.Close()
	// 探测只读，可能与解除绑定/归档事件并发；发送前再次读取共享群配置和持久事实。
	current, e := b.store.ByThread(thread)
	if e != nil {
		return e
	}
	if current.Address.Key() != s.Address.Key() {
		return errors.New("归档前会话绑定发生变化")
	}
	shared, e = b.isSharedChat(current.Address)
	if e != nil {
		return e
	}
	if shared {
		return errors.New("目标已变为受保护共享群，未发送")
	}
	attempt, claimed, e := b.store.ReserveClosureArchive(current, plan.Route())
	if e != nil {
		return e
	}
	if !claimed {
		return errors.New("已有归档发送记录，未重复发送；等待状态核对")
	}
	outcome := plan.Send(ctx)
	if e = b.store.CompleteClosureArchive(attempt, outcome.State, outcome.Detail); e != nil {
		return errors.New("归档结果未能持久化，保留发送中状态；不会自动重发，请等待核对")
	}
	if outcome.State != "confirmed" {
		return errors.New(outcome.Detail)
	}
	return nil
}
