package codex

import (
	"context"
	"encoding/json"
	"errors"
)

// RecentHistory 先读取存储契约；分页会话不请求已被弃用的全量 includeTurns。
func (c *Client) RecentHistory(ctx context.Context, id string) (Thread, []HistoryMessage, error) {
	return c.recentHistory(ctx, id, 5)
}
func (c *Client) RecentConversation(ctx context.Context, id string) (Thread, []HistoryMessage, error) {
	if messages, found, err := c.recentRollout(ctx, id); found || err != nil {
		// 展示名称由独立 metadata 路径维护；消息内容不依赖可能滞后的分页索引。
		return Thread{ID: id}, messages, err
	}
	metadata, e := c.ReadMetadata(ctx, id)
	if e != nil {
		return metadata, nil, e
	}
	if metadata.HistoryMode == "paginated" {
		messages, e := c.readRecentPages(ctx, id, 2)
		return metadata, messages, e
	}
	thread, e := c.Read(ctx, id)
	if e != nil {
		return metadata, nil, e
	}
	for _, turn := range thread.Turns {
		if turn.ItemsView != "" && turn.ItemsView != "full" {
			return metadata, nil, errors.New("Codex 只返回部分历史，不能当成完整历史发送")
		}
	}
	messages, e := RecentConversation(thread)
	return thread, messages, e
}
func (c *Client) recentHistory(ctx context.Context, id string, limit int) (Thread, []HistoryMessage, error) {
	metadata, e := c.ReadMetadata(ctx, id)
	if e != nil {
		return metadata, nil, e
	}
	if metadata.HistoryMode == "paginated" {
		messages, e := c.readRecentPages(ctx, id, limit)
		return metadata, messages, e
	}
	thread, e := c.Read(ctx, id)
	if e != nil {
		// 有的旧接口没有返回 historyMode，但明确拒绝 includeTurns；只尝试只读分页，不重放任务。
		var rpc *RPCError
		if errors.As(e, &rpc) && rpc.Code == -32600 {
			messages, pageErr := c.readRecentPages(ctx, id, limit)
			return metadata, messages, pageErr
		}
		return metadata, nil, e
	}
	for _, turn := range thread.Turns {
		if turn.ItemsView != "" && turn.ItemsView != "full" {
			return metadata, nil, errors.New("Codex 只返回部分历史，不能当成完整历史发送")
		}
	}
	messages, e := RecentMessages(thread, limit)
	return thread, messages, e
}
func (c *Client) readRecentPages(ctx context.Context, id string, limit int) ([]HistoryMessage, error) {
	out := []HistoryMessage{}
	cursor := ""
	turnCursors := map[string]bool{}
	pages := 0
	for len(out) < limit {
		if pages >= 100 {
			return nil, errors.New("历史分页超过安全上限，未将部分结果标记为成功")
		}
		pages++
		params := map[string]any{"threadId": id, "limit": 8, "itemsView": "notLoaded", "sortDirection": "desc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, e := c.Call(ctx, "thread/turns/list", params)
		if e != nil {
			return nil, historyPageError(e)
		}
		var response struct {
			Data       []Turn  `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Data == nil {
			return nil, errors.New("Codex 轮次分页响应无效")
		}
		for _, turn := range response.Data {
			if turn.Status == "inProgress" {
				continue
			}
			if turn.ID == "" {
				return nil, errors.New("历史轮次缺少 ID")
			}
			itemCursor := ""
			itemCursors := map[string]bool{}
			for len(out) < limit {
				if pages >= 100 {
					return nil, errors.New("历史条目分页超过安全上限")
				}
				pages++
				p := map[string]any{"threadId": id, "turnId": turn.ID, "limit": 20, "sortDirection": "desc"}
				if itemCursor != "" {
					p["cursor"] = itemCursor
				}
				raw, e := c.Call(ctx, "thread/items/list", p)
				if e != nil {
					return nil, historyPageError(e)
				}
				var entries struct {
					Data []struct {
						TurnID string `json:"turnId"`
						Item   Item   `json:"item"`
					} `json:"data"`
					NextCursor *string `json:"nextCursor"`
				}
				if json.Unmarshal(raw, &entries) != nil || entries.Data == nil {
					return nil, errors.New("Codex 条目分页响应无效")
				}
				for _, entry := range entries.Data {
					if entry.TurnID != turn.ID {
						return nil, errors.New("历史条目不属于请求轮次")
					}
					message, ok, e := messageFromItem(entry.Item)
					if e != nil {
						return nil, e
					}
					if ok {
						out = append(out, message)
						if len(out) == limit {
							break
						}
					}
				}
				if entries.NextCursor == nil || *entries.NextCursor == "" {
					break
				}
				itemCursor = *entries.NextCursor
				if itemCursors[itemCursor] {
					return nil, errors.New("历史条目游标重复，停止读取")
				}
				itemCursors[itemCursor] = true
			}
			if len(out) == limit {
				break
			}
		}
		if len(out) == limit || response.NextCursor == nil || *response.NextCursor == "" {
			break
		}
		cursor = *response.NextCursor
		if turnCursors[cursor] {
			return nil, errors.New("历史轮次游标重复，停止读取")
		}
		turnCursors[cursor] = true
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
func historyPageError(err error) error {
	var rpc *RPCError
	if errors.As(err, &rpc) && (rpc.Code == -32601 || rpc.Code == -32600) {
		return errors.New("当前 Codex CLI 不支持此会话的分页读取；请使用与桌面版本兼容的 CLI（code=-32601）")
	}
	return err
}
