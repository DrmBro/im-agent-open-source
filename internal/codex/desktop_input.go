package codex

import "strings"

// desktopInputMatches 首选完整原文匹配；兼容已实测的桌面纯文本下划线序列化。
// 不反向删除实际输入的转义、不做包含/相似度匹配，也不改写用户提交内容。
// 涉及代码、原始反斜线时只接受完整原文，避免把代码或路径当成等价 Markdown。
func desktopInputMatches(actual, expected string) bool {
	actual, expected = strings.TrimSpace(actual), strings.TrimSpace(expected)
	if actual == expected {
		return true
	}
	if strings.ContainsAny(expected, "`\\") {
		return false
	}
	return strings.Contains(expected, "_") && actual == strings.ReplaceAll(expected, "_", "\\_")
}
