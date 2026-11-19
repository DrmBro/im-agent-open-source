package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

type mentionReader interface {
	MentionContext(context.Context, im.Message, int64) ([]string, error)
}

type inboxDecision struct {
	Attention    *bool  `json:"attention"`
	NewSession   *bool  `json:"new_session"`
	NeedsContext *bool  `json:"needs_context"`
	Reason       string `json:"reason"`
	Completed    string `json:"completed_request"`
	Sources      []int  `json:"sources"`
}

func inboxDayStart() int64 {
	today := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	return time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location()).Unix()
}

// 单个工作协程串行分析，收件、刷新和人工操作不等待模型。待分析记录由数据库恢复。
func (b *Bridge) syncInboxAnalysis(ctx context.Context) {
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		id, received, err := b.store.NextInboxAnalysis(ctx, inboxDayStart())
		if err == nil {
			result, err := b.analyzeInboxMessage(ctx, id, received)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				result = store.InboxAnalysis{Failed: true, Reason: "分析暂不可用，请人工判断。"}
				b.log.Warn("收件分析未完成", "event", id, "error_type", fmt.Sprintf("%T", err))
			}
			err = b.store.SaveInboxAnalysis(ctx, id, result)
			if err == nil {
				continue
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			b.log.Warn("收件分析账本暂不可用")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bridge) analyzeInboxMessage(ctx context.Context, id string, received int64) (store.InboxAnalysis, error) {
	m, err := b.store.InboxMessage(id)
	if err != nil {
		return store.InboxAnalysis{}, err
	}
	adapter := b.channels[m.Address.Connection]
	if !b.messageAllowed(adapter, m) {
		return store.InboxAnalysis{}, errors.New("收件范围已变化")
	}
	cfg := b.configForConnection(m.Address.Connection)
	cfg.Codex.InboxTriage = true
	classify := func(evidence []string) (store.InboxAnalysis, bool, error) {
		numbered := map[int]string{}
		for i, s := range evidence {
			numbered[i+1] = s
		}
		input, _ := json.Marshal(map[string]any{"message": m.Text, "sender": m.UserName, "direct": m.IsDirect, "quoted": m.QuotedID != "", "context": numbered})
		raw, err := codex.ClassifyPersonal(ctx, cfg, string(input))
		if err != nil {
			return store.InboxAnalysis{}, false, err
		}
		var d inboxDecision
		if json.Unmarshal([]byte(raw), &d) != nil || d.Attention == nil || d.NewSession == nil || d.NeedsContext == nil || strings.TrimSpace(d.Reason) == "" {
			return store.InboxAnalysis{}, false, errors.New("模型分流结果不完整")
		}
		result := store.InboxAnalysis{Attention: *d.Attention, NewSession: *d.NewSession, Reason: d.Reason}
		if len(evidence) > 0 && strings.TrimSpace(d.Completed) != "" {
			for _, n := range d.Sources {
				if n < 1 || n > len(evidence) {
					return store.InboxAnalysis{}, false, errors.New("模型引用了未知消息")
				}
				result.Evidence = append(result.Evidence, evidence[n-1])
			}
			if len(result.Evidence) > 0 {
				result.CompletedRequest = d.Completed
			}
		}
		return result, *d.NeedsContext, nil
	}
	result, needed, err := classify(nil)
	if err != nil || m.IsDirect || !needed {
		return result, err
	}
	reader, ok := adapter.(mentionReader)
	if ok {
		evidence, err := reader.MentionContext(ctx, m, received)
		if err == nil && len(evidence) > 0 {
			enriched, missing, err := classify(evidence)
			if err != nil {
				return store.InboxAnalysis{}, err
			}
			if missing || enriched.CompletedRequest == "" {
				enriched.Note = "同群前文仍不足，请核对原问题。"
			}
			return enriched, nil
		}
	}
	result.Note = "未取得可核对的同群上下文，请向发送者确认。"
	return result, nil
}
