package dingtalk

import (
	"context"
	"errors"
	"strings"
	"time"

	"im-agent/internal/im"
)

func isIdentityCommand(text string) bool {
	return strings.EqualFold(strings.TrimSpace(text), "/whoami")
}

// replyIdentity is a fixed response to the authenticated caller's own identifier.
// It never reaches the question runner and never changes a user's authorization.
func (c *Client) replyIdentity(ctx context.Context, msg im.Message) error {
	key := msg.Address.ChatID + "\x00" + msg.ID
	now := time.Now()
	c.identityMu.Lock()
	if c.identityReplies == nil {
		c.identityReplies = map[string]time.Time{}
	}
	if at, ok := c.identityReplies[key]; ok && now.Sub(at) < 2*time.Hour {
		c.identityMu.Unlock()
		return nil
	}
	for id, at := range c.identityReplies {
		if now.Sub(at) >= 2*time.Hour {
			delete(c.identityReplies, id)
		}
	}
	if len(c.identityReplies) >= 256 {
		c.identityMu.Unlock()
		c.setStatus("connected", "身份识别请求已达暂存上限，暂缓回复；已保留现有去重记录")
		return nil
	}

	c.identityReplies[key] = now
	c.identityMu.Unlock()
	text := "你的企业 UserID：\n\n`" + msg.UserID + "`\n\n这是本条消息发送者的 UserID。请将它填入本机 IM Agent 的用户白名单；首次配置时，管理员填写同一个值。此命令只识别身份，不执行模型任务，也不会自动授权。"
	op, stop := context.WithTimeout(ctx, 20*time.Second)
	_, err := c.SendText(op, msg.Address, text, "")
	stop()
	if err == nil {
		return nil
	}
	var uncertain *im.DeliveryUncertain
	if errors.As(err, &uncertain) {
		c.setStatus("connected", "钉钉身份识别回复结果未确认，请核对群消息；未自动重发")
		return nil
	}
	c.identityMu.Lock()
	delete(c.identityReplies, key)
	c.identityMu.Unlock()
	c.setStatus("connected", "已收到身份识别命令，但发送失败；请检查机器人消息权限及应用发布状态")
	return err
}
