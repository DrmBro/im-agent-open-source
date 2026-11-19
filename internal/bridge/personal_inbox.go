package bridge

import (
	"context"
	"errors"
	"fmt"
	"im-agent/internal/im"
	"im-agent/internal/store"
	"strings"
	"time"
)

type PersonalInboxAction struct {
	Count  int      `json:"count,omitempty"`
	IDs    []string `json:"ids,omitempty"`
	ID     string   `json:"id"`
	Action string   `json:"action"`
}

func (b *Bridge) ReviewPersonalInbox(ctx context.Context, a PersonalInboxAction) error {
	switch a.Action {
	case "start":
		if len(a.IDs) > 0 {
			return b.CreatePersonalInboxSession(ctx, a.IDs...)
		}
		return b.CreatePersonalInboxSession(ctx, a.ID)
	case "regroup":
		return b.regroupInbox(ctx, a.IDs, a.Count)
	case "ignore":
		return b.IgnorePersonalInbox(a.ID)
	default:
		return store.ErrTaskDeliveryUnavailable
	}
}

// 正文、模型建议和引用均取自收件账本，按收件时间还原多条消息的顺序。
func (b *Bridge) inboxSessionInput(ctx context.Context, ids []string) (im.Message, []string, error) {
	var first im.Message
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || wanted[id] {
			return first, nil, errors.New("消息选择无效")
		}
		wanted[id] = true
	}
	if len(ids) == 0 {
		return first, nil, errors.New("请选择消息")
	}
	rows, err := b.store.PersonalInbox(ctx, inboxDayStart())
	if err != nil {
		return first, nil, err
	}
	var prompt strings.Builder
	var ordered []string
	for i := len(rows) - 1; i >= 0; i-- {
		x := rows[i]
		if !wanted[x.ID] {
			continue
		}
		if x.State != "new" {
			return first, nil, errors.New("所选消息已处理，请刷新列表")
		}
		m, err := b.store.InboxMessage(x.ID)
		if err != nil {
			return first, nil, err
		}
		a := b.channels[m.Address.Connection]
		if a == nil || !a.Capabilities().PersonalInbox || !b.messageAllowed(a, m) {
			return first, nil, errors.New("消息连接或接收权限已失效")
		}
		if len(ordered) == 0 {
			first = m
		}
		if m.Address.Key() != first.Address.Key() || m.IsDirect != first.IsDirect || (m.IsDirect && m.UserID != first.UserID) {
			return first, nil, errors.New("同一会话只能处理同一联系人或同一群的消息")
		}
		ordered = append(ordered, x.ID)
		sender := x.UserName
		if sender == "" {
			sender = x.User
		}
		fmt.Fprintf(&prompt, "【%s · %s · 来源收件 ID %s】\n%s\n", time.Unix(x.Received, 0).Format("2006-01-02 15:04"), sender, x.ID, x.Text)
		if a := x.Analysis; a != nil && !a.Failed {
			fmt.Fprintf(&prompt, "分析理由：%s\n", a.Reason)
			if a.CompletedRequest != "" {
				fmt.Fprintf(&prompt, "补全请求：%s\n", a.CompletedRequest)
			}
			for _, e := range a.Evidence {
				fmt.Fprintf(&prompt, "引用依据：%s\n", e)
			}
			if a.Note != "" {
				fmt.Fprintf(&prompt, "待核对：%s\n", a.Note)
			}
		}
		prompt.WriteByte('\n')
	}
	if len(ordered) != len(ids) {
		return first, nil, errors.New("部分所选消息已不在今日收件列表，请刷新")
	}
	first.Text = "请结合以下所选消息的原文和上下文处理其中的请求。模型建议和群聊引文供核对。\n\n" + strings.TrimSpace(prompt.String())
	if len([]rune(first.Text)) > b.cfg.Limits.MaxInputChars {
		return first, nil, errors.New("所选上下文超过输入上限，请减少勾选消息")
	}
	return first, ordered, nil
}
