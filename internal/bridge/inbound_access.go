package bridge

import (
	"database/sql"
	"errors"

	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) messageAllowed(adapter im.Adapter, msg im.Message) bool {
	if adapter == nil {
		return false
	}
	if scoped, ok := adapter.(im.InboundScopeAdapter); ok {
		return scoped.AcceptsMessage(msg)
	}
	return b.allowed(msg.Address.Connection, msg.UserID)
}

// 使用本次启动的配置快照；配置保存仍按现有约定在重启桥接后生效。
func (b *Bridge) inboundAccess(s store.Session, msg im.Message) error {
	adapter := b.channels[s.Address.Connection]
	if adapter == nil || adapter.Identity().Channel != s.Address.Channel ||
		msg.Address.Connection != s.Address.Connection || msg.Address.Channel != s.Address.Channel ||
		!b.messageAllowed(adapter, msg) {
		return diagnostics.UserError("原发送者或连接权限已失效，未提交且不会恢复旧请求")
	}
	current, err := b.store.ByThread(s.ThreadID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err // 数据库暂不可读不视为撤权，保留原请求等待核对。
	}
	if err != nil || current.Status != "active" || current.Address.Key() != s.Address.Key() {
		return diagnostics.UserError("原会话已关闭或绑定已改变，未提交且不会跨会话恢复")
	}
	return nil
}

func (b *Bridge) rejectInbound(event, key, reason string) error {
	rejected, err := b.store.RejectUnsubmittedInbound(event, key, reason)
	if err != nil {
		return err
	}
	if rejected {
		b.log.Warn("未提交的旧输入已失效，不自动恢复", "event", event)
	}
	return nil
}
