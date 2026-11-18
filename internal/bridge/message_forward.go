package bridge

import (
	"context"
	"errors"
	"im-agent/internal/codex"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type MessageForwardRequest struct {
	Category    string `json:"category"`
	Action      string `json:"action"`
	Thread      string `json:"thread"`
	Before      string `json:"before"`
	Message     string `json:"message"`
	Fingerprint string `json:"fingerprint"`
	Connection  string `json:"connection"`
	Group       string `json:"group"`
	Text        string `json:"text"`
	Confirmed   bool   `json:"confirmed"`
}
type ForwardChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type ForwardGroup struct {
	LastMessageAt int64  `json:"last_message_at,omitempty"`
	TimeState     string `json:"time_state,omitempty"`
	Pinned        bool   `json:"pinned,omitempty"`
	Members       int    `json:"members,omitempty"`
	Connection    string `json:"connection"`
	ID            string `json:"id"`
	Name          string `json:"name"`
}
type ForwardMessage struct {
	codex.HistoryMessage
	Fingerprint string `json:"fingerprint"`
	Truncated   bool   `json:"truncated"`
}
type MessageForwardView struct {
	ImageCount  int                          `json:"image_count,omitempty"`
	Categories  []ForwardCategory            `json:"categories,omitempty"`
	Threads     []ForwardChoice              `json:"threads,omitempty"`
	Connections []ForwardChoice              `json:"connections,omitempty"`
	Groups      []ForwardGroup               `json:"groups,omitempty"`
	Messages    []ForwardMessage             `json:"messages,omitempty"`
	More        bool                         `json:"more"`
	Records     []store.MessageForwardRecord `json:"records,omitempty"`
	ID          string                       `json:"id,omitempty"`
	State       string                       `json:"state,omitempty"`
}

func (b *Bridge) MessageForward(ctx context.Context, r MessageForwardRequest) (MessageForwardView, error) {
	var out MessageForwardView
	switch r.Action {
	case "categories":
		var err error
		out.Categories, err = b.forwardCategories(ctx)
		return out, err
	case "category_groups":
		var err error
		out.Groups, err = b.forwardCategoryGroups(ctx, r.Connection, r.Category)
		return out, err
	case "catalog":
		threads, err := b.codex.List(ctx, "")
		if err != nil {
			return out, err
		}
		archived, err := b.codex.Archived(ctx)
		if err != nil {
			return out, err
		}
		seen := map[string]bool{}
		for _, t := range append(threads, archived...) {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			name := t.Name
			if name == "" {
				name = t.Preview
			}
			if name == "" {
				name = t.ID
			}
			if runes := []rune(name); len(runes) > 160 {
				name = string(runes[:160]) + "…"
			}
			out.Threads = append(out.Threads, ForwardChoice{t.ID, name})
		}
		out.Connections, out.Groups, err = b.forwardTargets()
		if err != nil {
			return out, err
		}
		out.Records, err = b.store.MessageForwards(ctx)
		return out, err
	case "records":
		var err error
		out.Records, err = b.store.MessageForwards(ctx)
		return out, err
	case "messages", "send", "preview":
		if r.Thread == "" || len(r.Thread) > 128 || strings.ContainsAny(r.Thread, "/\\\x00\r\n") {
			return out, diagnostics.UserError("请选择有效的 Codex 会话")
		}
	default:
		return out, diagnostics.UserError("投递操作无效")
	}
	messages, more, err := b.engineForThread(r.Thread).ForwardMessages(ctx, r.Thread, r.Before)
	if err != nil {
		return out, err
	}
	if r.Action == "messages" {
		out.More = more
		for _, m := range messages {
			fingerprint := store.Hash(m.Text)
			truncated := len(m.Text) > 24000
			if truncated {
				m.Text = m.Text[:24000]
				for !utf8.ValidString(m.Text) {
					m.Text = m.Text[:len(m.Text)-1]
				}
			}
			out.Messages = append(out.Messages, ForwardMessage{m, fingerprint, truncated})
		}
		return out, nil
	}
	if r.Action == "send" && !r.Confirmed {
		return out, diagnostics.UserError("请预览并确认发送")
	}
	valid := false
	original := ""
	for _, m := range messages {
		if m.ID == r.Message && store.Hash(m.Text) == r.Fingerprint {
			valid = true
			original = m.Text
			break
		}
	}
	if !valid {
		return out, diagnostics.UserError("来源消息已变化或不属于当前页，请重新选择")
	}
	if strings.TrimSpace(r.Text) == "" || len(r.Text) > 24000 {
		return out, diagnostics.UserError("投递正文不能为空，且不能超过24000字节")
	}
	if r.Group == "" || len(r.Group) > 256 || strings.HasPrefix(r.Group, "-") || strings.ContainsAny(r.Group, "@, \t\r\n\x00") {
		return out, diagnostics.UserError("钉钉群 ID 无效")
	}
	cfg := b.conn(r.Connection)
	adapter := b.channels[r.Connection]
	if cfg.Type != "dingtalk_personal" || !cfg.PersonalMentions || adapter == nil {
		return out, diagnostics.UserError("请选择已开启群消息的钉钉个人账号")
	}
	profile, err := cfg.Credential("profile")
	if err != nil {
		return out, err
	}
	target := im.Address{Channel: cfg.Type, Connection: r.Connection, ChatID: "account:" + profile + ":group:" + r.Group}
	body, imageDir, imageProof, imageCount, cleanup, err := b.prepareForwardImages(ctx, r.Thread, original, r.Text)
	if err != nil {
		return out, err
	}
	defer cleanup()
	if r.Action == "preview" {
		out.State = "ready"
		out.ImageCount = imageCount
		return out, nil
	}
	format := "markdown-v1"
	if imageProof != "" {
		format = "image-markdown-v1"
	}
	id := "forward:" + store.Hash(format+"\x00"+r.Thread+"\x00"+r.Message+"\x00"+target.Key()+"\x00"+r.Text+"\x00"+imageProof)
	// 没有图片时保留原幂等身份，避免升级后重复投递同一正文。
	if imageProof == "" {
		id = "forward:" + store.Hash("markdown-v1\x00"+r.Thread+"\x00"+r.Message+"\x00"+target.Key()+"\x00"+r.Text)
	}
	parts := b.personalParts(body)
	sender := adapter.SendText
	if images, ok := adapter.(im.ImageMarkdownAdapter); ok {
		sender = func(ctx context.Context, a im.Address, text, key string) (string, error) {
			return images.SendImageMarkdown(ctx, a, text, key, imageDir)
		}
		parts = []string{body}
	} else if imageProof != "" {
		return out, diagnostics.UserError("当前连接不支持图文上传，尚未发送")
	} else if markdown, ok := adapter.(im.MarkdownAdapter); ok {
		sender = markdown.SendMarkdown
		parts = []string{body}
	}
	claimed, err := b.store.ReserveMessageForward(ctx, store.MessageForwardRecord{ID: id, Thread: r.Thread, Message: r.Message, Target: r.Connection + " / " + r.Group, Total: len(parts)}, r.Text)
	if err != nil {
		return out, err
	}
	out.ID = id
	if !claimed {
		out.State = "unknown"
		record, err := b.store.MessageForwardRecord(ctx, id)
		if err != nil {
			return out, err
		}
		out.State = record.State
		return out, nil
	}
	// 请求取消、网络不确定或进程重启均不重放。分片逐条保存平台回执。
	finish := func(state string) error {
		op, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return b.store.FinishMessageForward(op, id, state)
	}
	for i, part := range parts {
		if ctx.Err() != nil {
			_ = finish("unknown")
			return out, diagnostics.UserError("投递中断，请先核对记录与目标群")
		}
		platform, err := sender(ctx, target, part, "personal-reply:"+id+":"+strconv.Itoa(i))
		if err != nil {
			var rejected *im.DeliveryRejected
			if errors.As(err, &rejected) {
				_ = finish("failed")
				out.State = "failed"
				return out, diagnostics.UserError("钉钉拒绝图文消息参数，消息未发出；图片可能已上传。不会自动重发。")
			}
			_ = finish("unknown")
			return out, diagnostics.UserError("投递未确认，请核对目标群和投递记录；不会自动重发")
		}
		if err = b.store.ForwardPartSent(ctx, id, i, platform); err != nil {
			_ = finish("unknown")
			return out, diagnostics.UserError("平台已响应但回执保存失败，请核对目标群；不会重发")
		}
	}
	if err = finish("sent"); err != nil {
		return out, err
	}
	out.State = "sent"
	return out, nil
}

func (b *Bridge) forwardTargets() ([]ForwardChoice, []ForwardGroup, error) {
	var out MessageForwardView
	for _, c := range b.cfg.Connections {
		if c.Type == "dingtalk_personal" && c.PersonalMentions {
			out.Connections = append(out.Connections, ForwardChoice{c.ID, "钉钉个人账号 · " + c.ID})
		}
	}
	sessions, err := b.store.LifecycleSessions()
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, s := range sessions {
		if s.Status != "active" || s.Address.Channel != "dingtalk_personal" {
			continue
		}
		wire, err := s.Address.DeliveryAddress()
		if err != nil {
			continue
		}
		_, group, ok := strings.Cut(wire.ChatID, ":group:")
		if !ok || seen[wire.Key()] {
			continue
		}
		seen[wire.Key()] = true
		out.Groups = append(out.Groups, ForwardGroup{Connection: wire.Connection, ID: group, Name: group})
	}

	return out.Connections, out.Groups, nil
}
