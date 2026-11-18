package codex

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
)

//go:embed desktop_approvals.js
var desktopApprovalsScript string

//go:embed desktop_adapter.js
var desktopAdapterScript string

//go:embed desktop_async_questions.js
var desktopAsyncQuestionsScript string

type DesktopApproval struct {
	Kind        string                 `json:"kind,omitempty"`
	EntityKey   string                 `json:"entityKey,omitempty"`
	RequestID   json.RawMessage        `json:"requestId"`
	ThreadID    string                 `json:"threadId"`
	TurnID      string                 `json:"turnId"`
	ItemID      string                 `json:"itemId"`
	Fingerprint string                 `json:"fingerprint"`
	Command     string                 `json:"command"`
	Reason      string                 `json:"reason"`
	AllowOnce   bool                   `json:"allowOnce"`
	Questions   []DesktopInputQuestion `json:"questions,omitempty"`
}
type DesktopInputQuestion struct {
	ID       string               `json:"id"`
	Header   string               `json:"header"`
	Question string               `json:"question"`
	IsSecret bool                 `json:"isSecret"`
	Options  []DesktopInputOption `json:"options"`
}
type DesktopInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type DesktopInputAnswer struct {
	Option *int   `json:"option,omitempty"`
	Text   string `json:"text,omitempty"`
}
type DesktopApprovalSnapshot struct {
	Adapter      string            `json:"adapter,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	CoveredKinds []string          `json:"coveredKinds,omitempty"`
	State        string            `json:"state"`
	Complete     bool              `json:"complete"`
	Requests     []DesktopApproval `json:"requests"`
}

func (c *Client) desktopApprovalCall(ctx context.Context, input any) (DesktopApprovalSnapshot, error) {
	var out DesktopApprovalSnapshot
	if !c.desktopMode() {
		return out, errors.New("当前模式不使用桌面审批入口")
	}
	endpoint, err := discoverDesktopCDP(ctx, c.runtimeConfig.Codex.CdpPort)
	if err != nil {
		return out, err
	}
	defer endpoint.close()
	raw, err := json.Marshal(input)
	if err != nil {
		return out, err
	}
	value, err := endpoint.evaluate(ctx, "("+desktopApprovalsScript+")("+string(raw)+",("+desktopAsyncQuestionsScript+"),("+desktopAdapterScript+"))")
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	if out.State == "unavailable" || out.State == "" {
		return out, errors.New("桌面审批能力不可用：" + desktopCompatibilityReason(out.Reason))
	}
	return out, nil
}
func (c *Client) ReadDesktopApprovals(ctx context.Context, threads []string) (DesktopApprovalSnapshot, error) {
	return c.desktopApprovalCall(ctx, map[string]any{"action": "snapshot", "threads": threads})
}
func (c *Client) ReplyDesktopApproval(ctx context.Context, request DesktopApproval, decision string) (string, error) {
	if decision != "accept" && decision != "decline" {
		return "", errors.New("只允许本次允许或拒绝")
	}
	result, err := c.desktopApprovalCall(ctx, map[string]any{"action": "respond", "threadId": request.ThreadID, "turnId": request.TurnID, "itemId": request.ItemID, "requestId": request.RequestID, "fingerprint": request.Fingerprint, "decision": decision})
	return result.State, err
}
func (c *Client) ReplyDesktopUserInput(ctx context.Context, request DesktopApproval, answers map[string]DesktopInputAnswer) (string, error) {
	if request.Kind != "input" && request.Kind != "async_input" {
		return "", errors.New("当前请求不是提问")
	}
	result, err := c.desktopApprovalCall(ctx, map[string]any{"action": "respond", "kind": request.Kind, "entityKey": request.EntityKey, "threadId": request.ThreadID, "turnId": request.TurnID, "itemId": request.ItemID, "requestId": request.RequestID, "fingerprint": request.Fingerprint, "answers": answers})
	return result.State, err
}

func desktopCompatibilityReason(reason string) string {
	switch reason {
	case "unsupported_desktop_version":
		return "当前桌面版本尚未适配"
	case "desktop_scope_unavailable":
		return "桌面会话作用域未就绪"
	case "request_catalog_unavailable":
		return "桌面请求目录不可用"
	case "async_input_unavailable":
		return "异步提问读取暂不可用"
	default:
		return "桌面入口未就绪"
	}
}
