package dingtalk

import (
	"encoding/json"
	"errors"
	"strings"

	"im-agent/internal/config"
	"im-agent/internal/im"
)

func (c *Client) decodeMessage(raw []byte) (im.Message, bool, error) {
	var data struct {
		ConversationID   string `json:"conversationId"`
		ConversationType string `json:"conversationType"`
		MsgID            string `json:"msgId"`
		SenderStaffID    string `json:"senderStaffId"`
		SenderNick       string `json:"senderNick"`
		SenderCorpID     string `json:"senderCorpId"`
		ChatbotCorpID    string `json:"chatbotCorpId"`
		RobotCode        string `json:"robotCode"`
		IsInAtList       bool   `json:"isInAtList"`
		MsgType          string `json:"msgtype"`
		Text             struct {
			Content string `json:"content"`
		} `json:"text"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return im.Message{}, false, errors.New("钉钉消息格式无效")
	}
	if data.ConversationType != "2" || !data.IsInAtList || data.MsgType != "text" {
		return im.Message{}, false, nil
	}
	if data.RobotCode != "" && data.RobotCode != c.robot {
		return im.Message{}, false, nil
	}
	if data.SenderCorpID == "" || data.ChatbotCorpID == "" || data.SenderCorpID != data.ChatbotCorpID {
		return im.Message{}, false, nil
	}
	if data.ConversationID == "" || len(data.ConversationID) > 512 || data.MsgID == "" || len(data.MsgID) > 512 || data.SenderStaffID == "" || len(data.SenderStaffID) > 256 || len(data.SenderNick) > 1024 || strings.ContainsAny(data.SenderStaffID, "\x00\r\n`") {
		return im.Message{}, false, errors.New("钉钉消息缺少身份信息")
	}
	msg := im.Message{Address: c.Identity(), ID: data.MsgID, UserID: data.SenderStaffID, Text: strings.TrimSpace(data.Text.Content), SenderName: data.SenderNick}
	msg.Address.ChatID = data.ConversationID
	if isIdentityCommand(msg.Text) {
		return msg, true, nil
	}
	if !config.Contains(c.cfg.AllowedUsers, data.SenderStaffID) {
		return im.Message{}, false, nil
	}
	var fields map[string]any
	if e := json.Unmarshal(raw, &fields); e != nil {
		return im.Message{}, false, e
	}
	refs := map[string]bool{}
	var inspect func(map[string]any, int)
	inspect = func(m map[string]any, depth int) {
		if depth > 5 {
			return
		}
		for key, value := range m {
			switch key {
			case "isReply", "isReplyMsg":
				if b, ok := value.(bool); ok && b {
					msg.HasReply = true
				}
			case "originalProcessQueryKey":
				if s, ok := value.(string); ok && s != "" {
					refs["process:"+s] = true
					msg.HasReply = true
				}
			case "repliedMsg", "replyMsg":
				if v, ok := value.(map[string]any); ok {
					msg.HasReply = true
					if id, ok := v["msgId"].(string); ok && id != "" {
						refs["message:"+id] = true
					}
					inspect(v, depth+1)
				}
			default:
				if v, ok := value.(map[string]any); ok {
					inspect(v, depth+1)
				}
			}
		}
	}
	inspect(fields, 0)
	if msg.HasReply {
		var originals func(map[string]any, int)
		originals = func(m map[string]any, depth int) {
			if depth > 5 {
				return
			}
			for k, v := range m {
				if k == "originalMsgId" {
					if s, ok := v.(string); ok && s != "" {
						refs["message:"+s] = true
					}
				}
				if child, ok := v.(map[string]any); ok {
					originals(child, depth+1)
				}
			}
		}
		originals(fields, 0)
	}
	for ref := range refs {
		msg.ReplyToIDs = append(msg.ReplyToIDs, ref)
	}
	return msg, true, nil
}
