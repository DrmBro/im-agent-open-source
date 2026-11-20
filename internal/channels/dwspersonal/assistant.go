package dwspersonal

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"im-agent/internal/im"
)

var shanghai = time.FixedZone("Asia/Shanghai", 8*3600)

func dwsTime(ms int64) string { return time.UnixMilli(ms).In(shanghai).Format("2006-01-02 15:04:05") }

func (c *Client) AssistantSelf() string { return c.self }

func (c *Client) AllowAssistantTargets(peers, groups []string) {
	p, g := map[string]bool{}, map[string]bool{}
	for _, x := range peers {
		if validID(x) && x != c.self {
			p[x] = true
		}
	}
	for _, x := range groups {
		if validID(x) {
			g[x] = true
		}
	}
	c.mu.Lock()
	c.assistantPeers, c.assistantGroups = p, g
	c.mu.Unlock()
}
func (c *Client) assistantPeer(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.assistantPeers[id]
}
func (c *Client) assistantGroup(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return id != "" && c.assistantGroups[id]
}

func (c *Client) AssistantAddress(kind, conversation, peer string) (im.Address, error) {
	a := c.Identity()
	switch kind {
	case "direct":
		if !validID(peer) || peer == c.self {
			return im.Address{}, errors.New("单聊联系人标识无效")
		}
		a.ChatID = c.addressPrefix() + peer
	case "group":
		if !validID(conversation) {
			return im.Address{}, errors.New("群会话标识无效")
		}
		a.ChatID = c.accountPrefix() + "group:" + conversation
	default:
		return im.Address{}, errors.New("会话类型无效")
	}
	return a, nil
}

type dwsMessage struct {
	ID           string          `json:"messageId"`
	OpenID       string          `json:"openMessageId"`
	Text         string          `json:"text"`
	Content      string          `json:"content"`
	Created      json.RawMessage `json:"createTime"`
	Sender       string          `json:"sender"`
	SenderNick   string          `json:"senderNick"`
	SenderID     string          `json:"senderOpenDingTalkId"`
	SenderAlt    string          `json:"senderId"`
	SendType     string          `json:"sendType"`
	Conversation string          `json:"conversationId"`
	OpenConv     string          `json:"openConversationId"`
	Resources    []struct {
		Type string `json:"type"`
	} `json:"resourceRefs"`
	Quoted *struct {
		Text    string `json:"text"`
		Content string `json:"content"`
		Sender  string `json:"sender"`
	} `json:"quotedMessage"`
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func (c *Client) PullMessages(ctx context.Context, by, id string, sinceMs int64) ([]im.PulledMessage, error) {
	if !validID(id) {
		return nil, errors.New("会话标识无效")
	}
	flag := "--conversation-id"
	if by == "peer" {
		flag = "--open-dingtalk-id"
	} else if by != "conversation" {
		return nil, errors.New("拉取方式无效")
	}
	raw, err := c.runInDirectory(ctx, "", "", 60*time.Second, "chat", "message", "list", flag, id, "--direction", "newer", "--time", dwsTime(sinceMs), "--limit", "50", "--page-all", "--page-limit", "8", "--page-delay", "0")
	if err != nil {
		return nil, err
	}
	raw, err = successPayload(raw)
	if err != nil {
		return nil, err
	}
	var page struct {
		Success  *bool        `json:"success"`
		Messages []dwsMessage `json:"messages"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Messages == nil || (page.Success != nil && !*page.Success) {
		return nil, errors.New("钉钉未返回有效的消息页")
	}
	out := make([]im.PulledMessage, 0, len(page.Messages))
	seen := map[string]bool{}
	for _, m := range page.Messages {
		mid := m.ID
		if mid == "" {
			mid = m.OpenID
		}
		conv := m.Conversation
		if conv == "" {
			conv = m.OpenConv
		}
		created := parseDirectoryTime(m.Created)
		if mid == "" || conv == "" || created <= 0 || seen[mid] {
			continue
		}
		seen[mid] = true
		sender := m.SenderID
		if sender == "" {
			sender = m.SenderAlt
		}
		name := m.Sender
		if name == "" {
			name = m.SenderNick
		}
		text := m.Text
		if strings.TrimSpace(text) == "" {
			text = m.Content
		}
		if strings.TrimSpace(text) == "" && len(m.Resources) > 0 {
			text = "[图片或文件]"
		}
		p := im.PulledMessage{ConversationID: conv, MessageID: mid, SenderID: sender, SenderName: name, Text: clip(text, 4000), CreatedMs: created, Self: sender != "" && sender == c.self, Robot: m.SendType == "robot"}
		if q := m.Quoted; q != nil {
			qt := q.Text
			if qt == "" {
				qt = q.Content
			}
			if qt != "" {
				p.Quoted = clip(strings.TrimSpace(q.Sender+" "+qt), 600)
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedMs != out[j].CreatedMs {
			return out[i].CreatedMs < out[j].CreatedMs
		}
		return out[i].MessageID < out[j].MessageID
	})
	return out, nil
}

func (c *Client) SearchPeople(ctx context.Context, query string) ([]im.Person, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 40 || strings.HasPrefix(query, "-") {
		return nil, errors.New("请输入姓名关键词")
	}
	raw, err := c.run(ctx, "", "contact", "user", "search", "--query", query)
	if err != nil {
		return nil, err
	}
	raw, err = successPayload(raw)
	if err != nil {
		return nil, err
	}
	var r struct {
		Result []struct {
			Name   string `json:"name"`
			Nick   string `json:"nick"`
			OpenID string `json:"openDingTalkId"`
			Title  string `json:"title"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, errors.New("通讯录搜索结果格式无效")
	}
	out := []im.Person{}
	for _, p := range r.Result {
		if !validID(p.OpenID) {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.Nick
		}
		out = append(out, im.Person{ID: p.OpenID, Name: name, Title: p.Title})
	}
	return out, nil
}

// RecentConversations 读取最近有消息的会话。钉钉分页中途超时时 DWS 返回 partial_failure，
// 已校验的页面仍然可用，因此保留部分结果而不是整体失败。
func (c *Client) RecentConversations(ctx context.Context, sinceMs int64) ([]im.RecentConversation, error) {
	raw, runErr := c.runPartial(ctx, 60*time.Second, "chat", "+recent-conversations", "--start", dwsTime(sinceMs), "--page-delay", "0")
	if runErr != nil && raw == nil {
		return nil, runErr
	}
	type conversation struct {
		ID     string          `json:"conversationId"`
		Name   string          `json:"name"`
		Type   string          `json:"type"`
		Latest json.RawMessage `json:"latestMessageTime"`
	}
	var envelope struct {
		Outcome string `json:"outcome"`
		Data    struct {
			Conversations []conversation `json:"conversations"`
			Succeeded     []struct {
				Conversations []conversation `json:"conversations"`
			} `json:"succeeded"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, errors.New("最近会话列表格式无效")
	}
	list := envelope.Data.Conversations
	for _, page := range envelope.Data.Succeeded {
		list = append(list, page.Conversations...)
	}
	if list == nil && envelope.Outcome != "success" {
		return nil, errors.New("钉钉最近会话暂时读取失败，请稍后重试")
	}
	out := []im.RecentConversation{}
	seen := map[string]bool{}
	for _, x := range list {
		if !validID(x.ID) || seen[x.ID] || (x.Type != "group" && x.Type != "direct") {
			continue
		}
		seen[x.ID] = true
		out = append(out, im.RecentConversation{ID: x.ID, Name: x.Name, Kind: x.Type, LatestMs: parseDirectoryTime(x.Latest)})
	}
	return out, nil
}

func (c *Client) MentionsSince(ctx context.Context, sinceMs int64) ([]im.Mention, error) {
	raw, err := c.run(ctx, "", "chat", "+at-me", "--days", "1", "--limit", "50")
	if err != nil {
		return nil, err
	}
	raw, err = successPayload(raw)
	if err != nil {
		return nil, err
	}
	var r struct {
		Items []struct {
			Conversation   string          `json:"conversation"`
			ConversationID string          `json:"conversationId"`
			MessageID      string          `json:"messageId"`
			Time           json.RawMessage `json:"time"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, errors.New("@我的消息格式无效")
	}
	out := []im.Mention{}
	for _, x := range r.Items {
		at := parseDirectoryTime(x.Time)
		if !validID(x.ConversationID) || at < sinceMs {
			continue
		}
		out = append(out, im.Mention{ConversationID: x.ConversationID, Name: x.Conversation, MessageID: x.MessageID, CreatedMs: at})
	}
	return out, nil
}
