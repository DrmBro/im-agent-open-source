package im

import "context"

// 目录与收件授权分离：只提供本人选择投递目标，不扩大自动监听范围。
type GroupCategory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type DirectoryGroup struct {
	LastMessageAt int64  `json:"last_message_at,omitempty"`
	TimeState     string `json:"time_state,omitempty"`
	Pinned        bool   `json:"pinned,omitempty"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	Members       int    `json:"members,omitempty"`
}
type GroupDirectoryAdapter interface {
	GroupCategories(context.Context) ([]GroupCategory, error)
	CategoryGroups(context.Context, string) ([]DirectoryGroup, error)
}
