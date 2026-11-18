package bridge

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
)

func (b *Bridge) respondByText(ctx context.Context, adapter im.Adapter, msg im.Message, parts []string) error {
	if msg.Address.Channel != "dingtalk" {
		return diagnostics.UserError("请使用当前渠道的交互卡片")
	}
	if len(parts) != 3 {
		return diagnostics.UserError("格式：/respond 请求ID 选项值，请复制确认消息中的命令")
	}
	a, err := b.store.Action(parts[1])
	if err != nil {
		return diagnostics.UserError("交互请求不存在或已失效")
	}
	// MessageID 只从已保存的原回执读取；receiveInteraction 再次验证地址、用户、状态和选项。
	if err = b.receiveInteraction(ctx, im.Interaction{Address: msg.Address, UserID: msg.UserID, MessageID: a.MessageID, ActionID: parts[1], Choice: parts[2]}); err != nil {
		return err
	}
	_, err = adapter.SendText(ctx, msg.Address, "已接收你的选择。", msg.ID+":respond")
	return err
}
func (b *Bridge) stopByText(ctx context.Context, adapter im.Adapter, msg im.Message) error {
	if msg.Address.Channel != "dingtalk" {
		return diagnostics.UserError("请使用当前渠道的中断控制卡片")
	}
	s, err := b.store.ByChat(msg.Address)
	if err != nil {
		return diagnostics.UserError("当前会话没有可中断的任务")
	}
	if msg.UserID != s.Owner && !config.Contains(b.conn(msg.Address.Connection).AdminUsers, msg.UserID) {
		return diagnostics.UserError("只有会话创建者或管理员可以中断")
	}
	b.mu.Lock()
	active := b.active[s.ThreadID]
	if active != nil {
		active.cancel()
	}
	b.mu.Unlock()
	text := "当前没有正在执行的桥接任务。"
	if active != nil {
		text = "已请求中断当前任务，等待 Codex 确认。"
	}
	_, err = adapter.SendText(ctx, msg.Address, text, msg.ID+":stop")
	return err
}
func (b *Bridge) dingtalkNew(ctx context.Context, adapter im.Adapter, msg im.Message) error {
	if !config.Contains(b.conn(msg.Address.Connection).AdminUsers, msg.UserID) {
		return diagnostics.UserError("只有连接管理员可以显式创建会话")
	}
	if _, err := b.store.ByChat(msg.Address); err == nil {
		return diagnostics.UserError("当前入口已绑定会话；直接发送文字即可继续。需要独立会话请使用另一个钉钉群")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if strings.TrimSpace(strings.TrimPrefix(msg.Text, "/new")) != "" {
		return diagnostics.UserError("请先发送 /new 创建会话，再发送问题")
	}
	_, err := b.getOrCreateSession(ctx, adapter, msg)
	return err
}
