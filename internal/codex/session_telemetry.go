package codex

import (
	"context"
	"encoding/json"
	"errors"

	"im-agent/internal/redact"
	"im-agent/internal/telemetry"
)

// 当前会话状态与最新对话复用同一日志文件、完整行游标和完成核验。
func (c *Client) LatestSessionTelemetry(ctx context.Context, thread string) (telemetry.Snapshot, string, error) {
	view, found, err := c.observeRollout(ctx, thread)
	if err == nil && !found {
		err = errors.New("会话日志暂不可用")
	}
	return view.Snapshot, view.UserItemID, err
}

func applySessionTelemetry(state *telemetry.State, thread, kind string, raw json.RawMessage, at int64) error {
	if kind == "event_msg" {
		var event struct {
			Type   string
			TurnID string `json:"turn_id"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return errors.New("会话状态事件格式无效")
		}
		if event.Type == "task_started" {
			*state = telemetry.State{MatchedTurn: event.TurnID, CurrentTurn: event.TurnID, CurrentStartedAt: at,
				Snapshot: telemetry.Snapshot{TurnID: event.TurnID, StartedAt: at, LastActivityAt: at, Activity: "started"}}
			if event.TurnID == "" {
				state.Snapshot.Issue = "identity_conflict"
			}
		}
	}
	if kind == "turn_context" {
		var row struct {
			TurnID string `json:"turn_id"`
		}
		if json.Unmarshal(raw, &row) == nil && row.TurnID != "" && row.TurnID != state.MatchedTurn {
			*state = telemetry.State{Snapshot: telemetry.Snapshot{Issue: "state_invalid", LastActivityAt: at}}
		}
	}
	if kind == "response_item" {
		var item struct{ Type, Role string }
		if json.Unmarshal(raw, &item) != nil {
			return errors.New("会话状态消息格式无效")
		}
		if item.Type == "message" && item.Role == "user" {
			message, visible, err := publicUserMessage(raw)
			if err != nil {
				return err
			}
			if !visible {
				return nil
			}
			// 会话视图允许本轮追加输入；单条 IM 请求仍使用自身的严格观察器。
			if state.MatchedTurn != "" {
				state.UserItemID = message.ID
				state.MatchedUserCount = 0
			}
		}
	}
	if state.MatchedTurn != "" && !state.Halted {
		if _, err := applyRequestTelemetry(state, thread, kind, raw, at); err != nil {
			return err
		}
	}
	if kind == "response_item" && state.MatchedTurn != "" && !state.Halted && state.Snapshot.EndedAt == 0 && state.MatchedUserCount == 1 {
		var item struct {
			ID, Type, Role, Phase string
			Content               []InputContent
			Meta                  struct {
				TurnID string `json:"turn_id"`
			} `json:"internal_chat_message_metadata_passthrough"`
		}
		if json.Unmarshal(raw, &item) == nil && item.ID != "" && item.Type == "message" && item.Role == "assistant" && publicAssistantPhase(item.Phase) {
			if item.Meta.TurnID != "" && item.Meta.TurnID != state.MatchedTurn {
				if text, ok := exactReplyText(item.Content); ok {
					state.Snapshot.Preview = publicPreview(redact.New().Text(text))
					state.Snapshot.PreviewAt = at
					state.Snapshot.PreviewScope = "session"
				}
			} else if state.Snapshot.PreviewAt == at {
				state.Snapshot.PreviewScope = ""
			}
		}
	}
	return nil
}
