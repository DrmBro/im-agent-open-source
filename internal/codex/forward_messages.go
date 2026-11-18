package codex

import (
	"context"
	"errors"
)

// ForwardMessages 复用实时日志的公开消息解析，不包含工具和内部推理。
// before 为消息 ID，返回其之前的最多 30 条消息；空值读取最新一页。
func (c *Client) ForwardMessages(ctx context.Context, thread, before string) ([]HistoryMessage, bool, error) {
	files, err := c.LiveRolloutFiles(ctx, thread)
	if err != nil {
		return nil, false, err
	}
	if len(files) == 0 {
		return nil, false, errors.New("没有找到可核对的本机会话日志")
	}
	messages := []HistoryMessage{}
	seen := map[string]bool{}
	found := before == ""
	stop := false
	var scanned int64
	for _, path := range files {
		offset, size, err := c.LiveRolloutInfo(ctx, thread, path)
		if err != nil {
			return nil, false, err
		}
		scanned += size
		if scanned > 512<<20 {
			return nil, false, errors.New("会话日志超过512MB，请选择较小的会话")
		}
		for offset < size && !stop {
			batch, next, err := c.ReadLiveMessages(ctx, thread, path, offset)
			if err != nil {
				return nil, false, err
			}
			if next == offset {
				break
			}
			offset = next
			for _, m := range batch {
				if m.Issue != nil || m.ID == "" || seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				if m.ID == before {
					found = true
					stop = true
					break
				}
				messages = append(messages, m.HistoryMessage)
				if len(messages) > 31 {
					messages = messages[len(messages)-31:]
				}
			}
		}
		if stop {
			break
		}
	}
	if !found {
		return nil, false, errors.New("消息分页位置已变化，请重新加载会话")
	}
	more := len(messages) > 30
	if more {
		messages = messages[len(messages)-30:]
	}
	return messages, more, nil
}
