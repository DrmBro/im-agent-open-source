package codex

import (
	"encoding/json"
	"errors"
	"strings"
)

type HistoryMessage struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Text string `json:"text"`
}

// messageFromItem 共用角色筛选，不把工具/推理/进度混入用户可见历史。
func messageFromItem(item Item) (HistoryMessage, bool, error) {
	var text, role string
	switch item.Type {
	case "userMessage":
		role = "user"
		var content []InputContent
		if json.Unmarshal(item.Content, &content) != nil {
			return HistoryMessage{}, false, errors.New("用户历史消息结构不完整，暂不补发")
		}
		parts := []string{}
		for _, part := range content {
			switch part.Type {
			case "text", "input_text":
				if part.Text != "" {
					parts = append(parts, part.Text)
				}
			case "image", "localImage":
				parts = append(parts, "[图片，未转发附件]")
			}
		}
		text = strings.Join(parts, "\n")
	case "agentMessage":
		if item.Phase != "" && item.Phase != "final_answer" {
			return HistoryMessage{}, false, nil
		}
		role = "assistant"
		text = item.Text
	default:
		return HistoryMessage{}, false, nil
	}
	if strings.TrimSpace(text) == "" {
		return HistoryMessage{}, false, nil
	}
	return HistoryMessage{ID: item.ID, Role: role, Text: text}, true, nil
}

// RecentConversation 返回最近一个已完成轮次中的用户消息与最终助手回复。
// 它不是最近两条跨轮次消息，避免把上一轮助手回复和本轮用户输入拼成一组。
func RecentConversation(thread Thread) ([]HistoryMessage, error) {
	for ti := len(thread.Turns) - 1; ti >= 0; ti-- {
		turn := thread.Turns[ti]
		if turn.Status == "inProgress" || turn.Status == "interrupted" || turn.Status == "failed" {
			continue
		}
		out := []HistoryMessage{}
		for _, item := range turn.Items {
			message, ok, err := messageFromItem(item)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, message)
			}
		}
		if len(out) > 2 {
			// 一轮可能包含多个助手最终 item，只保留用户消息和最后一个助手回复。
			filtered := make([]HistoryMessage, 0, 2)
			for _, message := range out {
				if message.Role == "user" {
					filtered = append(filtered, message)
				}
			}
			for i := len(out) - 1; i >= 0; i-- {
				if out[i].Role == "assistant" {
					filtered = append(filtered, out[i])
					break
				}
			}
			if len(filtered) > 2 {
				filtered = filtered[len(filtered)-2:]
			}
			out = filtered
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, nil
}
func RecentMessages(thread Thread, limit int) ([]HistoryMessage, error) {
	if limit < 1 {
		return nil, nil
	}
	if limit > 5 {
		limit = 5
	}
	out := []HistoryMessage{}
	for ti := len(thread.Turns) - 1; ti >= 0 && len(out) < limit; ti-- {
		turn := thread.Turns[ti]
		if turn.Status == "inProgress" {
			continue
		}
		for i := len(turn.Items) - 1; i >= 0 && len(out) < limit; i-- {
			m, ok, e := messageFromItem(turn.Items[i])
			if e != nil {
				return nil, e
			}
			if ok {
				out = append(out, m)
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
