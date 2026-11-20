package bridge

import (
	"context"
	"errors"
	"sort"
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
	var err error
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
		out.People, err = b.assistantSearchPeople(ctx, r.Query)
		return out, err
	case "search_groups", "recent_groups":
		out.Groups, err = b.assistantSearchGroups(ctx, r.Query)
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

type cachedPeople struct {
	at     time.Time
	people []im.Person
}

// assistantSearchPeople 先查本机已拉取消息里出现过的发送人（即时），再查通讯录；通讯录结果按关键词缓存 30 分钟。
func (b *Bridge) assistantSearchPeople(ctx context.Context, query string) ([]im.Person, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("请输入姓名关键词")
	}
	seen := map[string]bool{}
	out := []im.Person{}
	add := func(list []im.Person) {
		for _, p := range list {
			if !seen[p.ID] {
				seen[p.ID] = true
				out = append(out, p)
			}
		}
	}
	local, _ := b.store.AssistantSenders(query, 20)
	add(local)
	state := b.assistant()
	state.mu.Lock()
	hit, ok := state.people[query]
	state.mu.Unlock()
	if ok && time.Since(hit.at) < 30*time.Minute {
		add(hit.people)
		return out, nil
	}
	src, err := b.assistantSourceFromSettings()
	if err != nil {
		return out, err
	}
	remote, err := src.SearchPeople(ctx, query)
	if err != nil {
		if len(out) > 0 {
			return out, nil // 本机已有匹配，通讯录暂不可用时不报错。
		}
		return nil, err
	}
	state.mu.Lock()
	if len(state.people) > 200 {
		state.people = map[string]cachedPeople{}
	}
	state.people[query] = cachedPeople{time.Now(), remote}
	state.mu.Unlock()
	add(remote)
	return out, nil
}

// assistantSearchGroups 读取后台每 5 分钟刷新的全部群目录缓存，本地按名称过滤，
// 今天活跃的群排在前面；目录尚未加载完时退回今日活跃会话（缓存 1 分钟）。
func (b *Bridge) assistantSearchGroups(ctx context.Context, query string) ([]im.RecentConversation, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	src, err := b.assistantSourceFromSettings()
	if err != nil {
		return nil, err
	}
	recent := b.assistantRecentCached(ctx, src)
	active := map[string]int64{}
	for _, c := range recent {
		active[c.ID] = c.LatestMs
	}
	var all []im.RecentConversation
	if dir, ok := src.(im.GroupDirectoryAdapter); ok {
		if groups, e := dir.CategoryGroups(ctx, "all"); e == nil {
			for _, g := range groups {
				all = append(all, im.RecentConversation{ID: g.ID, Name: g.Name, Kind: "group", LatestMs: max(active[g.ID], g.LastMessageAt)})
			}
		}
	}
	if len(all) == 0 {
		for _, c := range recent {
			if c.Kind == "group" {
				all = append(all, c)
			}
		}
	}
	out := []im.RecentConversation{}
	for _, g := range all {
		if query == "" && active[g.ID] == 0 {
			continue // 不输入关键词时只列今天活跃的群
		}
		if query == "" || strings.Contains(strings.ToLower(g.Name), query) {
			out = append(out, g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LatestMs > out[j].LatestMs })
	if len(out) > 50 {
		out = out[:50]
	}
	return out, nil
}

func (b *Bridge) assistantRecentCached(ctx context.Context, src im.AssistantSource) []im.RecentConversation {
	state := b.assistant()
	state.mu.Lock()
	list, at := state.recent, state.recentAt
	state.mu.Unlock()
	if time.Since(at) < time.Minute {
		return list
	}
	_, dayStart := assistantDay(nowFunc())
	fresh, err := src.RecentConversations(ctx, dayStart)
	if err != nil {
		return list // 读取失败时继续使用旧快照
	}
	state.mu.Lock()
	state.recent, state.recentAt = fresh, time.Now()
	state.mu.Unlock()
	return fresh
}
