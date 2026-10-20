package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"im-agent/internal/telemetry"
)

// LatestSessionTelemetry 只读已核验 session_meta 的日志尾部，不提交或恢复会话。
// 超出窗口或缺少轮次起点时保持未知，不能沿用上一轮的完成状态。
func (c *Client) LatestSessionTelemetry(ctx context.Context, thread string) (telemetry.Snapshot, string, error) {
	files, err := c.LiveRolloutFiles(ctx, thread)
	if err != nil {
		return telemetry.Snapshot{}, "", err
	}
	if len(files) == 0 {
		return telemetry.Snapshot{}, "", errors.New("会话日志暂不可用")
	}
	path := files[len(files)-1]
	file, header, err := c.openLiveRollout(thread, path)
	if err != nil {
		return telemetry.Snapshot{}, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return telemetry.Snapshot{}, "", err
	}
	const window = int64(16 << 20)
	start := info.Size() - window
	if start < header {
		start = header
	}
	if _, err = file.Seek(start, io.SeekStart); err != nil {
		return telemetry.Snapshot{}, "", err
	}
	reader := bufio.NewReaderSize(io.LimitReader(file, info.Size()-start), 64<<10)
	if start > header {
		// 起点可能在一条记录内部；整条丢弃，不尝试解析残缺 JSON。
		if _, err = readRolloutLine(reader); err != nil {
			return telemetry.Snapshot{}, "", err
		}
	}
	var state telemetry.State
	var lastAt int64
	for {
		if err = ctx.Err(); err != nil {
			return telemetry.Snapshot{}, "", err
		}
		raw, readErr := readRolloutLine(reader)
		if readErr == io.EOF {
			break
		} // 正在写入的半行留待下次观察。
		if readErr != nil {
			return telemetry.Snapshot{}, "", readErr
		}
		var row struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &row) != nil {
			return telemetry.Snapshot{}, "", errors.New("会话状态日志格式无效")
		}
		stamp, parseErr := time.Parse(time.RFC3339Nano, row.Timestamp)
		if parseErr != nil || stamp.UnixMilli() <= 0 || stamp.After(time.Now().Add(5*time.Minute)) {
			continue
		}
		at := stamp.UnixMilli()
		if at > lastAt {
			lastAt = at
		}
		if row.Type == "event_msg" {
			var event struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(row.Payload, &event) != nil {
				return telemetry.Snapshot{}, "", errors.New("会话状态事件格式无效")
			}
			if event.Type == "task_started" {
				state = telemetry.State{MatchedTurn: event.TurnID, CurrentTurn: event.TurnID, CurrentStartedAt: at,
					Snapshot: telemetry.Snapshot{TurnID: event.TurnID, StartedAt: at, LastActivityAt: at, Activity: "started"}}
				if event.TurnID == "" {
					state.Snapshot.Issue = "identity_conflict"
				}
			}
		}
		if row.Type == "turn_context" {
			var contextRow struct {
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(row.Payload, &contextRow) == nil && contextRow.TurnID != "" && contextRow.TurnID != state.MatchedTurn {
				// 新上下文没有对应起点时，不再展示上一轮的完成结果。
				state = telemetry.State{Snapshot: telemetry.Snapshot{Issue: "state_invalid", LastActivityAt: at}}
			}
		}
		if row.Type == "response_item" && state.MatchedTurn != "" && state.UserItemID == "" {
			var item struct{ ID, Type, Role string }
			if json.Unmarshal(row.Payload, &item) == nil && item.Type == "message" && item.Role == "user" {
				state.UserItemID = item.ID
			}
		}
		if state.MatchedTurn != "" && !state.Halted {
			if _, err = applyRequestTelemetry(&state, thread, row.Type, row.Payload, at); err != nil {
				return telemetry.Snapshot{}, "", err
			}
		}
	}
	if state.Snapshot.StartedAt == 0 {
		state.Snapshot = telemetry.Snapshot{Issue: "state_invalid", LastActivityAt: lastAt}
	}
	state.Snapshot.ObservedAt = time.Now().UnixMilli()
	return state.Snapshot, state.UserItemID, nil
}
