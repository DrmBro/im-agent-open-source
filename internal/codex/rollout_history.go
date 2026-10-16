package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
)

// 桌面持有的活跃会话可能尚未更新独立 App Server 的分页索引。
// 这里只读同一个 session_meta.id 的追加日志，不修改 Codex 文件、不恢复或执行会话。
// 每个 Client 缓存已消费字节，只扫描新增完整行，保留最后一个用户输入和最终回复。
type rolloutHistory struct {
	mu              sync.Mutex
	path            string
	offset          int64
	info            os.FileInfo
	verified        bool
	user, assistant HistoryMessage
}

func (c *Client) recentRollout(ctx context.Context, id string) ([]HistoryMessage, bool, error) {
	value, _ := c.rolloutHistories.LoadOrStore(id, &rolloutHistory{})
	cache := value.(*rolloutHistory)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.path == "" {
		cache.path, _ = findDesktopRollout(c.runtimeConfig.Codex.Home, id)
		if cache.path == "" {
			return nil, false, nil
		}
	}
	file, err := os.Open(cache.path)
	if err != nil {
		return nil, true, errors.New("桌面会话日志不可读，未回退旧快照")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, true, err
	}
	if cache.info != nil && (!os.SameFile(cache.info, info) || info.Size() < cache.offset) {
		cache.offset = 0
		cache.verified = false
		cache.user, cache.assistant = HistoryMessage{}, HistoryMessage{}
	}
	cache.info = info
	if _, err = file.Seek(cache.offset, io.SeekStart); err != nil {
		return nil, true, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		if ctx.Err() != nil {
			return nil, true, ctx.Err()
		}
		raw, err := readRolloutLine(reader)
		if err == io.EOF {
			break
		} // 未完成的一行留到下次从原始 offset 重读。
		if err != nil {
			return nil, true, err
		}
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &record) != nil {
			return nil, true, errors.New("桌面会话日志包含无效记录")
		}
		if !cache.verified {
			var meta struct {
				ID string `json:"id"`
			}
			if record.Type != "session_meta" || json.Unmarshal(record.Payload, &meta) != nil || meta.ID != id {
				return nil, true, errors.New("桌面日志会话标识不匹配，拒绝同步")
			}
			cache.verified = true
		}
		if record.Type == "response_item" {
			var item struct {
				Type    string         `json:"type"`
				Role    string         `json:"role"`
				ID      string         `json:"id"`
				Phase   string         `json:"phase"`
				Content []InputContent `json:"content"`
			}
			if json.Unmarshal(record.Payload, &item) == nil && item.Type == "message" {
				var parts []string
				for _, part := range item.Content {
					if part.Type == "input_text" || part.Type == "output_text" || part.Type == "text" {
						parts = append(parts, part.Text)
					}
				}
				text := strings.TrimSpace(strings.Join(parts, "\n"))
				if text != "" {
					message := HistoryMessage{ID: item.ID, Role: item.Role, Text: text}
					if message.ID == "" {
						message.ID = storeHash(string(raw))
					}
					if item.Role == "user" && visibleUserText(text) {
						cache.user = message
					}
					if item.Role == "assistant" && item.Phase == "final_answer" {
						cache.assistant = message
					}
				}
			}
		}
		cache.offset += int64(len(raw))
	}
	if !cache.verified {
		return nil, true, errors.New("桌面会话日志元数据未完整写入")
	}
	messages := []HistoryMessage{}
	if cache.user.Text != "" {
		messages = append(messages, cache.user)
	}
	if cache.assistant.Text != "" {
		messages = append(messages, cache.assistant)
	}
	return messages, true, nil
}

// 日志里的 user 角色也承载内部代理回传，不等于人类手工发送。
// 仅过滤完整且可解析的已知通知格式，普通正文提及/引用标签不受影响。
func internalSubagentNotification(text string) bool {
	text = strings.TrimSpace(text)
	const start, end = "<subagent_notification>", "</subagent_notification>"
	if !strings.HasPrefix(text, start) || !strings.HasSuffix(text, end) {
		return false
	}
	var payload struct {
		AgentPath string          `json:"agent_path"`
		AgentID   string          `json:"agent_id"`
		Status    json.RawMessage `json:"status"`
	}
	raw := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, start), end))
	return json.Unmarshal([]byte(raw), &payload) == nil &&
		(payload.AgentPath != "" || payload.AgentID != "") && len(payload.Status) > 0 && string(payload.Status) != "null"
}

func visibleUserText(text string) bool {
	if internalSubagentNotification(text) {
		return false
	}
	for _, prefix := range []string{"<environment_context>", "<codex_internal_context", "# AGENTS.md instructions", "<permissions instructions>", "Another language model started to solve this problem"} {
		if strings.HasPrefix(text, prefix) {
			return false
		}
	}
	return true
}

var errRolloutLineTooLarge = errors.New("桌面日志单条记录过大，停止同步")

func readRolloutLine(reader *bufio.Reader) ([]byte, error) {
	const limit = 32 << 20
	var raw []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(raw)+len(part) > limit {
			return nil, errRolloutLineTooLarge
		}
		raw = append(raw, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return raw, err
	}
}
