package dwspersonal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"im-agent/internal/im"
)

// 从原群以收件时间为边界取一页，只接受原消息前的文字和经核对的同群引用。
func (c *Client) MentionContext(ctx context.Context, m im.Message, received int64) ([]string, error) {
	if m.IsDirect || !c.AcceptsMessage(m) || m.Address.ChatID != c.accountPrefix()+"group:"+m.ConversationID || !validID(m.ConversationID) {
		return nil, errors.New("缺少可核对的群地址")
	}
	cutoff := time.Unix(received+1, 0).In(time.FixedZone("Asia/Shanghai", 8*3600))
	raw, err := c.run(ctx, "", "chat", "message", "list", "--conversation-id", m.ConversationID, "--direction", "older", "--time", cutoff.Format("2006-01-02 15:04:05"), "--limit", "16")
	if err != nil {
		return nil, err
	}
	raw, err = successPayload(raw)
	if err != nil {
		return nil, err
	}
	type item struct {
		ID           string          `json:"messageId"`
		Text         string          `json:"text"`
		Chat         string          `json:"openConversationId"`
		Conversation string          `json:"conversationId"`
		Created      json.RawMessage `json:"createTime"`
	}
	type row struct {
		item
		Quoted *item `json:"quotedMessage"`
	}
	var page struct {
		Messages []row `json:"messages"`
		Success  bool  `json:"success"`
	}
	if json.Unmarshal(raw, &page) != nil || !page.Success || page.Messages == nil {
		return nil, errors.New("无法读取群消息上下文")
	}
	matches := func(r item) bool {
		return (r.Chat == "" || r.Chat == m.ConversationID) && (r.Conversation == "" || r.Conversation == m.ConversationID)
	}
	var evidence []string
	seen := map[string]bool{}
	add := func(r item, label string) {
		if r.ID == "" || seen[r.ID] || !matches(r) || strings.TrimSpace(r.Text) == "" {
			return
		}
		seen[r.ID] = true
		chars := []rune(r.Text)
		evidence = append(evidence, label+string(chars[:min(len(chars), 600)]))
	}
	var anchor int64
	for _, r := range page.Messages {
		if r.ID != m.PlatformID || !matches(r.item) {
			continue
		}
		anchor = parseDirectoryTime(r.Created)
		if q := r.Quoted; q != nil && q.ID == m.QuotedID && (m.QuotedConversationID == "" || m.QuotedConversationID == m.ConversationID) {
			add(*q, "原消息引用 · ")
		}
		break
	}
	for _, r := range page.Messages {
		if r.ID == m.PlatformID {
			continue
		}
		if r.ID == m.QuotedID && (m.QuotedConversationID == "" || m.QuotedConversationID == m.ConversationID) {
			add(r.item, "原消息引用 · ")
			continue
		}
		stamp := parseDirectoryTime(r.Created)
		if anchor > 0 && stamp > 0 && stamp < anchor && anchor-stamp <= int64((2*time.Hour)/time.Millisecond) {
			add(r.item, "同群前文 · ")
		}
	}
	return evidence, nil
}
