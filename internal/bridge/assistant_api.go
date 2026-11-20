package bridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"im-agent/internal/im"
	"im-agent/internal/llm"
	"im-agent/internal/store"
)

type AssistantRequest struct {
	Action   string                      `json:"action"`
	ID       string                      `json:"id,omitempty"`
	Text     string                      `json:"text,omitempty"`
	Category string                      `json:"category,omitempty"`
	Query    string                      `json:"query,omitempty"`
	Thread   string                      `json:"thread,omitempty"`
	Consume  bool                        `json:"consume,omitempty"`
	Settings *store.AssistantSettings    `json:"settings,omitempty"`
	Review   *store.PersonalReviewAction `json:"review,omitempty"`
}

type AssistantResponse struct {
	View     *store.AssistantView     `json:"view,omitempty"`
	People   []im.Person              `json:"people,omitempty"`
	Groups   []im.RecentConversation  `json:"groups,omitempty"`
	Queue    []store.AssistantQueued  `json:"queue,omitempty"`
	Item     *store.AssistantItem     `json:"item,omitempty"`
	Settings *store.AssistantSettings `json:"settings,omitempty"`
}

func (b *Bridge) Assistant(ctx context.Context, r AssistantRequest) (AssistantResponse, error) {
	var out AssistantResponse
	switch r.Action {
	case "view":
		v, err := b.assistantView()
		out.View = &v
		return out, err
	case "save":
		if r.Settings == nil {
			return out, errors.New("缺少设置")
		}
		if err := b.store.SaveAssistantSettings(*r.Settings); err != nil {
			return out, err
		}
		b.RunAssistantNow()
		v, err := b.assistantView()
		out.View = &v
		return out, err
	case "run":
		b.RunAssistantNow()
		return out, nil
	case "search_people":
		src, err := b.assistantSourceFromSettings()
		if err != nil {
			return out, err
		}
		out.People, err = src.SearchPeople(ctx, r.Query)
		return out, err
	case "recent_groups":
		src, err := b.assistantSourceFromSettings()
		if err != nil {
			return out, err
		}
		_, dayStart := assistantDay(nowFunc())
		list, err := src.RecentConversations(ctx, dayStart)
		for _, c := range list {
			if c.Kind == "group" {
				out.Groups = append(out.Groups, c)
			}
		}
		return out, err
	case "item":
		x, err := b.store.AssistantItem(r.ID)
		out.Item = &x
		return out, err
	case "done", "ignored", "open":
		return out, b.store.SetAssistantItemState(r.ID, r.Action)
	case "recategorize":
		return out, b.store.RecategorizeAssistantItem(r.ID, r.Category)
	case "reply":
		src, err := b.assistantSourceFromSettings()
		if err != nil {
			return out, err
		}
		x, err := b.store.AssistantItem(r.ID)
		if err != nil {
			return out, err
		}
		return out, b.sendAssistantReply(ctx, src, x, r.Text, false)
	case "followup":
		src, err := b.assistantSourceFromSettings()
		if err != nil {
			return out, err
		}
		x, err := b.store.AssistantItem(r.ID)
		if err != nil {
			return out, err
		}
		if !x.Awaiting {
			return out, errors.New("该事项不在等待回复")
		}
		return out, b.sendAssistantReply(ctx, src, x, r.Text, true)
	case "await", "unawait":
		return out, b.store.SetAssistantAwaiting(r.ID, r.Action == "await", "")
	case "reply_reset":
		return out, b.store.ResetAssistantReply(r.ID)
	case "session":
		src, err := b.assistantSourceFromSettings()
		if err != nil {
			return out, err
		}
		x, err := b.store.AssistantItem(r.ID)
		if err != nil {
			return out, err
		}
		if x.ThreadID != "" {
			return out, errors.New("该事项已有会话")
		}
		_, dayStart := assistantDay(nowFunc())
		b.allowAssistantTargets(src, b.mustSettings(), dayStart)
		thread, err := b.startAssistantSession(ctx, src, x)
		if err != nil {
			_ = b.store.LinkAssistantSession(x.ID, "", err.Error())
			return out, err
		}
		return out, b.store.LinkAssistantSession(x.ID, thread, "")
	case "queue":
		if strings.TrimSpace(r.Thread) == "" {
			return out, errors.New("缺少会话 ID")
		}
		q, err := b.store.AssistantQueue(ctx, r.Thread, r.Consume)
		out.Queue = q
		return out, err
	case "review":
		if r.Review == nil {
			return out, errors.New("缺少审核操作")
		}
		return out, b.ReviewPersonalReply(ctx, *r.Review)
	}
	return out, errors.New("不支持的消息助理操作")
}

func (b *Bridge) mustSettings() store.AssistantSettings {
	s, err := b.store.AssistantSettings()
	if err != nil {
		return store.DefaultAssistantSettings()
	}
	return s
}

func (b *Bridge) assistantSourceFromSettings() (im.AssistantSource, error) {
	return b.assistantSource(b.mustSettings().Connection)
}

func (b *Bridge) assistantView() (store.AssistantView, error) {
	day, dayStart := assistantDay(nowFunc())
	settings, err := b.store.AssistantSettings()
	if err != nil {
		return store.AssistantView{}, err
	}
	items, err := b.store.AssistantItems(day)
	if err != nil {
		return store.AssistantView{}, err
	}
	cursors, _ := b.store.AssistantCursors(dayStart)
	reviews, _ := b.store.PendingPersonalReviewList(50)
	state := b.assistant()
	state.mu.Lock()
	status, last := state.status, state.lastRun
	if state.running {
		status = "运行中：" + status
	}
	state.mu.Unlock()
	hours := settings.AwaitHours
	if hours <= 0 {
		hours = 4
	}
	limit := nowFunc().Add(-time.Duration(hours) * time.Hour).UnixMilli()
	for i := range items {
		items[i].Overdue = items[i].Awaiting && items[i].AwaitingSince > 0 && items[i].AwaitingSince < limit
	}
	v := store.AssistantView{Day: day, Items: items, Cursors: cursors, Pending: b.store.PendingAssistantCount(dayStart), Reviews: reviews, Settings: settings, Status: status, LastRun: last}
	for _, m := range llm.Models(b.cfg.Codex.Home) {
		v.Models = append(v.Models, m.ID)
	}
	return v, nil
}
