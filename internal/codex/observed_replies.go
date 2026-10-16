package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

type ReplyLocation struct {
	Timestamp        int64  `json:"timestamp,omitempty"`
	TurnID           string `json:"turn_id,omitempty"`
	CompletionOffset int64  `json:"completion_offset,omitempty"`
	ItemID           string `json:"item_id"`
	Offset           int64  `json:"offset"`
	Fingerprint      string `json:"fingerprint"`
}

func exactReplyText(content []InputContent) (string, bool) {
	var pieces []string
	for _, part := range content {
		switch part.Type {
		case "input_text", "output_text", "text":
			pieces = append(pieces, part.Text)
		default:
			return "", false
		}
	}
	text := strings.Join(pieces, "\n")
	return text, strings.TrimSpace(text) != ""
}

func (client *Client) ReadObservedReplies(ctx context.Context, thread, path string, locations []ReplyLocation) ([]HistoryMessage, error) {
	if len(locations) == 0 || len(locations) > 512 {
		return nil, errors.New("回复位置清单不完整")
	}
	file, header, err := client.openLiveRollout(thread, path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	seen := make(map[string]bool)
	var replies []HistoryMessage
	for _, location := range locations {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if location.ItemID == "" || seen[location.ItemID] || location.Offset < header || len(location.Fingerprint) != 64 {
			return nil, errors.New("回复位置或身份冲突，未重读替代消息")
		}
		seen[location.ItemID] = true
		if _, err = file.Seek(location.Offset, io.SeekStart); err != nil {
			return nil, err
		}
		raw, readErr := readRolloutLine(bufio.NewReaderSize(file, 64<<10))
		if readErr != nil {
			return nil, readErr
		}
		var record struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				ID, Type, Role, Phase, Status string
				Meta                          struct {
					TurnID string `json:"turn_id"`
				} `json:"internal_chat_message_metadata_passthrough"`
				Content []InputContent
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &record) != nil || record.Type != "response_item" || record.Payload.Type != "message" ||
			record.Payload.Role != "assistant" || !completedPublicReply(record.Payload.Phase, record.Payload.Status) || record.Payload.ID != location.ItemID {
			return nil, errors.New("日志位置已变化，无法重读原回复")
		}
		text, supported := exactReplyText(record.Payload.Content)
		if !supported || storeHash(text) != location.Fingerprint {
			return nil, errors.New("原回复指纹不一致，未使用替代正文")
		}
		if record.Payload.Phase == "" {
			stamp, parseErr := time.Parse(time.RFC3339Nano, record.Timestamp)
			if parseErr != nil || stamp.UnixMilli() != location.Timestamp {
				return nil, errors.New("回复时间证据已变化")
			}
			if location.TurnID == "" || record.Payload.Meta.TurnID != location.TurnID || location.CompletionOffset < location.Offset+int64(len(raw)) {
				return nil, errors.New("缺省阶段回复没有明确完成依据")
			}
			if _, err = file.Seek(location.CompletionOffset, io.SeekStart); err != nil {
				return nil, err
			}
			completionRaw, readErr := readRolloutLine(bufio.NewReaderSize(file, 64<<10))
			if readErr != nil {
				return nil, readErr
			}
			var completion struct {
				Type    string `json:"type"`
				Payload struct {
					Type   string          `json:"type"`
					TurnID string          `json:"turn_id"`
					Last   string          `json:"last_agent_message"`
					Error  json.RawMessage `json:"error"`
				} `json:"payload"`
			}
			if json.Unmarshal(completionRaw, &completion) != nil || completion.Type != "event_msg" || completion.Payload.Type != "task_complete" || completion.Payload.TurnID != location.TurnID || hasCompletionError(completion.Payload.Error) || storeHash(completion.Payload.Last) != location.Fingerprint {
				return nil, errors.New("完成事件与缺省阶段回复原文不一致")
			}
		}
		replies = append(replies, HistoryMessage{ID: location.ItemID, Role: "assistant", Text: text})
	}
	return replies, nil
}
