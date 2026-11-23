package im

import "context"

type GroupQuote struct {
	Profile   string `json:"profile"`
	Group     string `json:"group"`
	MessageID string `json:"message_id"`
	Recalled  bool   `json:"recalled"`
}

type GroupQuoteAdapter interface {
	CheckGroupQuote(context.Context, Address, string) (GroupQuote, error)
	SendGroupReply(context.Context, Address, string, string, string) (string, error)
}

// 同一连接仅订阅存在近期来源消息的群的撤回事件，不将撤回本身送给模型。
type RecallLedger interface {
	RecentQuoteGroups(context.Context, string, string) ([]string, error)
	RememberRecall(context.Context, Address, string) error
	HasRecall(context.Context, Address, string) (bool, error)
}
