package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"im-agent/internal/im"
)

func (c *Client) CreateTopicGroup(ctx context.Context, v im.Conversation) (im.Address, error) {
	return c.createConversation(ctx, v, "thread")
}
func (c *Client) ValidateTopicGroup(ctx context.Context, a im.Address) error {
	if e := c.checkAddress(a); e != nil {
		return e
	}
	if a.IsTopic() {
		return errors.New("共享群不能填写话题地址")
	}
	raw, err := c.request(ctx, "GET", "/im/v1/chats/"+url.PathEscape(a.ChatID), nil)
	if err != nil {
		return err
	}
	var v struct {
		Mode   string `json:"chat_mode"`
		Status string `json:"chat_status"`
	}
	if json.Unmarshal(raw, &v) != nil || (v.Mode != "group" && v.Mode != "topic") || v.Status != "normal" {
		return errors.New("共享群不存在、已关闭或不是可用群聊")
	}
	return nil
}

func topicCard(v im.Topic) im.Card {
	state := "可继续对话"
	if v.Status == "archived" {
		state = "已归档，不再执行新消息"
	}
	// 元数据只能作为文字展示，不能通过路径/名称注入卡片 Markdown。
	plain := func(s string) string {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "'", "[", "［", "]", "］", "*", "＊", "_", "＿").Replace(s)
	}
	return im.Card{Title: v.Name, Text: "状态：" + state + "\n工作目录：" + plain(v.WorkDir) + "\nCodex 会话：" + plain(v.SessionID) + "\n\n请在本话题内继续发消息。归档仅关闭此会话，不解散共享群。"}
}

// CreateTopic 不假造 thread_id；只有平台返回话题 ID 后才交给核心建立映射。
// 返回非空部分地址意味着根消息可能已送达，调用者必须保留意图且不得自动重建。
func (c *Client) CreateTopic(ctx context.Context, parent im.Address, topic im.Topic) (im.Address, error) {
	if err := c.ValidateTopicGroup(ctx, parent); err != nil {
		return im.Address{}, err
	}
	key := sha256.Sum256([]byte(parent.Key() + "\x00" + topic.IdempotencyKey + "\x00topic-root"))
	body := map[string]any{"receive_id": parent.ChatID, "msg_type": "interactive", "content": cardPayload(topicCard(topic)), "uuid": hex.EncodeToString(key[:16])}
	token, err := c.accessToken(ctx)
	if err != nil {
		return im.Address{}, err
	}
	result, err := c.do(ctx, "POST", "/im/v1/messages?receive_id_type=chat_id", token, body)
	if err != nil {
		var platform *apiError
		if errors.As(err, &platform) && platform.code != 0 {
			return im.Address{}, err
		}
		return im.Address{}, &im.DeliveryUncertain{Cause: err}
	}
	var root struct {
		ID      string `json:"message_id"`
		TopicID string `json:"thread_id"`
	}
	if json.Unmarshal(result.Data, &root) != nil || root.ID == "" {
		return im.Address{}, &im.DeliveryUncertain{Cause: errors.New("话题根消息未返回 ID")}
	}
	a := parent
	a.RootMessageID = root.ID
	a.TopicID = root.TopicID
	if a.TopicID != "" {
		return a, nil
	}
	// 普通群的根消息可能还不是话题。显式回复一次建立话题，保留首卡为根。
	sum := sha256.Sum256([]byte(parent.Key() + "\x00" + topic.IdempotencyKey + "\x00topic-open"))
	raw, err := c.request(ctx, "POST", "/im/v1/messages/"+url.PathEscape(root.ID)+"/reply", map[string]any{
		"msg_type": "text", "content": marshal(map[string]string{"text": "会话话题已开启，请在这里继续。"}), "reply_in_thread": true, "uuid": hex.EncodeToString(sum[:16]),
	})
	if err != nil {
		return a, &im.DeliveryUncertain{Cause: err}
	}
	var reply struct {
		TopicID string `json:"thread_id"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.TopicID == "" {
		return a, &im.DeliveryUncertain{Cause: errors.New("平台未返回话题 ID，请人工检查根消息，不自动重复创建")}
	}
	a.TopicID = reply.TopicID
	return a, nil
}
func (c *Client) UpdateTopic(ctx context.Context, a im.Address, topic im.Topic) error {
	if err := c.checkAddress(a); err != nil {
		return err
	}
	if !a.IsTopic() || a.RootMessageID == "" {
		return errors.New("缺少话题根消息，不更新群聊")
	}
	return c.UpdateCard(ctx, a, a.RootMessageID, topicCard(topic))
}
