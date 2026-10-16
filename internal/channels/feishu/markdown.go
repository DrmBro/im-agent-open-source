package feishu

import (
	"context"
	"regexp"
	"strings"

	"im-agent/internal/im"
)

var _ im.MarkdownAdapter = (*Client)(nil)

// 回复里的示例/模型输出不应被解释成飞书提醒他人的指令。
var markdownMention = regexp.MustCompile(`(?i)<at\b`)

// SendMarkdown只改变展示格式，目标、幂等、顺序与送达不确定保护复用send。
func (c *Client) SendMarkdown(ctx context.Context, a im.Address, text, key string) (string, error) {
	text = escapeMarkdownMentions(text)
	content := marshal(map[string]any{
		"zh_cn": map[string]any{
			"content": [][]map[string]string{{{"tag": "md", "text": text}}},
		},
	})
	return c.send(ctx, a, "post", content, key)
}

// 仅转义正文中的飞书扩展标签，代码示例的原文保持不变。
func escapeMarkdownMentions(text string) string {
	var out, prose strings.Builder
	var fence string
	flush := func() {
		out.WriteString(escapeInlineMentions(prose.String()))
		prose.Reset()
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		marker, tail := "", ""
		if indent <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 0
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				marker, tail = trimmed[:n], strings.TrimSpace(trimmed[n:])
			}
		}
		if fence != "" {
			out.WriteString(line)
			if len(marker) >= len(fence) && marker[0] == fence[0] && tail == "" {
				fence = ""
			}
			continue
		}
		if marker != "" && (marker[0] != '`' || !strings.Contains(tail, "`")) {
			flush()
			fence = marker
			out.WriteString(line)
			continue
		}
		prose.WriteString(line)
	}
	flush()
	return out.String()
}
func escapeInlineMentions(text string) string {
	var out strings.Builder
	for len(text) > 0 {
		start := strings.IndexByte(text, '`')
		if start < 0 {
			out.WriteString(markdownMention.ReplaceAllString(text, "&lt;at"))
			break
		}
		backslashes := 0
		for i := start - 1; i >= 0 && text[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			out.WriteString(markdownMention.ReplaceAllString(text[:start+1], "&lt;at"))
			text = text[start+1:]
			continue
		}
		out.WriteString(markdownMention.ReplaceAllString(text[:start], "&lt;at"))
		text = text[start:]
		n := 0
		for n < len(text) && text[n] == '`' {
			n++
		}
		end := -1
		for i := n; i < len(text); {
			if text[i] != '`' {
				i++
				continue
			}
			j := i
			for j < len(text) && text[j] == '`' {
				j++
			}
			if j-i == n {
				end = j
				break
			}
			i = j
		}
		if end < 0 {
			out.WriteString(text[:n])
			text = text[n:]
			continue
		}
		out.WriteString(text[:end])
		text = text[end:]
	}
	return out.String()
}
