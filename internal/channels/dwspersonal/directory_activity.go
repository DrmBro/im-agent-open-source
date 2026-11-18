package dwspersonal

import (
	"context"
	"encoding/json"
	"errors"
	"im-agent/internal/im"
	"im-agent/internal/queue"
	"sort"
	"strconv"
	"strings"
	"time"
)

type directoryActivity struct {
	At    time.Time
	Last  int64
	State string
}

func parseDirectoryTime(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	value := strings.Trim(string(raw), "\"")
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		if n > 0 && n < 100000000000 {
			n *= 1000
		}
		return n
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UnixMilli()
	}
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", value, zone); err == nil {
		return t.UnixMilli()
	}
	return 0
}
func (c *Client) startDirectoryJobs(ctx context.Context) func() {
	jobs := queue.New(ctx, 4, 256)
	c.mu.Lock()
	c.directoryJobs = jobs
	c.mu.Unlock()
	return func() { c.mu.Lock(); c.directoryJobs = nil; c.mu.Unlock(); jobs.Wait() }
}

// 会话窗口只用于补充已知时间，不把满页窗口当成完整群目录。
func (c *Client) readDirectoryActivityIndex(ctx context.Context) {
	c.directoryIndexMu.Lock()
	defer c.directoryIndexMu.Unlock()
	if time.Since(c.directoryIndexAt) < time.Minute {
		return
	}
	c.directoryIndexAt = time.Now()
	raw, err := c.run(ctx, "", "chat", "list-all-conversations", "--limit", "100")
	if err != nil {
		return
	}
	raw, err = successPayload(raw)
	if err != nil {
		return
	}
	var r struct {
		Success bool `json:"success"`
		Result  struct {
			Conversations []struct {
				ID     string          `json:"openConversationId"`
				Single bool            `json:"singleChat"`
				Last   json.RawMessage `json:"lastMsgCreateAt"`
			} `json:"conversations"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &r) != nil || !r.Success {
		return
	}
	for _, g := range r.Result.Conversations {
		if last := parseDirectoryTime(g.Last); !g.Single && last > 0 && validID(g.ID) {
			c.directoryActivity.Store(g.ID, directoryActivity{time.Now(), last, "known"})
		}
	}
}
func (c *Client) scheduleDirectoryActivity(id string) {
	if _, busy := c.directoryScheduled.LoadOrStore(id, true); busy {
		return
	}
	c.mu.Lock()
	jobs := c.directoryJobs
	c.mu.Unlock()
	if jobs == nil {
		c.directoryScheduled.Delete(id)
		return
	}
	if err := jobs.Push(id, func(ctx context.Context) {
		defer c.directoryScheduled.Delete(id)
		state, last := "unavailable", int64(0)
		raw, err := c.run(ctx, "", "chat", "message", "list", "--conversation-id", id, "--direction", "older", "--limit", "1")
		if err == nil {
			raw, err = successPayload(raw)
		}
		if err == nil {
			var r struct {
				Success  bool `json:"success"`
				Partial  bool `json:"partial"`
				Failed   int  `json:"failedCount"`
				Messages []struct {
					Conversation   string          `json:"openConversationId"`
					ConversationID string          `json:"conversationId"`
					Created        json.RawMessage `json:"createTime"`
				} `json:"messages"`
			}
			if json.Unmarshal(raw, &r) == nil && r.Success && !r.Partial && r.Failed == 0 && r.Messages != nil {
				if len(r.Messages) == 0 {
					state = "empty"
				} else {
					m := r.Messages[0]
					if (m.Conversation == "" || m.Conversation == id) && (m.ConversationID == "" || m.ConversationID == id) {
						last = parseDirectoryTime(m.Created)
						if last > 0 {
							state = "known"
						}
					}
				}
			}
		}
		if ctx.Err() == nil {
			c.directoryActivity.Store(id, directoryActivity{time.Now(), last, state})
		}
	}); err != nil {
		c.directoryScheduled.Delete(id)
	}
}
func (c *Client) sortDirectory(ctx context.Context, groups []im.DirectoryGroup) []im.DirectoryGroup {
	c.readDirectoryActivityIndex(ctx)
	for i := range groups {
		g := &groups[i]
		if g.LastMessageAt > 0 {
			c.directoryActivity.Store(g.ID, directoryActivity{time.Now(), g.LastMessageAt, "known"})
		}
		value, ok := c.directoryActivity.Load(g.ID)
		if ok {
			v := value.(directoryActivity)
			g.LastMessageAt = v.Last
			g.TimeState = v.State
			if time.Since(v.At) < 15*time.Minute {
				continue
			}
		}
		g.TimeState = "loading"
		c.scheduleDirectoryActivity(g.ID)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].LastMessageAt > groups[j].LastMessageAt })
	return groups
}
func (c *Client) pinnedGroups(ctx context.Context) ([]im.DirectoryGroup, error) {
	out := []im.DirectoryGroup{}
	cursor := "0"
	seen := map[string]bool{}
	pages := map[string]bool{}
	for page := 0; page < 50; page++ {
		if pages[cursor] {
			return nil, errors.New("置顶群分页游标重复")
		}
		pages[cursor] = true
		raw, err := c.run(ctx, "", "chat", "+conversation-list-top", "--type", "group", "--limit", "100", "--no-detail", "--cursor", cursor)
		if err != nil {
			return nil, err
		}
		raw, err = successPayload(raw)
		if err != nil {
			return nil, err
		}
		var r struct {
			Complete      bool            `json:"complete"`
			HasMore       bool            `json:"hasMore"`
			Next          json.RawMessage `json:"nextCursor"`
			Conversations []struct {
				ID     string          `json:"openConversationId"`
				Name   string          `json:"conversationName"`
				Type   string          `json:"conversationType"`
				Single bool            `json:"singleChat"`
				Last   json.RawMessage `json:"lastMsgCreateAt"`
			} `json:"conversations"`
		}
		if json.Unmarshal(raw, &r) != nil || r.Conversations == nil {
			return nil, errors.New("置顶群列表格式无效")
		}
		for _, g := range r.Conversations {
			if g.Single || g.Type != "group" || !validID(g.ID) || seen[g.ID] {
				continue
			}
			seen[g.ID] = true
			out = append(out, im.DirectoryGroup{ID: g.ID, Name: g.Name, LastMessageAt: parseDirectoryTime(g.Last), Pinned: true})
		}
		if !r.HasMore {
			if !r.Complete {
				return nil, errors.New("置顶群列表完整性未确认")
			}
			return out, nil
		}
		cursor = strings.Trim(string(r.Next), "\"")
		if n, err := strconv.ParseInt(cursor, 10, 64); err != nil || n <= 0 {
			return nil, errors.New("置顶群缺少有效续页游标")
		}
	}
	return nil, errors.New("置顶群分页超过上限")
}
