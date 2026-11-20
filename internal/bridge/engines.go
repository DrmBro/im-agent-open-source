package bridge

import (
	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

// SetCLI 在 Start 之前装配；两个客户端独立持有进程、订阅和生命周期，不复制 mutex。
func (b *Bridge) SetCLI(client *codex.Client) { b.cli = client }
func (b *Bridge) engineForConnection(connection string) *codex.Client {
	if config.UsesDirectCodex(b.conn(connection).Type) {
		return b.cli
	}
	return b.codex
}
func (b *Bridge) configForConnection(connection string) config.Config {
	cfg := b.cfg
	if config.UsesDirectCodex(b.conn(connection).Type) {
		cfg.Codex.SubmissionMode = "direct"
	}
	if config.IsPersonalChannel(b.conn(connection).Type) {
		cfg.Codex.PersonalReply = true
	}
	return cfg
}
func (b *Bridge) configForMessage(m im.Message) config.Config {
	cfg := b.configForConnection(m.Address.Connection)
	cfg.Codex.InboxSources = m.InboxSources
	return cfg
}
func (b *Bridge) SetDesktop(client *codex.Client) { b.desktop = client }
func (b *Bridge) desktopDispatch(thread string) bool {
	route, _ := b.threadChannels.Load(thread)
	return route == store.DesktopAssistantRoute
}
func (b *Bridge) desktopDispatchConfig(m im.Message) config.Config {
	cfg := b.configForMessage(m)
	cfg.Codex.SubmissionMode = "native_queue"
	return cfg
}
func (b *Bridge) configForThreadMessage(thread string, m im.Message) config.Config {
	if b.desktopDispatch(thread) {
		return b.desktopDispatchConfig(m)
	}
	return b.configForMessage(m)
}
func (b *Bridge) engineForThread(thread string) *codex.Client {
	if b.desktopDispatch(thread) {
		return b.desktop
	}
	if channel, ok := b.threadChannels.Load(thread); ok && config.UsesDirectCodex(channel.(string)) {
		return b.cli
	}
	return b.codex
}
func (b *Bridge) saveSession(s store.Session) error {
	// 先登记路由，后台扫描即使紧随事务提交也不会误走桌面。
	route := s.Address.Channel
	if s.DesktopDispatch {
		route = store.DesktopAssistantRoute
	}
	b.threadChannels.Store(s.ThreadID, route)
	return b.store.Save(s)
}
