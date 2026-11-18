package bridge

import (
	"context"
	"errors"
	"im-agent/internal/im"
)

type ForwardCategory struct {
	Connection string `json:"connection"`
	ID         string `json:"id"`
	Name       string `json:"name"`
}

func (b *Bridge) forwardCategories(ctx context.Context) ([]ForwardCategory, error) {
	out := []ForwardCategory{}
	for _, conn := range b.cfg.Connections {
		if conn.Type != "dingtalk_personal" || !conn.PersonalMentions {
			continue
		}
		adapter, ok := b.channels[conn.ID].(im.GroupDirectoryAdapter)
		if !ok {
			continue
		}
		categories, err := adapter.GroupCategories(ctx)
		if err != nil {
			return nil, err
		}
		for _, cat := range categories {
			out = append(out, ForwardCategory{conn.ID, cat.ID, cat.Name})
		}
	}
	return out, nil
}
func (b *Bridge) forwardCategoryGroups(ctx context.Context, connection, category string) ([]ForwardGroup, error) {
	cfg := b.conn(connection)
	adapter, ok := b.channels[connection].(im.GroupDirectoryAdapter)
	if cfg.Type != "dingtalk_personal" || !cfg.PersonalMentions || !ok {
		return nil, errors.New("钉钉账号目录不可用")
	}
	groups, err := adapter.CategoryGroups(ctx, category)
	if err != nil {
		return nil, err
	}
	out := []ForwardGroup{}
	for _, g := range groups {
		out = append(out, ForwardGroup{Connection: connection, ID: g.ID, Name: g.Name, Members: g.Members, LastMessageAt: g.LastMessageAt, TimeState: g.TimeState, Pinned: g.Pinned})
	}
	return out, nil
}
