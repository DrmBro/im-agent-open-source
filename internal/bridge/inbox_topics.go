package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) syncInboxTopics(ctx context.Context) {
	retry := map[string]time.Time{}
	tick := time.NewTicker(8 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		rows, err := b.store.PersonalInbox(ctx, inboxDayStart())
		if err == nil {
			byChat := map[string][]store.PersonalInboxItem{}
			var order []string
			for _, row := range rows {
				if row.State != "new" {
					continue
				}
				addr := im.Address{Channel: row.Channel, Connection: row.Connection, ChatID: row.Chat}
				if !b.messageAllowed(b.channels[row.Connection], im.Message{Address: addr, UserID: row.User, IsDirect: row.IsDirect}) {
					continue
				}
				key := addr.Key()
				if _, ok := byChat[key]; !ok {
					order = append(order, key)
				}
				byChat[key] = append(byChat[key], row)
			}
			for _, key := range order {
				if ctx.Err() != nil {
					return
				}
				if len(byChat[key]) < 2 || time.Now().Before(retry[key]) {
					continue
				}
				if err := b.assignInboxTopics(ctx, byChat[key]); err != nil {
					retry[key] = time.Now().Add(time.Minute)
					b.log.Warn("收件事项归类暂不可用，将保留原消息并重试")
				} else {
					delete(retry, key)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func topicExcerpt(s string) string {
	chars := []rune(strings.TrimSpace(s))
	return string(chars[:min(400, len(chars))])
}

func (b *Bridge) assignInboxTopics(ctx context.Context, rows []store.PersonalInboxItem) error {
	var pending []store.PersonalInboxItem
	var existing []store.InboxTopic
	known := map[string]bool{}
	var examples []map[string]any
	// 最近的事项作为延续候选，附一条原文避免同名误并。
	for _, r := range rows {
		if r.TopicID != "" && !known[r.TopicID] && len(existing) < 24 {
			known[r.TopicID] = true
			existing = append(existing, store.InboxTopic{ID: r.TopicID, Title: r.TopicTitle})
			examples = append(examples, map[string]any{"id": len(existing), "title": r.TopicTitle, "example": topicExcerpt(r.Text)})
		}
	}
	var inputs []map[string]any
	for i := len(rows) - 1; i >= 0 && len(pending) < 24; i-- {
		r := rows[i]
		if r.TopicID != "" {
			continue
		}
		pending = append(pending, r)
		item := map[string]any{"id": len(pending), "text": topicExcerpt(r.Text), "sender": r.UserName}
		if a := r.Analysis; a != nil && !a.Failed {
			item["clarified"] = topicExcerpt(a.CompletedRequest)
		}
		inputs = append(inputs, item)
	}
	if len(pending) == 0 {
		return nil
	}
	data, err := json.Marshal(map[string]any{"messages": inputs, "existing": examples})
	if err != nil {
		return err
	}
	cfg := b.configForConnection(rows[0].Connection)
	cfg.Codex.InboxTopics = true
	answer, err := codex.ClassifyPersonal(ctx, cfg, string(data))
	if err != nil {
		return err
	}
	var result struct {
		Topics []struct {
			Title    string
			Existing int
			Messages []int
		}
	}
	if err = json.Unmarshal([]byte(answer), &result); err != nil {
		return err
	}
	seen := map[int]bool{}
	var topics []store.InboxTopic
	for _, group := range result.Topics {
		title := strings.TrimSpace(group.Title)
		if group.Existing < 0 || group.Existing > len(existing) || len(group.Messages) == 0 || title == "" || len([]rune(title)) > 100 {
			return errors.New("事项归类格式无效")
		}
		topic := store.InboxTopic{Title: title}
		if group.Existing > 0 {
			topic = existing[group.Existing-1]
		}
		for _, n := range group.Messages {
			if n < 1 || n > len(pending) || seen[n] {
				return errors.New("事项归类消息编号无效")
			}
			seen[n] = true
			topic.Messages = append(topic.Messages, pending[n-1].ID)
		}
		if topic.ID == "" {
			topic.ID = topic.Messages[0]
		}
		topics = append(topics, topic)
	}
	if len(seen) != len(pending) {
		return errors.New("事项归类缺少消息")
	}
	return b.store.SaveInboxTopics(ctx, topics)
}
