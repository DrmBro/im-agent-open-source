package codex

import (
	"encoding/json"
	"fmt"
	"strings"
)

// UserMessageTextForDisplay 仅转换完整的桌面引用批注协议。
// 原始输入仍由调用方保留，用于来源匹配；普通正文与不可解析内容保持原样。
func UserMessageTextForDisplay(text string) string {
	normalized := strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	const prefix = "# Response annotations:\nEach item contains text selected from an earlier Codex response"
	if !strings.HasPrefix(normalized, prefix) {
		return text
	}
	_, content, ok := strings.Cut(normalized, "\n<response-annotations>\n")
	if !ok {
		return text
	}
	raw, tail, ok := strings.Cut(content, "\n</response-annotations>")
	if !ok {
		return text
	}
	request, ok := strings.CutPrefix(strings.TrimSpace(tail), "## My request:")
	if !ok || request != "" && !strings.HasPrefix(request, "\n") {
		return text
	}
	var annotations []struct {
		Text       string `json:"text"`
		Annotation string `json:"annotation"`
	}
	if json.Unmarshal([]byte(raw), &annotations) != nil || len(annotations) == 0 {
		return text
	}
	parts := make([]string, 0, len(annotations)+1)
	for i, annotation := range annotations {
		if strings.TrimSpace(annotation.Text) == "" {
			return text
		}
		part := fmt.Sprintf("引用 %d：\n%s", i+1, annotation.Text)
		if strings.TrimSpace(annotation.Annotation) != "" {
			part += "\n评论：" + annotation.Annotation
		}
		parts = append(parts, part)
	}
	if request = strings.TrimSpace(request); request != "" {
		parts = append(parts, "补充说明：\n"+request)
	}
	return strings.Join(parts, "\n\n")
}
