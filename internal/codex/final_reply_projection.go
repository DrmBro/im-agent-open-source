package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// 在明确的完成事件处提取最终回复，不把缺少 phase 的过程消息提前入聊天账本。
// 使用 ReadAt 的独立视图，不回退或改变实时读取器的游标。
func finalRepliesAtCompletion(ctx context.Context, file *os.File, header, end int64, turn, last string) ([]LiveMessage, error) {
	if turn == "" || end < header {
		return nil, errors.New("完成事件缺少轮次或有效边界")
	}
	start := header
	if end-start > 32<<20 {
		start = end - (32 << 20)
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(file, start, end-start), 64<<10)
	position := start
	if start > header {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		position += int64(len(line))
	}
	current := ""
	starts := 0
	ids := map[string]bool{}
	finals := []LiveMessage{}
	unphased := []LiveMessage{}
	for lines := 0; position < end; lines++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if lines >= 20000 {
			return nil, errors.New("最终回复核验超过单轮读取上限")
		}
		raw, err := readRolloutLine(reader)
		if err != nil {
			return nil, err
		}
		offset := position
		position += int64(len(raw))
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &record) != nil {
			return nil, errors.New("最终回复核验范围包含无效记录")
		}
		if record.Type == "event_msg" {
			var event struct {
				Type   string `json:"type"`
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(record.Payload, &event) != nil {
				return nil, errors.New("轮次事件无效")
			}
			if event.Type == "task_started" {
				current = event.TurnID
				if current == turn {
					starts++
				}
			}
			continue
		}
		if record.Type != "response_item" {
			continue
		}
		var item struct {
			ID, Type, Role, Phase, Status string
			Content                       []InputContent
			Meta                          struct {
				TurnID string `json:"turn_id"`
			} `json:"internal_chat_message_metadata_passthrough"`
		}
		if json.Unmarshal(record.Payload, &item) != nil {
			return nil, errors.New("回复条目无效")
		}
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		actual := item.Meta.TurnID
		if actual == "" {
			actual = current
		}
		if actual != turn || item.ID == "" {
			continue
		}
		if !completedPublicReply(item.Phase, item.Status) {
			continue
		}
		if item.Phase == "" && item.Meta.TurnID != turn {
			continue
		}
		text, ok := exactReplyText(item.Content)
		if !ok {
			continue
		}
		if ids[item.ID] {
			return nil, errors.New("最终回复条目标识重复，未擅自合并")
		}
		ids[item.ID] = true
		message := LiveMessage{HistoryMessage: HistoryMessage{ID: item.ID, Role: "assistant", Text: text}, Offset: offset}
		if item.Phase == "final_answer" {
			finals = append(finals, message)
		} else if last != "" && text == last {
			unphased = append(unphased, message)
		}
		if len(ids) > 512 {
			return nil, errors.New("单轮回复候选过多")
		}
	}
	if starts != 1 {
		return nil, errors.New("无法完整核验该轮次的回复范围")
	}
	covered := false
	for _, reply := range finals {
		if reply.Text == last {
			covered = true
		}
	}
	if !covered {
		if len(unphased) > 1 {
			return nil, errors.New("完成正文匹配多个缺省阶段条目，未猜测最终回复")
		}
		if len(unphased) == 1 {
			finals = append(finals, unphased[0])
		}
	}
	if len(finals) == 0 && last != "" {
		return nil, errors.New("完成事件未关联到唯一最终回复")
	}
	return finals, nil
}
