package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"im-agent/internal/telemetry"
)

type rolloutObservation struct {
	Messages   []HistoryMessage
	Snapshot   telemetry.Snapshot
	UserItemID string
	ReplyIssue string
}

type rolloutHistory struct {
	anchor                []byte
	mu                    sync.Mutex
	path                  string
	offset                int64
	turnStart             int64
	info                  os.FileInfo
	discoveredAt          time.Time
	currentTurn, userTurn string
	user, assistant       HistoryMessage
	state                 telemetry.State
	lastAt                int64
	replyIssue            string
}

// 消息摘要与会话状态共用一个增量观察器；同一日志只按完整行推进。
func (c *Client) observeRollout(ctx context.Context, id string) (rolloutObservation, bool, error) {
	value, _ := c.rolloutHistories.LoadOrStore(id, &rolloutHistory{})
	cache := value.(*rolloutHistory)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.path == "" || time.Since(cache.discoveredAt) > 5*time.Second {
		files, err := c.LiveRolloutFiles(ctx, id)
		if err != nil {
			return rolloutObservation{}, true, err
		}
		if len(files) == 0 {
			return rolloutObservation{}, false, nil
		}
		cache.path = files[len(files)-1]
		cache.discoveredAt = time.Now()
	}
	file, header, err := c.openLiveRollout(id, cache.path)
	if err != nil {
		cache.discoveredAt = time.Time{}
		return rolloutObservation{}, true, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return rolloutObservation{}, true, err
	}
	changed := false
	if len(cache.anchor) > 0 && cache.offset <= info.Size() {
		current := make([]byte, len(cache.anchor))
		if _, readErr := file.ReadAt(current, cache.offset-int64(len(current))); readErr != nil {
			return rolloutObservation{}, true, readErr
		}
		changed = !bytes.Equal(current, cache.anchor)
	}
	if cache.info == nil || !os.SameFile(cache.info, info) || info.Size() < cache.offset || changed {
		cache.offset = header
		cache.turnStart = header
		cache.anchor = nil
		cache.currentTurn, cache.userTurn = "", ""
		cache.user, cache.assistant = HistoryMessage{}, HistoryMessage{}
		cache.state, cache.lastAt, cache.replyIssue = telemetry.State{}, 0, ""
	}
	cache.info = info
	if _, err = file.Seek(cache.offset, io.SeekStart); err != nil {
		return rolloutObservation{}, true, err
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, info.Size()-cache.offset), 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return rolloutObservation{}, true, err
		}
		raw, readErr := readRolloutLine(reader)
		if readErr == io.EOF {
			break
		}
		if errors.Is(readErr, errRolloutLineTooLarge) {
			_, end, scanErr := oversizedLiveIssue(ctx, file, cache.offset)
			if scanErr == io.EOF {
				break
			}
			if scanErr != nil {
				return rolloutObservation{}, true, scanErr
			}
			cache.replyIssue = "会话日志存在超限记录，当前轮次待核对"
			cache.state.Halted = true
			cache.state.Snapshot.Issue = "state_invalid"
			cache.offset = end
			cache.anchor = nil
			if _, err = file.Seek(end, io.SeekStart); err != nil {
				return rolloutObservation{}, true, err
			}
			reader = bufio.NewReaderSize(io.LimitReader(file, info.Size()-end), 64<<10)
			continue
		}
		if readErr != nil {
			return rolloutObservation{}, true, readErr
		}
		var row struct {
			Type, Timestamp string
			Payload         json.RawMessage
		}
		if json.Unmarshal(raw, &row) != nil {
			cache.replyIssue = "会话观察日志存在无效记录，当前轮次待核对"
			cache.state.Halted = true
			cache.state.Snapshot.Issue = "state_invalid"
			cache.advance(raw)
			continue
		}
		var event struct {
			Type   string
			TurnID string          `json:"turn_id"`
			Last   string          `json:"last_agent_message"`
			Error  json.RawMessage `json:"error"`
		}
		if row.Type == "event_msg" {
			if json.Unmarshal(row.Payload, &event) != nil {
				cache.replyIssue = "会话事件格式无效，当前轮次待核对"
				cache.state.Halted = true
				cache.state.Snapshot.Issue = "state_invalid"
				cache.advance(raw)
				continue
			}
			if event.Type == "task_started" {
				cache.currentTurn = event.TurnID
				cache.turnStart = cache.offset
				cache.userTurn = ""
				cache.user, cache.assistant = HistoryMessage{}, HistoryMessage{}
				cache.replyIssue = ""
			}
		}
		if row.Type == "response_item" {
			message, visible, parseErr := publicUserMessage(row.Payload)
			if parseErr != nil {
				cache.replyIssue = parseErr.Error()
			}
			if visible {
				cache.user, cache.assistant = message, HistoryMessage{}
				cache.userTurn, cache.replyIssue = cache.currentTurn, ""
			}
		}
		stamp, parseErr := time.Parse(time.RFC3339Nano, row.Timestamp)
		if parseErr == nil && stamp.UnixMilli() > 0 && !stamp.After(time.Now().Add(5*time.Minute)) {
			if err = applySessionTelemetry(&cache.state, id, row.Type, row.Payload, stamp.UnixMilli()); err != nil {
				// 完整坏记录由实时读取账本隔离；观察器继续到下一明确轮次，不能永久卡在旧行。
				cache.replyIssue = "公开消息或状态记录不完整，当前轮次待核对"
				cache.state.Halted = true
				cache.state.Snapshot.Issue = "state_invalid"
			}
			if stamp.UnixMilli() > cache.lastAt {
				cache.lastAt = stamp.UnixMilli()
			}
		}
		if event.Type == "task_complete" && !hasCompletionError(event.Error) && event.TurnID == cache.userTurn && cache.user.ID != "" {
			replies, checkErr := finalRepliesAtCompletion(ctx, file, cache.turnStart, cache.offset, event.TurnID, event.Last)
			if ctx.Err() != nil {
				return rolloutObservation{}, true, ctx.Err()
			}
			if checkErr != nil {
				cache.replyIssue = "最终回复未通过完成核验，摘要等待核对"
				cache.state.Snapshot.CompletionOutputVerified = false
			} else {
				cache.replyIssue = ""
				if len(replies) > 0 {
					cache.assistant = replies[len(replies)-1].HistoryMessage
				}
				cache.state.Snapshot.CompletionOutputVerified = len(replies) > 0
			}
		}
		cache.advance(raw)
	}
	out := rolloutObservation{UserItemID: cache.state.UserItemID, ReplyIssue: cache.replyIssue}
	// 用量结构含指针；返回独立快照，避免后续增量扫描与展示读取发生数据竞争。
	raw, _ := json.Marshal(cache.state.Snapshot)
	_ = json.Unmarshal(raw, &out.Snapshot)
	if out.Snapshot.StartedAt == 0 {
		out.Snapshot = telemetry.Snapshot{Issue: "state_invalid", LastActivityAt: cache.lastAt}
	}
	out.Snapshot.ObservedAt = time.Now().UnixMilli()
	if cache.user.ID != "" {
		out.Messages = append(out.Messages, cache.user)
	}
	if cache.assistant.ID != "" {
		out.Messages = append(out.Messages, cache.assistant)
	}
	return out, true, nil
}

func (c *Client) recentRollout(ctx context.Context, id string) ([]HistoryMessage, bool, error) {
	view, found, err := c.observeRollout(ctx, id)
	if err == nil && view.ReplyIssue != "" {
		err = errors.New(view.ReplyIssue)
	}
	return view.Messages, found, err
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

func (cache *rolloutHistory) advance(raw []byte) {
	cache.offset += int64(len(raw))
	if len(raw) > 256 {
		raw = raw[len(raw)-256:]
	}
	cache.anchor = append(cache.anchor[:0], raw...)
}
