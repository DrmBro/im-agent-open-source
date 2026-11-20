package im

import "context"

// PulledMessage 是消息助理主动拉取到的一条会话消息；时间为毫秒。
type PulledMessage struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	SenderID       string `json:"sender_id"`
	SenderName     string `json:"sender_name"`
	Text           string `json:"text"`
	Quoted         string `json:"quoted,omitempty"`
	CreatedMs      int64  `json:"created_ms"`
	Self           bool   `json:"self"`
	Robot          bool   `json:"robot"`
}

type Person struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
}

type RecentConversation struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // group 或 direct
	LatestMs int64  `json:"latest_ms"`
}

type Mention struct {
	ConversationID string `json:"conversation_id"`
	Name           string `json:"name"`
	MessageID      string `json:"message_id"`
	CreatedMs      int64  `json:"created_ms"`
}

// AssistantSource 由支持个人账号读取的渠道实现，只读拉取，不改变平台状态。
type AssistantSource interface {
	AssistantSelf() string
	// AllowAssistantTargets 登记主人指定关注的人和群，允许向其发送已确认的回复。
	AllowAssistantTargets(peers, groups []string)
	// AssistantAddress 返回可直接投递的私聊或群地址。
	AssistantAddress(kind, conversation, peer string) (Address, error)
	// PullMessages 读取 since 之后的消息；by 为 conversation 或 peer。
	PullMessages(ctx context.Context, by, id string, sinceMs int64) ([]PulledMessage, error)
	SearchPeople(ctx context.Context, query string) ([]Person, error)
	RecentConversations(ctx context.Context, sinceMs int64) ([]RecentConversation, error)
	MentionsSince(ctx context.Context, sinceMs int64) ([]Mention, error)
}
