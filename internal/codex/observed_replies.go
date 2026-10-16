package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type ReplyLocation struct {
	ItemID      string `json:"item_id"`
	Offset      int64  `json:"offset"`
	Fingerprint string `json:"fingerprint"`
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
			Type    string `json:"type"`
			Payload struct {
				ID, Type, Role, Phase string
				Content               []InputContent
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &record) != nil || record.Type != "response_item" || record.Payload.Type != "message" ||
			record.Payload.Role != "assistant" || record.Payload.Phase != "final_answer" || record.Payload.ID != location.ItemID {
			return nil, errors.New("日志位置已变化，无法重读原回复")
		}
		text, supported := exactReplyText(record.Payload.Content)
		if !supported || storeHash(text) != location.Fingerprint {
			return nil, errors.New("原回复指纹不一致，未使用替代正文")
		}
		replies = append(replies, HistoryMessage{ID: location.ItemID, Role: "assistant", Text: text})
	}
	return replies, nil
}
