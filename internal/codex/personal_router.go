package codex

import (
	"context"
	"errors"
	"im-agent/internal/config"
	"os"
	"strings"
	"time"
)

// 复用 App Server 协议，单独进程和临时 thread 避免归类内容污染业务会话。
// 不发送平台消息，也不执行工具；退出时释放整个临时进程。
func ClassifyPersonal(ctx context.Context, cfg config.Config, input string) (string, error) {
	op, cancel := context.WithTimeout(ctx, 55*time.Second)
	defer cancel()
	cfg.WorkDir = ""
	cfg.Codex.SubmissionMode = "direct"
	cfg.Codex.RoutingOnly = true
	cfg.Codex.PersonalReply = true
	cfg.Codex.Sandbox = "read-only"
	cfg.Codex.ApprovalPolicy = "never"
	c, err := Start(op, cfg)
	if err != nil {
		return "", err
	}
	defer c.Close()
	t, err := c.StartThread(op, cfg)
	if err != nil {
		return "", err
	}
	defer os.Remove(t.Cwd)
	notices, closeSub, err := c.Subscribe(t.ID)
	if err != nil {
		return "", err
	}
	defer closeSub()
	turn, err := c.Turn(op, t.ID, input)
	if err != nil {
		return "", err
	}
	var result strings.Builder
	for {
		select {
		case <-op.Done():
			return "", op.Err()
		case n, ok := <-notices:
			if !ok {
				return "", errors.New("问题归类连接已结束")
			}
			e, ok := DecodeThreadEvent(n)
			if !ok || e.ThreadID != t.ID || e.TurnID != turn {
				continue
			}
			switch e.Kind {
			case "item":
				if result.Len()+len(e.Text) > 8192 {
					return "", errors.New("问题归类输出过长")
				}
				result.WriteString(e.Text)
			case "turn_completed":
				if e.Status != "completed" {
					return "", errors.New("问题归类未完成")
				}
				return result.String(), nil
			case "turn_failed":
				return "", errors.New("问题归类失败")
			}
		}
	}
}
