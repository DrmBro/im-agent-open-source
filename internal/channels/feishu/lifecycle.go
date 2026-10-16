package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"im-agent/internal/im"
	"net/url"
)

// 仅在启动长连接前装配回调，不在运行中替换。
func (c *Client) SetClosedHandler(h func(context.Context, im.ConversationClosed) error) {
	c.closedHandler = h
}
func (c *Client) checkAddress(a im.Address) error {
	if a.Channel != "feishu" || a.Connection != c.cfg.ID || a.ChatID == "" {
		return errors.New("群聊与当前连接不匹配")
	}
	return nil
}
func (c *Client) ConversationState(ctx context.Context, a im.Address) (im.ConversationState, error) {
	if e := c.checkAddress(a); e != nil {
		return im.ConversationUnknown, e
	}
	raw, e := c.request(ctx, "GET", "/im/v1/chats/"+url.PathEscape(a.ChatID), nil)
	if e != nil {
		return im.ConversationUnknown, e
	}
	var v struct {
		Status string `json:"chat_status"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return im.ConversationUnknown, errors.New("群状态响应无效")
	}
	switch v.Status {
	case "normal":
		return im.ConversationOpen, nil
	case "dissolved":
		return im.ConversationGone, nil
	default:
		return im.ConversationUnknown, errors.New("飞书未返回可识别的群状态")
	}
}
func (c *Client) CloseConversation(ctx context.Context, a im.Address) error {
	if a.IsTopic() || a.RootMessageID != "" || (c.cfg.SharedChatID != "" && a.ChatID == c.cfg.SharedChatID) {
		return errors.New("共享群/话题禁止调用群解散接口")
	}
	if e := c.checkAddress(a); e != nil {
		return e
	}
	_, e := c.request(ctx, "DELETE", "/im/v1/chats/"+url.PathEscape(a.ChatID), nil)
	// 错误（包括无权限、404）不等价于已解散，核心需独立查询或等待解散事件确认。
	return e
}

// RenameConversation 只更新名称，不改变成员、权限或会话映射。
func (c *Client) RenameConversation(ctx context.Context, a im.Address, name string) error {
	if a.IsTopic() || a.RootMessageID != "" {
		return errors.New("话题改名必须更新入口卡片，不能改共享群名称")
	}
	if e := c.checkAddress(a); e != nil {
		return e
	}
	if len([]rune(name)) < 1 || len([]rune(name)) > 55 {
		return errors.New("群名称长度无效")
	}
	_, e := c.request(ctx, "PUT", "/im/v1/chats/"+url.PathEscape(a.ChatID), map[string]string{"name": name})
	return e
}

func (c *Client) ValidateIndependentConversation(ctx context.Context, a im.Address) error {
	if a.IsTopic() || a.RootMessageID != "" {
		return errors.New("不能把话题当作独立群复用")
	}
	if err := c.checkAddress(a); err != nil {
		return err
	}
	raw, err := c.request(ctx, "GET", "/im/v1/chats/"+url.PathEscape(a.ChatID), nil)
	if err != nil {
		return err
	}
	var data struct {
		Mode        string `json:"chat_mode"`
		Status      string `json:"chat_status"`
		MessageType string `json:"group_message_type"`
	}
	if json.Unmarshal(raw, &data) != nil || data.Mode != "group" || data.Status != "normal" || data.MessageType != "chat" {
		return errors.New("目标不是已确认可用的普通会话群，未复用")
	}
	return nil
}
