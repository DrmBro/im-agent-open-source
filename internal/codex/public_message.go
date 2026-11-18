package codex

import (
	"encoding/json"
	"errors"
	"strings"
)

var errMissingPublicItemID = errors.New("公开消息缺少原始条目标识")

// 公开用户消息统一保留图片占位、过滤内部上下文，并以原始 item_id 关联各展示出口。
func publicUserMessage(payload json.RawMessage) (HistoryMessage, bool, error) {
	var item struct {
		Type, Role, ID string
		Content        []InputContent
	}
	if json.Unmarshal(payload, &item) != nil {
		return HistoryMessage{}, false, errors.New("公开消息格式无效")
	}
	if item.Type != "message" || item.Role != "user" {
		return HistoryMessage{}, false, nil
	}
	var parts []string
	for _, part := range item.Content {
		switch part.Type {
		case "input_text", "output_text", "text":
			parts = append(parts, part.Text)
		case "input_image", "image", "localImage":
			parts = append(parts, "[图片，附件未同步]")
		}
	}
	body := strings.TrimSpace(strings.Join(parts, "\n"))
	if body == "" || !visibleUserText(body) {
		return HistoryMessage{}, false, nil
	}
	if item.ID == "" {
		return HistoryMessage{}, false, errMissingPublicItemID
	}
	return HistoryMessage{ID: item.ID, Role: "user", Text: body}, true, nil
}
