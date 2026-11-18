package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
	"strings"
	"time"
	"unicode/utf8"
)

const forwardGroupPageSize = 50

type forwardCardForm struct {
	Request      MessageForwardRequest `json:"request"`
	Categories   []ForwardCategory     `json:"categories,omitempty"`
	Targets      []ForwardGroup        `json:"targets,omitempty"`
	Phase        string                `json:"phase,omitempty"`
	CategoryID   string                `json:"category_id,omitempty"`
	Connection   string                `json:"connection,omitempty"`
	Query        string                `json:"query,omitempty"`
	CategoryName string                `json:"category_name,omitempty"`
	Page         int                   `json:"page,omitempty"`
}

func forwardCategoryKey(c ForwardCategory) string { return store.Hash(c.Connection + "\x00" + c.ID) }
func forwardGroupKey(g ForwardGroup) string       { return store.Hash(g.Connection + "\x00" + g.ID) }
func (b *Bridge) openForwardControl(ctx context.Context, s store.Session, event, message string) error {
	if _, err := b.store.OpenSessionControl(s.ThreadID, event, "forward"); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	view, err := b.MessageForward(ctx, MessageForwardRequest{Action: "messages", Thread: s.ThreadID})
	if err != nil {
		return err
	}
	var latest *ForwardMessage
	for i := len(view.Messages) - 1; i >= 0; i-- {
		if view.Messages[i].Role == "assistant" {
			latest = &view.Messages[i]
			break
		}
	}
	if latest == nil {
		return diagnostics.UserError("当前会话尚无可投递的已完成回复")
	}
	if latest.Truncated {
		return diagnostics.UserError("最新回复超过卡片正文上限，请在本机任务看板编辑后投递")
	}
	categories, err := b.forwardCategories(ctx)
	if err != nil {
		return err
	}
	if len(categories) == 0 {
		return diagnostics.UserError("没有可用的钉钉分组，请检查账号登录")
	}
	r := MessageForwardRequest{Action: "send", Thread: s.ThreadID, Message: latest.ID, Fingerprint: latest.Fingerprint, Text: latest.Text, Confirmed: true}
	c := store.SessionControl{ID: store.ID(), ThreadID: s.ThreadID, EventID: event, AddressKey: store.Hash(s.Address.Key()), MessageID: message, Kind: "forward", Payload: store.JSON(forwardCardForm{Request: r, Categories: categories, Phase: "categories"}), ExpiresAt: time.Now().Add(30 * time.Minute).Unix()}
	ok, err := b.store.PutSessionControl(c)
	if err != nil {
		return err
	}
	if !ok {
		return diagnostics.UserError("卡片已变化，请刷新")
	}
	return nil
}
func forwardFormCard(c store.SessionControl, card *im.Card) error {
	var form forwardCardForm
	if json.Unmarshal([]byte(c.Payload), &form) != nil {
		return diagnostics.UserError("投递表单无效")
	}
	card.ActionID = "session-form:" + c.ID
	card.Buttons = nil
	card.Title = "投递最新回复到钉钉群"
	if form.Phase == "categories" {
		options := []im.Button{}
		for _, cat := range form.Categories {
			options = append(options, im.Button{Label: cat.Name + " · " + cat.Connection, Value: forwardCategoryKey(cat)})
		}
		card.Text = "按你在钉钉中的会话分组选择。只展示群聊，不接入分组里的单聊。\n选择分组后可继续选择群，或填写群名关键词筛选；此步不会发送。"
		card.Form = &im.Form{SubmitLabel: "展开分组，选择群", SubmitChoice: "browse", CancelLabel: "取消投递", Fields: []im.FormField{
			{Name: "category", Label: "钉钉会话分组", Required: true, Options: options},
			{Name: "query", Label: "群名关键词（可选）", MaxLength: 100},
		}}
		return nil
	}
	count := len(form.Targets)
	start := form.Page * forwardGroupPageSize
	if start < 0 || start > count {
		return diagnostics.UserError("群列表页码无效")
	}
	end := start + forwardGroupPageSize
	if end > count {
		end = count
	}
	targets := []im.Button{}
	for _, g := range form.Targets[start:end] {
		if g.ID != "" {
			name := g.Name + " · " + g.ID[min(3, len(g.ID)):min(11, len(g.ID))]
			if g.Members > 0 {
				name = fmt.Sprintf("%s · %d人", name, g.Members)
			}
			targets = append(targets, im.Button{Label: name, Value: forwardGroupKey(g)})
		}
	}
	card.Text = "目录：" + form.CategoryName + fmt.Sprintf(" → 群聊（%d个，第%d/%d页）", count, form.Page+1, max(1, (count+forwardGroupPageSize-1)/forwardGroupPageSize)) + "\n已固定打开表单时的最新回复；确认后以钉钉个人账号发送。正文中的图片会上传并内嵌显示，普通文件不上传。\n\n待投递正文：\n" + form.Request.Text
	fields := []im.FormField{}
	if len(targets) > 0 {
		fields = append(fields, im.FormField{Name: "target", Label: "选择群", Required: true, Options: targets}, im.FormField{Name: "text", Label: "替换正文（可选，最多1000字；留空发送上方原文）", Multiline: true, MaxLength: 1000})
	}
	card.Form = &im.Form{SubmitLabel: "确认投递到所选群", CancelLabel: "返回分组", CancelChoice: "back", Fields: fields}
	if len(targets) == 0 {
		card.Text = "目录：" + form.CategoryName + "\n没有符合条件的群聊，请返回分组重新选择或调整关键词。"
		card.Form.SubmitChoice = "back"
		card.Form.SubmitLabel = "返回分组"
	}
	pending, unknown := 0, 0
	for _, g := range form.Targets {
		if g.TimeState == "loading" {
			pending++
		}
		if g.TimeState == "unavailable" {
			unknown++
		}
	}
	if pending > 0 {
		card.Text = fmt.Sprintf("排序更新中：%d个群的最近消息时间读取中，可稍后点击刷新排序。\n", pending) + card.Text
	} else if unknown > 0 {
		card.Text = fmt.Sprintf("%d个群的时间暂不可读，已排在末尾。\n", unknown) + card.Text
	}
	card.Form.Navigation = append(card.Form.Navigation, im.Button{Label: "刷新排序", Value: "refresh_groups"})
	if start > 0 {
		card.Form.Navigation = append(card.Form.Navigation, im.Button{Label: "上一页群", Value: "previous"})
	}
	if end < count {
		card.Form.Navigation = append(card.Form.Navigation, im.Button{Label: "下一页群", Value: "next"})
	}
	return nil
}
func (b *Bridge) navigateForwardForm(ctx context.Context, c store.SessionControl, choice string, fields map[string]string) error {
	var form forwardCardForm
	if json.Unmarshal([]byte(c.Payload), &form) != nil {
		return diagnostics.UserError("投递表单记录无效")
	}
	switch choice {
	case "browse":
		if form.Phase != "categories" {
			return diagnostics.UserError("目录已变化，请刷新卡片")
		}
		var selected *ForwardCategory
		for i := range form.Categories {
			if forwardCategoryKey(form.Categories[i]) == fields["category"] {
				selected = &form.Categories[i]
				break
			}
		}
		if selected == nil {
			return diagnostics.UserError("请选择钉钉分组")
		}
		query := strings.ToLower(strings.TrimSpace(fields["query"]))
		if utf8.RuneCountInString(query) > 100 {
			return diagnostics.UserError("群名关键词过长")
		}
		groups, err := b.forwardCategoryGroups(ctx, selected.Connection, selected.ID)
		if err != nil {
			return err
		}
		form.Targets = nil
		for _, g := range groups {
			if query == "" || strings.Contains(strings.ToLower(g.Name), query) {
				form.Targets = append(form.Targets, g)
			}
		}
		form.Phase = "groups"
		form.Page = 0
		form.CategoryName = selected.Name
		form.CategoryID = selected.ID
		form.Connection = selected.Connection
		form.Query = query
	case "refresh_groups":
		if form.Phase != "groups" || form.CategoryID == "" {
			return diagnostics.UserError("请返回分组重新选择")
		}
		groups, err := b.forwardCategoryGroups(ctx, form.Connection, form.CategoryID)
		if err != nil {
			return err
		}
		form.Targets = nil
		for _, g := range groups {
			if form.Query == "" || strings.Contains(strings.ToLower(g.Name), form.Query) {
				form.Targets = append(form.Targets, g)
			}
		}
		if form.Page*forwardGroupPageSize >= len(form.Targets) {
			form.Page = 0
		}
	case "back":
		cats, err := b.forwardCategories(ctx)
		if err != nil {
			return err
		}
		form.Categories = cats
		form.Phase = "categories"
		form.Targets = nil
		form.Page = 0
	case "next", "previous":
		if form.Phase != "groups" {
			return diagnostics.UserError("请先选择分组")
		}
		if choice == "next" {
			form.Page++
		} else {
			form.Page--
		}
		if form.Page < 0 || form.Page*forwardGroupPageSize >= len(form.Targets) {
			return diagnostics.UserError("群列表已到边界，请刷新卡片")
		}
	default:
		return diagnostics.UserError("目录操作无效")
	}
	return b.store.UpdateSessionControlPayload(c, store.JSON(form))
}
func (b *Bridge) sendForwardForm(ctx context.Context, c store.SessionControl, fields map[string]string) error {
	var form forwardCardForm
	if json.Unmarshal([]byte(c.Payload), &form) != nil || form.Phase == "categories" {
		return diagnostics.UserError("请先选择分组和群")
	}
	if len(fields) > 2 {
		return diagnostics.UserError("投递字段无效")
	}
	var target *ForwardGroup
	for i := range form.Targets {
		if form.Targets[i].ID != "" && forwardGroupKey(form.Targets[i]) == fields["target"] {
			target = &form.Targets[i]
			break
		}
	}
	if target == nil {
		return diagnostics.UserError("群选择已变化，请重新选择")
	}
	r := form.Request
	r.Connection = target.Connection
	r.Group = target.ID
	if text := fields["text"]; strings.TrimSpace(text) != "" {
		if utf8.RuneCountInString(text) > 1000 {
			return diagnostics.UserError("替换正文最多1000字")
		}
		r.Text = text
	}
	if strings.TrimSpace(r.Text) == "" || len(r.Text) > 24000 {
		return diagnostics.UserError("正文为空或超过24000字节")
	}
	// 上传前先走同一准备流程；本机校验失败时保留原表单可重试。
	r.Action = "preview"
	if _, err := b.MessageForward(ctx, r); err != nil {
		return err
	}
	r.Action = "send"
	claimed, err := b.store.ClaimSessionControl(c.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return diagnostics.UserError("该投递已处理，请查看结果")
	}
	result, err := b.MessageForward(ctx, r)
	if err != nil {
		if result.State == "failed" {
			_ = b.store.FinishSessionControl(c.ID, "failed", "钉钉明确拒绝消息参数，消息未发出")
			return err
		}
		if result.ID == "" {
			_ = b.store.ReopenForwardControl(c.ID)
			return err
		}
		_ = b.store.FinishSessionControl(c.ID, "unknown", "投递未确认；请查看本机投递记录")
		return err
	}
	state, detail := "done", "最新回复已投递到钉钉群「"+target.Name+"」"
	if result.State == "failed" {
		state, detail = "failed", "该图文消息此前被钉钉拒绝，未发送；未自动重试"
	} else if result.State != "sent" {
		state, detail = "unknown", "投递结果待核对，请查看钉钉群和本机投递记录；不会重发"
	}
	return b.store.FinishSessionControl(c.ID, state, detail)
}
