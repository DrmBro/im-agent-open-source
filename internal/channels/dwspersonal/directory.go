package dwspersonal

import (
	"context"
	"encoding/json"
	"errors"
	"im-agent/internal/im"
	"strconv"
	"strings"
	"time"
)

type groupDirectoryCache struct {
	At     time.Time
	Groups []im.DirectoryGroup
}

func (c *Client) fetchGroupCategories(ctx context.Context) ([]im.GroupCategory, error) {
	raw, err := c.run(ctx, "", "chat", "+category-list")
	if err != nil {
		return nil, err
	}
	raw, err = successPayload(raw)
	if err != nil {
		return nil, err
	}
	var result struct {
		Categories []struct {
			ID    json.Number `json:"categoryId"`
			Title string      `json:"title"`
		} `json:"categories"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Categories == nil {
		return nil, errors.New("钉钉分组列表格式无效")
	}
	out := []im.GroupCategory{{ID: "pinned", Name: "置顶群"}}
	seen := map[string]bool{}
	for _, item := range result.Categories {
		id := item.ID.String()
		if _, err := strconv.ParseInt(id, 10, 64); err != nil || seen[id] {
			return nil, errors.New("钉钉分组身份无效")
		}
		seen[id] = true
		out = append(out, im.GroupCategory{ID: id, Name: item.Title})
	}
	return append(out, im.GroupCategory{ID: "all", Name: "全部群（含未分组）"}), nil
}
func (c *Client) fetchCategoryGroups(ctx context.Context, category string) ([]im.DirectoryGroup, error) {
	if category == "pinned" {
		groups, err := c.pinnedGroups(ctx)
		if err != nil {
			return nil, err
		}
		c.cacheDirectory(category, groups)
		return c.sortDirectory(ctx, append([]im.DirectoryGroup{}, groups...)), nil
	}
	args := []string{"chat", "+category-list-conversations", "--category-id", category}
	if category == "all" {
		args = []string{"chat", "+my-groups", "--page-all", "--page-limit", "50"}
	} else if id, err := strconv.ParseInt(category, 10, 64); err != nil || id <= 0 {
		return nil, errors.New("钉钉分组 ID 无效")
	}
	raw, err := c.run(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	raw, err = successPayload(raw)
	if err != nil {
		return nil, err
	}
	var result struct {
		Complete      bool `json:"complete"`
		HasMore       bool `json:"hasMore"`
		Failed        int  `json:"failedCount"`
		Conversations []struct {
			ID     string `json:"openConversationId"`
			Name   string `json:"conversationName"`
			Type   string `json:"conversationType"`
			Single bool   `json:"singleChat"`
		} `json:"conversations"`
		Groups []struct {
			ID      string `json:"conversationId"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			Members int    `json:"memberCount"`
		} `json:"groups"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.Complete || result.HasMore || result.Failed != 0 {
		return nil, errors.New("钉钉群列表未完整读取，请稍后重试")
	}
	out := []im.DirectoryGroup{}
	seen := map[string]bool{}
	add := func(id, name string, n int) {
		if !validID(id) || !strings.HasPrefix(id, "cid") || seen[id] {
			return
		}
		seen[id] = true
		if name == "" {
			name = id
		}
		out = append(out, im.DirectoryGroup{ID: id, Name: name, Members: n})
	}
	if category == "all" {
		if result.Groups == nil {
			return nil, errors.New("钉钉群列表缺失")
		}
		for _, g := range result.Groups {
			if strings.Contains(strings.ToLower(g.Type), "group") {
				add(g.ID, g.Name, g.Members)
			}
		}
	} else {
		if result.Conversations == nil {
			return nil, errors.New("钉钉分组内容缺失")
		}
		for _, g := range result.Conversations {
			if !g.Single && strings.EqualFold(g.Type, "group") {
				add(g.ID, g.Name, 0)
			}
		}
	}
	c.cacheDirectory(category, out)
	return c.sortDirectory(ctx, append([]im.DirectoryGroup{}, out...)), nil
}

// 页面只读取本机快照；远端请求由连接生命周期内的定时任务负责。
func (c *Client) GroupCategories(ctx context.Context) ([]im.GroupCategory, error) {
	if value, ok := c.directoryCache.Load("@categories"); ok {
		return append([]im.GroupCategory{}, value.([]im.GroupCategory)...), nil
	}
	return nil, errors.New("群目录正在后台加载，请稍后重试")
}
func (c *Client) CategoryGroups(ctx context.Context, category string) ([]im.DirectoryGroup, error) {
	if value, ok := c.directoryCache.Load(category); ok {
		return c.sortDirectory(ctx, append([]im.DirectoryGroup{}, value.(groupDirectoryCache).Groups...)), nil
	}
	return nil, errors.New("群目录正在后台加载，请稍后重试")
}
func (c *Client) refreshGroupDirectory(ctx context.Context) {
	categories, err := c.fetchGroupCategories(ctx)
	if err != nil {
		if value, ok := c.directoryCache.Load("@categories"); ok {
			categories = value.([]im.GroupCategory)
		}
	} else {
		c.directoryCache.Store("@categories", categories)
	}
	for _, category := range categories {
		if ctx.Err() != nil {
			return
		}
		// 单个分组失败保留其旧快照，其余分组继续更新。
		_, _ = c.fetchCategoryGroups(ctx, category.ID)
	}
}

type cachedGroupName struct {
	name      string
	refreshAt time.Time
}

func (c *Client) cacheDirectory(category string, groups []im.DirectoryGroup) {
	now := time.Now()
	for _, g := range groups {
		if name := strings.TrimSpace(g.Name); name != "" && name != g.ID {
			c.groupNames.Store(g.ID, cachedGroupName{name, now.Add(10 * time.Minute)})
		}
	}
	c.directoryCache.Store(category, groupDirectoryCache{now, groups})
}

func (c *Client) InboxGroupName(chat string) string {
	id, ok := strings.CutPrefix(chat, c.accountPrefix()+"group:")
	if !ok || !validID(id) {
		return ""
	}
	var cached cachedGroupName
	if v, ok := c.groupNames.Load(id); ok {
		cached = v.(cachedGroupName)
		if time.Now().Before(cached.refreshAt) {
			return cached.name
		}
	}
	c.mu.Lock()
	jobs := c.nameJobs
	c.mu.Unlock()
	if jobs == nil {
		return cached.name
	}
	key := "name:" + id
	if _, busy := c.directoryScheduled.LoadOrStore(key, true); busy {
		return cached.name
	}
	err := jobs.Push(key, func(ctx context.Context) {
		defer c.directoryScheduled.Delete(key)
		name := c.lookupGroupName(ctx, id)
		if name != "" {
			c.groupNames.Store(id, cachedGroupName{name, time.Now().Add(10 * time.Minute)})
		} else if ctx.Err() == nil {
			// 查询失败保留已有名称，短暂退避。期间目录刷新得到的新名称优先。
			next := cachedGroupName{cached.name, time.Now().Add(time.Minute)}
			if cached.refreshAt.IsZero() {
				c.groupNames.LoadOrStore(id, next)
			} else {
				c.groupNames.CompareAndSwap(id, cached, next)
			}
		}
	})
	if err != nil {
		c.directoryScheduled.Delete(key)
	}
	return cached.name
}

func (c *Client) lookupGroupName(ctx context.Context, id string) string {
	raw, err := c.run(ctx, "", "chat", "+conversation-info", "--group", id)
	if err != nil {
		return ""
	}
	raw, err = successPayload(raw)
	if err != nil {
		return ""
	}
	var response struct {
		Success bool `json:"success"`
		Result  struct {
			Conversation struct {
				ID     string `json:"openConversationId"`
				Title  string `json:"title"`
				Single bool   `json:"singleChat"`
			} `json:"conversationInfo"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || !response.Success {
		return ""
	}
	group := response.Result.Conversation
	if group.ID != id || group.Single || group.Title == id {
		return ""
	}
	return strings.TrimSpace(group.Title)
}
