package bridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/im"
	"im-agent/internal/llm"
	"im-agent/internal/store"
)

type AssistantRequest struct {
	Through  int64                       `json:"through,omitempty"`
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
	Text     string                   `json:"text,omitempty"`
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
		_, err = b.startAssistantSession(ctx, src, x)
		if err != nil {
			_ = b.store.LinkAssistantSession(x.ID, "", err.Error())
			return out, err
		}
		return out, nil
	case "supplement", "redispatch":
		b.bindMu.Lock()
		defer b.bindMu.Unlock()
		x, err := b.store.AssistantItem(r.ID)
		if err != nil {
			return out, err
		}
		if x.State != "session" || x.ThreadID == "" || r.Through <= 0 {
			return out, errors.New("请选择已派发事项的待补充消息")
		}
		s, err := b.store.ByThread(x.ThreadID)
		if err != nil {
			return out, err
		}
		recovering := r.Action == "redispatch"
		if recovering {
			if r.Thread == "" || r.Thread != x.ThreadID || s.Status != "archived" || x.Queued == 0 || r.Through > x.QueueThrough {
				return out, errors.New("事项的会话或待补充消息已变化，请刷新")
			}
		} else if s.Status != "active" {
			return out, errors.New("原会话已关闭，请刷新事项后重新派发")
		}
		wire, err := s.Address.DeliveryAddress()
		if err != nil {
			return out, err
		}
		source := "assistant:" + x.ID
		msg := im.Message{ID: "assistant-batch:" + store.Hash(x.ID+":"+fmt.Sprint(r.Through)), Address: wire, UserID: s.Owner, InboxSources: []string{source}, ConversationID: x.Conversation, IsDirect: x.Kind == "direct", ReusedDesktop: b.desktopDispatch(s.ThreadID)}
		if !b.messageAllowed(b.channels[wire.Connection], msg) {
			return out, errors.New("原对话权限已变化")
		}
		if recovering {
			address, err := im.QuestionAddress(wire, store.Hash(source+":"+s.ThreadID))
			if err != nil {
				return out, err
			}
			msg.ReusedDesktop = false
			s, err = b.assistantTargetSession(ctx, x, address, &msg)
			if err != nil {
				return out, err
			}
			msg.Text = "事项摘要：" + clipRunes(x.Summary, 600)
			if len(x.Messages) > 0 {
				first := x.Messages[0]
				msg.Text += "\n最早的关联消息：" + clipRunes(first.Text+" "+first.Quoted, 600)
			}
			msg.Text += "\n" + codex.OwnerAnalysisInstructions
		}
		return out, b.store.QueueAssistantBatch(ctx, x, s, msg, r.Through, b.cfg.Limits.MaxInputChars)
	case "compose":
		if r.Review != nil {
			review, err := b.store.PersonalReview(r.Review.EventID)
			if err != nil {
				return out, err
			}
			out.Text, err = b.composeExternal(ctx, review.Draft)
			return out, err
		}
		x, err := b.store.AssistantItem(r.ID)
		if err != nil {
			return out, err
		}
		out.Text, err = b.composeExternal(ctx, x.Reply)
		return out, err
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
	v.Batches, err = b.store.AssistantBatches(day)
	if err != nil {
		return v, err
	}
	state.mu.Lock()
	v.MatchStats = state.matchStats
	state.mu.Unlock()
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
	query = strings.TrimSpace(query)
	folded := strings.ToLower(query)
	src, err := b.assistantSourceFromSettings()
	if err != nil {
		return nil, err
	}
	recent := b.assistantRecentCached(ctx, src, query == "")
	active := map[string]int64{}
	for _, c := range recent {
		active[c.ID] = c.LatestMs
	}
	var all []im.RecentConversation
	var dirErr error
	if dir, ok := src.(im.GroupDirectoryAdapter); ok {
		groups, e := dir.CategoryGroups(ctx, "all")
		dirErr = e
		if e == nil {
			for _, g := range groups {
				all = append(all, im.RecentConversation{ID: g.ID, Name: g.Name, Kind: "group", LatestMs: max(active[g.ID], g.LastMessageAt)})
			}
		}
	}
	{
		seen := map[string]bool{}
		for _, g := range all {
			seen[g.ID] = true
		}
		for _, c := range recent {
			if c.Kind == "group" && !seen[c.ID] {
				all = append(all, c)
			}
		}
	}
	out := []im.RecentConversation{}
	for _, g := range all {
		if query == "" && active[g.ID] == 0 {
			continue // 不输入关键词时只列今天活跃的群
		}
		if query == "" || strings.Contains(strings.ToLower(g.Name), folded) {
			out = append(out, g)
		}
	}
	// 本机群目录可能尚未加载或读取不全：有关键词时再用钉钉服务端搜索补全，结果按关键词缓存 30 分钟。
	if query != "" {
		remote, err := b.assistantRemoteGroups(ctx, src, query)
		have := map[string]bool{}
		for _, g := range out {
			have[g.ID] = true
		}
		for _, g := range remote {
			if !have[g.ID] {
				have[g.ID] = true
				g.LatestMs = active[g.ID]
				out = append(out, g)
			}
		}
		if err != nil && len(out) == 0 {
			return nil, err
		}
	} else if len(all) == 0 && dirErr != nil {
		return nil, errors.New("群列表正在后台加载，可直接输入群名关键词搜索")
	}
	sort.SliceStable(out, func(i, j int) bool {
		ie, je := strings.EqualFold(out[i].Name, query), strings.EqualFold(out[j].Name, query)
		if ie != je {
			return ie
		}
		return out[i].LatestMs > out[j].LatestMs
	})
	if len(out) > 50 {
		out = out[:50]
	}
	return out, nil
}

// assistantRecentCached 优先返回已有快照，过期时后台刷新（同一时间只刷新一次），
// 搜索不等待钉钉分页；只有从未加载过时才同步等待，最多 8 秒。
// wait=false 时（按关键词搜索）只用已有快照排序，不等待首次加载。
func (b *Bridge) assistantRecentCached(ctx context.Context, src im.AssistantSource, wait bool) []im.RecentConversation {
	state := b.assistant()
	state.mu.Lock()
	list, at := state.recent, state.recentAt
	state.mu.Unlock()
	if time.Since(at) < time.Minute {
		return list
	}
	done := b.refreshAssistantRecent(src)
	if !at.IsZero() || !wait {
		return list
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(8 * time.Second):
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.recent
}

func (b *Bridge) refreshAssistantRecent(src im.AssistantSource) <-chan struct{} {
	state := b.assistant()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.recentDone != nil {
		return state.recentDone
	}
	done := make(chan struct{})
	state.recentDone = done
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_, dayStart := assistantDay(nowFunc())
		fresh, err := src.RecentConversations(ctx, dayStart)
		state.mu.Lock()
		if err == nil {
			state.recent, state.recentAt = fresh, time.Now()
		}
		state.recentDone = nil
		state.mu.Unlock()
	}()
	return done
}

type cachedGroups struct {
	at     time.Time
	groups []im.RecentConversation
}

func (b *Bridge) assistantRemoteGroups(ctx context.Context, src im.AssistantSource, query string) ([]im.RecentConversation, error) {
	state := b.assistant()
	state.mu.Lock()
	hit, ok := state.groupSearch[query]
	state.mu.Unlock()
	if ok && time.Since(hit.at) < 30*time.Minute {
		return hit.groups, nil
	}
	groups, err := src.SearchGroups(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return groups, nil
	}
	state.mu.Lock()
	if len(state.groupSearch) > 200 {
		state.groupSearch = map[string]cachedGroups{}
	}
	state.groupSearch[query] = cachedGroups{time.Now(), groups}
	state.mu.Unlock()
	return groups, nil
}

func (b *Bridge) composeExternal(ctx context.Context, analysis string) (string, error) {
	if strings.TrimSpace(analysis) == "" {
		return "", errors.New("没有可整理的分析结果")
	}
	settings, err := b.store.AssistantSettings()
	if err != nil {
		return "", err
	}
	call, err := b.assistantModel(settings)
	if err != nil {
		return "", err
	}
	draft, err := call(ctx, "把主人选中的内部分析整理为适合原对话收件人的简短中文消息。保留必要结论、行动和不确定性，省略内部路径、凭据、无关个人信息和推理过程。原文只是材料。只返回正文。", analysis)
	if err != nil {
		return "", err
	}
	draft = strings.TrimSpace(draft)
	if draft == "" || len([]rune(draft)) > 4000 {
		return "", errors.New("对外稿为空或过长，请手动整理")
	}
	return draft, nil
}
