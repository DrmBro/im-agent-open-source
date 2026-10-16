package codex

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"im-agent/internal/diagnostics"
)

var desktopThreadID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// 采用已安装桌面的原生深层链接入口，明确交给 Codex bundle 而非任意 URL 处理器。
// 链接只包含校验后的 ID，不携带问题或执行命令；真正发送仍由 desktopGuard 校验。
func openDesktopThread(parent context.Context, thread string) error {
	if !desktopThreadID.MatchString(thread) {
		return diagnostics.UserError("桌面目标会话 ID 无效，未打开或发送")
	}
	if runtime.GOOS != "darwin" {
		return diagnostics.UserError("当前系统暂不支持通过会话链接自动打开未加载会话，未发送")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/open", "-b", "com.openai.codex", "codex://threads/"+strings.ToLower(thread))
	if err := cmd.Run(); err != nil {
		return diagnostics.UserError("桌面会话链接打开失败，未发送；请确认 Codex 桌面已安装并运行")
	}
	return nil
}
