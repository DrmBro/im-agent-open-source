package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"im-agent/internal/im"
	"im-agent/internal/store"
)

type inputQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	IsSecret bool   `json:"isSecret"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}
type inputChoicePayload struct {
	Choices []string `json:"choices"`
}

func validateActionChoice(a store.Action, choice string) error {
	if a.Kind != "input" {
		if choice == "accept" || choice == "decline" {
			return nil
		}
		return errors.New("不支持的审批选项")
	}
	var p inputChoicePayload
	if json.Unmarshal([]byte(a.Payload), &p) != nil {
		return errors.New("选择记录无效")
	}
	for _, valid := range p.Choices {
		if valid == choice {
			return nil
		}
	}
	return errors.New("该选项不在当前问题的可选范围内")
}

// 多问题逐张展示，最后按问题 ID 返回真正的 answers，不能返回审批 decision。
func (b *Bridge) requestUserInput(ctx context.Context, adapter im.Adapter, s store.Session, raw json.RawMessage) (any, error) {
	var p struct {
		Questions []inputQuestion `json:"questions"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.Questions) == 0 || len(p.Questions) > 10 {
		return nil, errors.New("问题数量或格式无效")
	}
	ids := map[string]bool{}
	for _, q := range p.Questions {
		if q.ID == "" || ids[q.ID] || strings.TrimSpace(q.Question) == "" || q.IsSecret {
			return nil, errors.New("问题标识为空/重复或包含不宜在群中收集的秘密信息")
		}
		ids[q.ID] = true
		if len(q.Options) == 0 || len(q.Options) > 100 {
			return nil, errors.New("当前 IM 选择框需要 1..100 个明确选项，自由文本问题请在 Codex 中回答")
		}
		for _, o := range q.Options {
			if o.Label == "" {
				return nil, errors.New("选项名称不能为空")
			}
		}
	}
	answers := map[string]any{}
	for i, q := range p.Questions {
		choice, err := b.waitInputChoice(ctx, adapter, s, q, i+1, len(p.Questions))
		if err != nil {
			return nil, err
		}
		answers[q.ID] = map[string]any{"answers": []string{choice}}
	}
	return map[string]any{"answers": answers}, nil
}

func (b *Bridge) waitInputChoice(ctx context.Context, adapter im.Adapter, s store.Session, q inputQuestion, index, total int) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	actionID := store.ID()
	title := fmt.Sprintf("Codex 需要选择（%d/%d）", index, total)
	if q.Header != "" {
		title += " · " + q.Header
	}
	options := []im.Button{}
	payload := inputChoicePayload{}
	text := q.Question
	for i, o := range q.Options {
		key := strconv.Itoa(i)
		options = append(options, im.Button{Label: o.Label, Value: key})
		payload.Choices = append(payload.Choices, key)
		if o.Description != "" {
			text += "\n\n" + o.Label + "：" + o.Description
		}
	}
	ttl := time.Duration(b.cfg.Limits.ApprovalTimeoutSeconds) * time.Second
	if err := b.store.PutAction(store.Action{ID: actionID, ThreadID: s.ThreadID, Address: s.Address, UserID: s.Owner, Kind: "input", Payload: store.JSON(payload), ExpiresAt: time.Now().Add(ttl).Unix()}); err != nil {
		return "", err
	}
	ch := make(chan approvalResult, 1)
	b.mu.Lock()
	b.approvals[actionID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.approvals, actionID)
		b.mu.Unlock()
		_ = b.store.Expire(actionID, "timeout")
	}()
	card := im.Card{Title: title, Text: text, ActionID: actionID}
	if adapter.Capabilities().SelectMenus {
		card.Selection = &im.Selection{Placeholder: "请选择一项（选择后提交）", Options: options}
	} else {
		card.Buttons = options
	}
	messageID, err := adapter.SendCard(ctx, s.Address, card, actionID)
	if err != nil {
		return "", err
	}
	if err = b.store.ActionMessage(actionID, messageID); err != nil {
		return "", errors.New("选择卡片回执保存失败，未接受任何选择")
	}
	timer := time.NewTimer(ttl)
	defer timer.Stop()
	select {
	case result := <-ch:
		key, ok := result.Value.(string)
		if !ok {
			return "", errors.New("选择类型无效")
		}
		n, err := strconv.Atoi(key)
		if err != nil || n < 0 || n >= len(q.Options) || key != strconv.Itoa(n) {
			return "", errors.New("选择超出范围")
		}
		resolved, err := b.store.Resolve(actionID, key)
		if err != nil || !resolved || ctx.Err() != nil {
			return "", errors.New("选择已失效或会话已关闭")
		}
		answer := q.Options[n].Label
		_ = adapter.UpdateCard(ctx, s.Address, messageID, im.Card{Title: title, Text: q.Question + "\n\n已选择：" + answer})
		return answer, nil
	case <-timer.C:
		_ = adapter.UpdateCard(ctx, s.Address, messageID, im.Card{Title: title, Text: q.Question + "\n\n已超时，未提交答案。"})
		return "", errors.New("用户选择超时，未默认选择任何选项")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
