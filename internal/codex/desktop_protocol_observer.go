package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 直接按 turn/start 回执中的真实 turn_id 查询公开结果。原始日志只作兼容回退。
func (c *Client) readProtocolTurn(ctx context.Context, thread, turn string) (Turn, error) {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 4; page++ {
		params := map[string]any{"threadId": thread, "limit": 8, "sortDirection": "desc", "itemsView": "full"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.sessionControlCall(ctx, "thread/turns/list", params)
		if err != nil {
			return Turn{}, err
		}
		var result struct {
			Data       []Turn `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Data == nil {
			return Turn{}, errors.New("轮次查询响应不完整")
		}
		var found *Turn
		for _, entry := range result.Data {
			if matchesProtocolTurn(turn, entry.ID) {
				if found != nil {
					return Turn{}, errors.New("轮次查询返回重复标识")
				}
				copy := entry
				found = &copy
			}
		}
		if found != nil {
			return *found, nil
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
		if seen[cursor] {
			return Turn{}, errors.New("轮次查询游标重复")
		}
		seen[cursor] = true
	}
	return Turn{}, errors.New("有限分页内尚未找到目标轮次")
}

var errProtocolReconstructedIdentity = errors.New("协议返回重建条目标识，需要原日志核验")

func protocolReplies(turn Turn) ([]HistoryMessage, error) {
	if turn.Items == nil || turn.ItemsView != "" && turn.ItemsView != "full" {
		return nil, errors.New("轮次条目尚未完整加载")
	}
	var replies []HistoryMessage
	seen := map[string]bool{}
	for _, item := range turn.Items {
		if item.Type != "agentMessage" || item.Phase != "" && item.Phase != "final_answer" {
			continue
		}
		if item.Phase == "" {
			return nil, errors.New("缺省阶段回复需要原日志完成正文核验")
		}
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		// 已实测 legacy 会话会返回 item-N，这不是原始日志身份，不能进入投递账本。
		if item.ID != "" && !strings.HasPrefix(item.ID, "msg_") {
			return nil, errProtocolReconstructedIdentity
		}
		if item.ID == "" || seen[item.ID] {
			return nil, errors.New("最终回复标识缺失或重复")
		}
		seen[item.ID] = true
		replies = append(replies, HistoryMessage{ID: item.ID, Role: "assistant", Text: item.Text})
	}
	if len(replies) == 0 {
		return nil, errors.New("完成轮次尚无可确认最终文本")
	}
	return replies, nil
}

// 返回 false 才启用原日志观察。任何分支都不恢复或重发模型输入。
func (c *Client) observeProtocolTurn(ctx context.Context, thread, reference, actual string) bool {
	failures := 0
	for ctx.Err() == nil {
		query, cancel := context.WithTimeout(ctx, 9*time.Second)
		turn, err := c.readProtocolTurn(query, thread, actual)
		cancel()
		if err == nil {
			switch turn.Status {
			case "completed":
				if turn.Error != nil {
					return c.publishProtocolResult(ctx, thread, reference, "failed", nil)
				}
				replies, replyErr := protocolReplies(turn)
				if replyErr == nil {
					return c.publishProtocolResult(ctx, thread, reference, "completed", replies)
				}
				if errors.Is(replyErr, errProtocolReconstructedIdentity) {
					return false
				}
				err = replyErr
			case "failed", "interrupted":
				return c.publishProtocolResult(ctx, thread, reference, turn.Status, nil)
			case "inProgress":
				failures = 0
			default:
				err = errors.New("轮次状态无法识别")
			}
		}
		if err != nil {
			failures++
			if failures >= 3 {
				return false
			}
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(2 * time.Second):
		}
	}
	return true
}

// item/completed 使用完整正文，不拼接流式预览；背压时等待，不静默丢掉完成通知。
func (c *Client) publishProtocolResult(ctx context.Context, thread, reference, status string, replies []HistoryMessage) bool {
	// 订阅锁保护关闭；背压等待有独立预算，不能阻塞所有会话直到模型任务超时。
	delivery, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c.subMu.Lock()
	defer c.subMu.Unlock()
	ch := c.subs[thread]
	if ch == nil {
		return true
	}
	for _, reply := range replies {
		params := map[string]any{"threadId": thread, "turnId": reference, "item": map[string]any{"id": reply.ID, "type": "agentMessage", "phase": "final_answer", "text": reply.Text}}
		select {
		case ch <- Notice{"item/completed", mustJSON(params)}:
		case <-delivery.Done():
			return true
		}
	}
	params := map[string]any{"threadId": thread, "turn": map[string]any{"id": reference, "status": status}}
	select {
	case ch <- Notice{"turn/completed", mustJSON(params)}:
	case <-delivery.Done():
	}
	return true
}
