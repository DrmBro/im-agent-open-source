package codex

import "encoding/json"

// DesktopOutcomeUnconfirmed 只表示本地观察终止，不能推断桌面任务没有执行或已取消。
type DesktopOutcomeUnconfirmed struct{}

func (*DesktopOutcomeUnconfirmed) Error() string {
	return "桌面执行结果未确认：可能仍在排队或执行。本次只停止等待，没有确认取消桌面任务；请先检查当前会话，不要重复发送。"
}

type ThreadEvent struct {
	ThreadID, TurnID, ItemID, Text, Status, Command string
	Kind                                            string
	Raw                                             json.RawMessage
}

func DecodeThreadEvent(n Notice) (ThreadEvent, bool) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		ItemID   string `json:"itemId"`
		Delta    string `json:"delta"`
		Message  string `json:"message"`
		Status   string `json:"status"`
		Turn     Turn   `json:"turn"`
		Item     Item   `json:"item"`
	}
	if json.Unmarshal(n.Params, &p) != nil {
		return ThreadEvent{}, false
	}
	event := ThreadEvent{ThreadID: p.ThreadID, TurnID: p.TurnID, ItemID: p.ItemID, Raw: n.Params}
	if event.TurnID == "" {
		event.TurnID = p.Turn.ID
	}
	if event.ItemID == "" {
		event.ItemID = p.Item.ID
	}
	switch n.Method {
	case "item/agentMessage/delta":
		event.Kind = "text_delta"
		event.Text = p.Delta
	case "item/completed":
		if p.Item.Type != "agentMessage" || (p.Item.Phase != "" && p.Item.Phase != "final_answer") {
			return ThreadEvent{}, false
		}
		event.Kind = "item"
		event.Text = p.Item.Text
		event.Status = p.Item.Status
	case "turn/completed":
		event.Kind = "turn_completed"
		event.Status = p.Turn.Status
		if event.Status == "" {
			event.Status = p.Status
		}
		if p.Turn.Error != nil {
			event.Text = p.Turn.Error.Message
		}
	case "bridge/desktopObservationStopped":
		event.Kind = "desktop_unconfirmed"
	case "turn/failed":
		event.Kind = "turn_failed"
		event.Text = p.Message
	default:
		return ThreadEvent{}, false
	}
	return event, true
}
