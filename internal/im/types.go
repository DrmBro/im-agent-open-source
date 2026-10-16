// Package im 定义渠道无关的消息、会话、交互与能力边界。
package im

import (
	"context"
	"io"
)

type Address struct {
	Channel       string `json:"channel"`
	Connection    string `json:"connection"`
	ChatID        string `json:"chat_id"`
	TopicID       string `json:"topic_id,omitempty"`
	RootMessageID string `json:"root_message_id,omitempty"`
}

// 空话题保持旧群级键不变，避免旧队列/送达键失效。
func (a Address) BaseKey() string { return a.Channel + "\x00" + a.Connection + "\x00" + a.ChatID }
func (a Address) Key() string {
	if a.TopicID == "" {
		return a.BaseKey()
	}
	return a.BaseKey() + "\x00" + a.TopicID
}
func (a Address) IsTopic() bool   { return a.TopicID != "" }
func (a Address) Parent() Address { a.TopicID = ""; a.RootMessageID = ""; return a }

type Message struct {
	Attachments      []Attachment
	Address          Address
	ID, UserID, Text string
	IsDirect         bool
}
type Interaction struct {
	Fields                              map[string]string `json:"fields,omitempty"`
	Address                             Address
	MessageID, UserID, ActionID, Choice string
}
type Button struct {
	Label       string
	Value       string
	Destructive bool
}
type Card struct {
	Form                  *Form `json:"form,omitempty"`
	Title, Text, ActionID string
	Buttons               []Button
	Selection             *Selection
}
type Selection struct {
	Placeholder string
	Options     []Button
}
type Capabilities struct{ CreateConversation, UpdateCard, InteractiveCards, StreamCards, CloseConversation, RenameConversation, Topics, SelectMenus bool }
type Conversation struct {
	Name           string
	Members        []string
	Owner          string
	IdempotencyKey string
}
type Adapter interface {
	Identity() Address
	Capabilities() Capabilities
	Start(context.Context, func(context.Context, Message) error, func(context.Context, Interaction) error) error
	CreateConversation(context.Context, Conversation) (Address, error)
	SendText(context.Context, Address, string, string) (string, error)
	SendCard(context.Context, Address, Card, string) (string, error)
	UpdateCard(context.Context, Address, string, Card) error
}

// ConversationClosed 是平台确认的整个会话关闭，不包括隐藏、退群或机器人被踢。
type ConversationClosed struct {
	Address          Address
	EventID, ActorID string
}
type ConversationState string

const (
	ConversationOpen    ConversationState = "open"
	ConversationGone    ConversationState = "closed"
	ConversationUnknown ConversationState = "unknown"
)

// LifecycleAdapter 为可选能力；未实现时保留原群并显示待同步，不能假装删除成功。
type LifecycleAdapter interface {
	SetClosedHandler(func(context.Context, ConversationClosed) error)
	CloseConversation(context.Context, Address) error
	ConversationState(context.Context, Address) (ConversationState, error)
}

// ConnectionStatus 是可选的运行诊断，不携带凭证、用户身份或消息正文。
type ConnectionStatus struct {
	Connection string `json:"connection"`
	Phase      string `json:"phase"`
	Message    string `json:"message"`
	Received   int64  `json:"received"`
	Rejected   int64  `json:"rejected"`
	UpdatedAt  int64  `json:"updated_at"`
}
type StatusProvider interface{ ConnectionStatus() ConnectionStatus }

// NamingAdapter 是可选能力，改名必须保留原绑定会话，不创建替代群。
type NamingAdapter interface {
	RenameConversation(context.Context, Address, string) error
}

// DeliveryUncertain 表示消息可能已送达，禁止在去重有效期未知时自动重复发送。
type DeliveryUncertain struct{ Cause error }

func (e *DeliveryUncertain) Error() string {
	return "消息送达结果不确定，请检查 IM 群内是否已经收到"
}
func (e *DeliveryUncertain) Unwrap() error { return e.Cause }

// TopicAdapter 是可选能力；话题归档仅更新展示，永远不关闭承载它的群。
type TopicAdapter interface {
	CreateTopicGroup(context.Context, Conversation) (Address, error)
	ValidateTopicGroup(context.Context, Address) error
	CreateTopic(context.Context, Address, Topic) (Address, error)
	UpdateTopic(context.Context, Address, Topic) error
}
type Topic struct {
	Name, WorkDir, SessionID, Status, IdempotencyKey string
}

// IndependentConversationValidator 可选能力：确认可复用目标为独立普通群。
type IndependentConversationValidator interface {
	ValidateIndependentConversation(context.Context, Address) error
}

// Attachment 仅包含平台资源引用；客户端发送前必须按消息来源授权下载。
type Attachment struct {
	SourceMessageID string `json:"source_message_id,omitempty"`
	SourceIndex     int    `json:"source_index,omitempty"`
	Kind            string `json:"kind"`
	Key             string `json:"key"`
	Name            string `json:"name"`
}

// FileAdapter 为渠道可选能力。下载只针对已授权消息；读取受上限限制。
type FileAdapter interface {
	DownloadAttachment(context.Context, string, Attachment) (io.ReadCloser, error)
	SendAttachment(context.Context, Address, string, string, io.Reader, string) (string, error)
}

// Form 描述渠道无关的创建输入，字段值必须在服务端按快照重新校验。
type Form struct {
	Fields      []FormField
	SubmitLabel string
}
type FormField struct {
	Name, Label, Placeholder, Default string
	Options                           []Button
	Multiline, Required               bool
	MaxLength                         int
}

// MarkdownAdapter 是可选的内容展示能力；不支持时使用已有纯文本投递。
// 失败不能自动换格式重发：原消息可能已送达，仍须遵守渠道的回执规则。
type MarkdownAdapter interface {
	SendMarkdown(context.Context, Address, string, string) (string, error)
}

// GuardedFileAdapter 在上传完成、实际发送群消息前重新核对授权。
// 报告等受会话生命周期约束的文件必须使用此接口，不向普通SendAttachment降级。
type GuardedFileAdapter interface {
	SendAttachmentGuarded(context.Context, Address, string, string, io.Reader, string, func() error) (string, error)
}

// InboundHistoryAdapter 对已绑定群做可分页只读对账，不直接执行模型任务。
type InboundHistoryAdapter interface {
	ReadInboundHistory(context.Context, Address, int64, int64, string) (InboundHistoryPage, error)
}
type InboundHistoryRecord struct {
	ID         string
	CreatedMS  int64
	Address    Address
	Message    *Message
	SkipReason string
}
type InboundHistoryPage struct {
	Records   []InboundHistoryRecord
	NextToken string
	HasMore   bool
}

// AttachmentReadError 只描述资源只读下载的固定类别；可恢复不等于可重发模型输入。
type AttachmentReadError struct {
	Category  string
	Retryable bool
	Cause     error
}

func (e *AttachmentReadError) Error() string { return e.Cause.Error() }
func (e *AttachmentReadError) Unwrap() error { return e.Cause }
