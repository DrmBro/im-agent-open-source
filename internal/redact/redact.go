package redact

import (
	"os"
	"regexp"
	"strings"
)

var bearer = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+`)
var named = regexp.MustCompile(`(?i)((?:api[_-]?key|app[_-]?secret|access[_-]?token|password)\s*[=:]\s*)[^\s"',;]+`)

type Filter struct{ secrets []string }

func New() *Filter {
	f := &Filter{}
	for _, e := range os.Environ() {
		k, v, ok := strings.Cut(e, "=")
		u := strings.ToUpper(k)
		if ok && len(v) >= 8 && (strings.Contains(u, "KEY") || strings.Contains(u, "SECRET") || strings.Contains(u, "TOKEN") || strings.Contains(u, "PASSWORD")) {
			f.secrets = append(f.secrets, v)
		}
	}
	return f
}
func (f *Filter) Text(s string) string {
	for _, v := range f.secrets {
		s = strings.ReplaceAll(s, v, "[已隐藏]")
	}
	s = bearer.ReplaceAllString(s, "${1}[已隐藏]")
	return named.ReplaceAllString(s, "${1}[已隐藏]")
}
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…（已截断）"
}

// NewWithSecrets 也过滤解密后仅存在内存中的 IM 凭证，不将它们写入日志或环境。
func NewWithSecrets(values []string) *Filter {
	f := New()
	for _, v := range values {
		if len(v) >= 8 {
			f.secrets = append(f.secrets, v)
		}
	}
	return f
}
