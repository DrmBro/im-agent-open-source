package codex

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
)

//go:embed desktop_approvals.js
var desktopApprovalsScript string

type DesktopApproval struct {
	RequestID   json.RawMessage `json:"requestId"`
	ThreadID    string          `json:"threadId"`
	TurnID      string          `json:"turnId"`
	ItemID      string          `json:"itemId"`
	Fingerprint string          `json:"fingerprint"`
	Command     string          `json:"command"`
	Reason      string          `json:"reason"`
	AllowOnce   bool            `json:"allowOnce"`
}
type DesktopApprovalSnapshot struct {
	State    string            `json:"state"`
	Complete bool              `json:"complete"`
	Requests []DesktopApproval `json:"requests"`
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
	value, err := endpoint.evaluate(ctx, "("+desktopApprovalsScript+")("+string(raw)+")")
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
		return out, errors.New("当前桌面审批入口未就绪或版本不兼容，未授予权限")
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
