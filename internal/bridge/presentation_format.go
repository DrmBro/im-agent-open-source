package bridge

import (
	"strings"
	"unicode"

	"im-agent/internal/codex"
	"im-agent/internal/redact"
	"im-agent/internal/store"
)

// conversationName 跨 IM 共用命名规则：会话名在最前，55字符内保留标识和ID后缀。
func conversationName(prefix, title, id string) string {
	clean := func(s string) string {
		return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }), " ")
	}
	clip := func(s string, n int) string {
		r := []rune(s)
		if len(r) > n {
			return string(r[:n-1]) + "…"
		}
		return s
	}
	prefix = clean(prefix)
	if prefix == "" {
		prefix = "Codex"
	}
	prefix = clip(prefix, 12)
	title = clean(title)
	if title == "" {
		title = "未命名会话"
	}
	token := strings.ReplaceAll(id, "-", "")
	if len(token) < 8 || strings.IndexFunc(token, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') }) >= 0 {
		token = store.Hash(id)
	}
	suffix := token[len(token)-8:]
	room := 55 - len([]rune(prefix)) - len([]rune(suffix)) - 6
	return clip(title, room) + " · " + prefix + " · " + suffix
}
func (b *Bridge) presentationFilter() *redact.Filter {
	var values []string
	for _, c := range b.cfg.Connections {
		for _, v := range c.Credentials {
			values = append(values, v)
		}
	}
	return redact.NewWithSecrets(values)
}
func (b *Bridge) threadName(thread codex.Thread, connection string) string {
	return conversationName(b.conn(connection).ChatNamePrefix, b.presentationFilter().Text(thread.Name), thread.ID)
}
func (b *Bridge) historyText(messages []codex.HistoryMessage, thread ...string) string {
	filter := b.presentationFilter()
	limit := b.cfg.Limits.MaxMessageChars
	if limit < 200 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	parts := []string{}
	for _, m := range messages {
		if len(thread) > 0 {
			m = b.userMessageForDisplay(thread[0], m)
		}
		if m.Role == "user" {
			m.Text = codex.UserMessageTextForDisplay(m.Text)
		}
		label := "👤 用户"
		if m.Role == "assistant" {
			label = "🤖 助手"
		}
		parts = append(parts, label+"\n"+redact.Clip(filter.Text(m.Text), limit))
	}
	return strings.Join(parts, "\n\n")
}
