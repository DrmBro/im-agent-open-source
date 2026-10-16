package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"im-agent/internal/redact"
	"im-agent/internal/telemetry"
)

var telemetryModelName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,95}$`)

func (client *Client) ReadRequestTelemetry(ctx context.Context, thread, path string, offset int64, user string, state telemetry.State) (telemetry.State, int64, bool, error) {
	if user == "" || state.UserItemID != "" && state.UserItemID != user {
		return state, offset, false, errors.New("运行元数据来源身份无效")
	}
	state.UserItemID = user
	if state.Halted {
		return state, offset, true, nil
	}
	file, header, err := client.openLiveRollout(thread, path)
	if err != nil {
		return state, offset, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return state, offset, false, err
	}
	if offset < header || offset > info.Size() {
		return state, offset, false, errors.New("运行元数据观察位置无效")
	}
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return state, offset, false, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	next := offset
	for lines := 0; lines < 1000 && next-offset < 4<<20; lines++ {
		if err = ctx.Err(); err != nil {
			return state, offset, false, err
		}
		raw, readErr := readRolloutLine(reader)
		if readErr == io.EOF {
			finished := state.Snapshot.EndedAt > 0 && time.Now().UnixMilli()-state.Snapshot.EndedAt >= 60_000
			return state, next, finished, nil
		}
		if readErr != nil {
			return state, offset, false, readErr
		}
		var record struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &record) != nil {
			return state, offset, false, errors.New("运行元数据日志格式无效")
		}
		next += int64(len(raw))
		stamp, parseErr := time.Parse(time.RFC3339Nano, record.Timestamp)
		if parseErr != nil || stamp.UnixMilli() <= 0 || stamp.After(time.Now().Add(5*time.Minute)) {
			continue
		}
		at := stamp.UnixMilli()
		finished, applyErr := applyRequestTelemetry(&state, thread, record.Type, record.Payload, at)
		if applyErr != nil {
			return state, offset, false, applyErr
		}
		if finished || state.Halted {
			return state, next, true, nil
		}
	}
	return state, next, false, nil
}

func telemetryActivity(state *telemetry.State, at int64, kind string) {
	if at > state.Snapshot.LastActivityAt {
		state.Snapshot.LastActivityAt, state.Snapshot.Activity = at, kind
	}
}

func haltTelemetry(state *telemetry.State) {
	state.Snapshot = telemetry.Snapshot{Issue: "identity_conflict"}
	state.Halted = true
}

func applyRequestTelemetry(state *telemetry.State, thread, kind string, raw json.RawMessage, at int64) (bool, error) {
	switch kind {
	case "event_msg":
		var event struct {
			Type   string          `json:"type"`
			TurnID string          `json:"turn_id"`
			Last   string          `json:"last_agent_message"`
			Error  json.RawMessage `json:"error"`
			Info   *struct {
				Last   *telemetry.Usage `json:"last_token_usage"`
				Window *int64           `json:"model_context_window"`
			} `json:"info"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return false, errors.New("运行元数据事件格式无效")
		}
		if event.Type == "task_started" {
			if state.Snapshot.EndedAt > 0 && event.TurnID != state.MatchedTurn {
				return true, nil
			}
			state.CurrentTurn, state.CurrentStartedAt, state.CurrentModel = event.TurnID, at, ""
			state.PendingContext = false
		}
		if state.MatchedTurn == "" {
			return false, nil
		}
		if (event.Type == "task_complete" || event.Type == "turn_aborted") && event.TurnID == state.MatchedTurn {
			if event.Type == "task_complete" && state.CurrentTurn == state.MatchedTurn && state.MatchedUserCount == 1 &&
				state.FinalCount == 1 && state.FinalID != "" && state.FinalAt > 0 && event.Last != "" &&
				storeHash(event.Last) == state.FinalFingerprint {
				if state.Snapshot.FirstOutputAt == 0 || state.FinalAt < state.Snapshot.FirstOutputAt {
					state.Snapshot.FirstOutputAt = state.FinalAt
				}
				if state.FinalAt > state.Snapshot.LastOutputAt {
					state.Snapshot.LastOutputAt = state.FinalAt
				}
				state.Snapshot.CompletionOutputVerified = true
				state.Snapshot.OutputConflict = state.UnconfirmedOutputs > 1
			}
			if state.Snapshot.EndedAt == 0 {
				state.Snapshot.EndedAt = at
			}
			state.Snapshot.Outcome = "completed"
			if event.Type == "turn_aborted" {
				state.Snapshot.Outcome = "interrupted"
			}
			if hasCompletionError(event.Error) {
				state.Snapshot.Outcome = "failed"
			}
			telemetryActivity(state, at, event.Type)
		}
		if event.Type == "token_count" {
			if state.PendingContext && state.CurrentTurn == state.MatchedTurn && event.Info != nil &&
				at >= state.Snapshot.UsageAt && at-state.Snapshot.UsageAt <= 5_000 &&
				event.Info.Window != nil && *event.Info.Window > 0 && *event.Info.Window <= 1_000_000_000 &&
				event.Info.Last.Equal(state.Snapshot.LastCall) {
				state.Snapshot.ContextWindow, state.Snapshot.ContextAt = event.Info.Window, at
			}
			state.PendingContext = false
		}
	case "turn_context":
		var event struct {
			TurnID string `json:"turn_id"`
			Model  string `json:"model"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return false, errors.New("运行元数据上下文格式无效")
		}
		if event.TurnID != "" && event.TurnID == state.CurrentTurn && telemetryModelName.MatchString(event.Model) {
			state.CurrentModel = event.Model
			if event.TurnID == state.MatchedTurn {
				state.Snapshot.Model = event.Model
			}
		}
	case "response_item":
		var item struct {
			ID, Type, Role, Phase string
			Content               []InputContent
			Meta                  struct {
				TurnID string `json:"turn_id"`
			} `json:"internal_chat_message_metadata_passthrough"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return false, errors.New("运行元数据条目格式无效")
		}
		actual := item.Meta.TurnID
		if actual == "" {
			actual = state.CurrentTurn
		}
		if item.Type == "message" && item.Role == "user" {
			if item.ID == state.UserItemID {
				state.MatchedUserCount++
				if actual == "" || state.MatchedTurn != "" && state.MatchedTurn != actual {
					haltTelemetry(state)
					return true, nil
				}
				state.MatchedTurn = actual
				state.Snapshot.TurnID = actual
				if actual == state.CurrentTurn {
					if state.Snapshot.StartedAt == 0 {
						state.Snapshot.StartedAt = state.CurrentStartedAt
					}
					if state.CurrentModel != "" {
						state.Snapshot.Model = state.CurrentModel
					}
				}
				telemetryActivity(state, at, "started")
			} else if state.MatchedTurn != "" && (actual == state.MatchedTurn || state.CurrentTurn == state.MatchedTurn) && state.Snapshot.EndedAt == 0 {
				haltTelemetry(state)
				return true, nil
			}
		}
		if state.MatchedTurn == "" || state.Snapshot.EndedAt > 0 {
			return false, nil
		}
		if item.Type == "message" && item.Role == "assistant" && (item.Phase == "commentary" || item.Phase == "final_answer") {
			if item.Phase == "final_answer" && state.CurrentTurn == state.MatchedTurn {
				state.FinalCount++
				if text, supported := exactReplyText(item.Content); supported && item.ID != "" {
					state.FinalID, state.FinalFingerprint, state.FinalAt = item.ID, storeHash(text), at
				}
			}
			if actual == state.MatchedTurn && item.ID != "" {
				if text, supported := exactReplyText(item.Content); supported && text != "" {
					state.Snapshot.Preview = publicPreview(redact.New().Text(text))
					state.Snapshot.PreviewAt = at
				}
				if state.Snapshot.FirstOutputAt == 0 || at < state.Snapshot.FirstOutputAt {
					state.Snapshot.FirstOutputAt = at
				}
				if at > state.Snapshot.LastOutputAt {
					state.Snapshot.LastOutputAt = at
				}
				telemetryActivity(state, at, "output")
			} else if state.CurrentTurn == state.MatchedTurn {
				state.UnconfirmedOutputs++
				state.Snapshot.OutputConflict = true
			}
		}
		if actual == state.MatchedTurn {
			switch item.Type {
			case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "web_search_call":
				telemetryActivity(state, at, "tool")
			}
		}
	case "token_usage_record":
		state.PendingContext = false
		var event struct {
			ThreadID   string           `json:"thread_id"`
			TurnID     string           `json:"turn_id"`
			ResponseID string           `json:"response_id"`
			Usage      *telemetry.Usage `json:"usage"`
			TurnUsage  *telemetry.Usage `json:"turn_token_usage"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return false, errors.New("运行元数据用量格式无效")
		}
		if state.MatchedTurn == "" || event.ThreadID != thread || event.TurnID != state.MatchedTurn || event.ResponseID == "" || at < state.Snapshot.UsageAt {
			return false, nil
		}
		if !event.Usage.Valid() || !event.TurnUsage.Valid() {
			state.Snapshot.Issue = "invalid_usage"
			return false, nil
		}
		state.Snapshot.LastCall, state.Snapshot.TurnUsage, state.Snapshot.UsageAt = event.Usage, event.TurnUsage, at
		state.Snapshot.ContextWindow, state.Snapshot.ContextAt = nil, 0
		state.Snapshot.Issue = ""
		state.PendingContext = true
		if state.Snapshot.EndedAt == 0 {
			telemetryActivity(state, at, "usage")
		}
	}
	return false, nil
}
