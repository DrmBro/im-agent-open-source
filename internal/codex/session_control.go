package codex

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"im-agent/internal/diagnostics"
	"strings"
	"time"
)

//go:embed desktop_session_control.js
var desktopSessionControlScript string

type SessionControlUnconfirmed struct{}

func (*SessionControlUnconfirmed) Error() string {
	return "会话控制结果未确认；可能已经生效，未自动重发，请刷新状态核对"
}

type ControlTurn struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Preview string `json:"preview"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *cdpClient) controlRequest(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	switch method {
	case "thread/read", "thread/turns/list", "turn/start", "turn/interrupt":
	default:
		return nil, errors.New("不支持的会话控制操作")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	raw, e := json.Marshal(map[string]any{"id": "im-agent-control-" + hex.EncodeToString(nonce[:]), "method": method, "params": params})
	if e != nil {
		return nil, e
	}
	value, e := c.evaluate(ctx, "("+desktopSessionControlScript+")("+string(raw)+")")
	if e != nil {
		return nil, &SessionControlUnconfirmed{}
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, &SessionControlUnconfirmed{}
	}
	switch result["state"] {
	case "completed":
		return json.Marshal(result["result"])
	case "unavailable", "rejected":
		return nil, diagnostics.UserError("当前桌面连接不支持会话控制，未切换其他提交通道")
	case "rpc_error":
		return nil, diagnostics.UserError("桌面拒绝会话控制请求，请检查会话状态与所选模型，未自动重发")
	default:
		return nil, &SessionControlUnconfirmed{}
	}
}
func (c *Client) desktopMode() bool {
	return c.runtimeConfig.Codex.SubmissionMode == "native_queue" || c.runtimeConfig.Codex.SubmissionMode == "desktop_queue"
}
func (c *Client) sessionControlCall(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if !c.desktopMode() {
		return c.Call(ctx, method, params)
	}
	endpoint, e := discoverDesktopCDP(ctx, c.runtimeConfig.Codex.CdpPort)
	if e != nil {
		return nil, e
	}
	defer endpoint.close()
	return endpoint.controlRequest(ctx, method, params)
}
func (c *Client) ReadControlTurn(ctx context.Context, thread, expected string) (ControlTurn, error) {
	if thread == "" || expected == "" {
		return ControlTurn{}, errors.New("尚未确认卡片对应的实际轮次")
	}
	raw, e := c.sessionControlCall(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "sortDirection": "desc", "itemsView": "full"})
	if e != nil {
		return ControlTurn{}, e
	}
	var response struct {
		Data []struct {
			ControlTurn
			Items []Item `json:"items"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Data) != 1 || response.Data[0].ID != expected {
		return ControlTurn{}, diagnostics.UserError("当前轮次已经变化，旧卡片不能控制新轮次")
	}
	turn := response.Data[0].ControlTurn
	if !c.desktopMode() {
		for _, item := range response.Data[0].Items {
			if item.Type == "agentMessage" && (item.Phase == "commentary" || item.Phase == "final_answer") {
				turn.Preview = publicPreview(item.Text)
			}
		}
	}
	switch turn.Status {
	case "inProgress", "completed", "failed", "interrupted":
	default:
		return ControlTurn{}, errors.New("轮次状态无法识别")
	}
	return turn, nil
}

// 返回true只表示目标轮次已结束；是否成功完成仍由实际状态决定。
func (c *Client) InterruptExactTurn(ctx context.Context, thread, turn string) (bool, error) {
	current, e := c.ReadControlTurn(ctx, thread, turn)
	if e != nil {
		return false, e
	}
	if current.Status != "inProgress" {
		return true, nil
	}
	if _, e = c.sessionControlCall(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}); e != nil {
		return false, e
	}
	current, e = c.ReadControlTurn(ctx, thread, turn)
	if e != nil {
		return false, &SessionControlUnconfirmed{}
	}
	return current.Status != "inProgress", nil
}
func (c *Client) selectedModelTurn(ctx context.Context, thread, text string, hooks TurnHooks) (string, error) {
	if !telemetryModelName.MatchString(hooks.Model) || hooks.Effort == "" {
		return "", diagnostics.UserError("模型或思考程度无效，本条未提交")
	}
	if c.desktopMode() {
		select {
		case desktopSubmissionGate <- struct{}{}:
			defer func() { <-desktopSubmissionGate }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	raw, e := c.sessionControlCall(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	if e != nil {
		return "", diagnostics.UserError("无法核对会话状态，本条尚未提交")
	}
	var before struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &before) != nil || before.Thread.ID != thread || before.Thread.Status.Type != "idle" {
		return "", diagnostics.UserError("会话尚未确认空闲，本条未提交；所选模型已保留，不中断正在执行的轮次")
	}
	path, offset := findDesktopRollout(c.runtimeConfig.Codex.Home, thread)
	if hooks.BeforeSubmit != nil {
		if e = hooks.BeforeSubmit(path, offset); e != nil {
			return "", e
		}
	}
	raw, e = c.sessionControlCall(ctx, "turn/start", map[string]any{"threadId": thread, "model": hooks.Model, "effort": hooks.Effort, "input": []map[string]any{{"type": "text", "text": text}}})
	if e != nil {
		var known diagnostics.UserError
		if errors.As(e, &known) {
			return "", known
		}
		return "", &SessionControlUnconfirmed{}
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Turn.ID == "" {
		return "", &SessionControlUnconfirmed{}
	}
	if !c.desktopMode() {
		return response.Turn.ID, nil
	}
	watchID := "desktop-" + storeHash(thread + response.Turn.ID + time.Now().String())[:16]
	watchCtx, cancel := context.WithCancel(ctx)
	c.nativeMu.Lock()
	if old := c.desktopTurns[thread]; old != nil {
		old()
	}
	c.desktopTurns[thread] = cancel
	c.nativeMu.Unlock()
	go c.watchDesktopTurn(watchCtx, thread, watchID, text, path, offset, response.Turn.ID)
	return watchID, nil
}
func publicPreview(text string) string {
	r := []rune(strings.TrimSpace(text))
	if len(r) > 1000 {
		return "…" + string(r[len(r)-1000:])
	}
	return string(r)
}

// 只对桥接自身的观察引用发结束通知，不创建轮次、不取消其它观察。
// 调用者必须先用 ReadControlTurn 核验同一实际轮次的终态。
func (c *Client) NotifyObservedTurnFailure(thread, reference, status string) {
	if !strings.HasPrefix(reference, "desktop-") {
		return
	}
	switch status {
	case "failed":
		c.sendDesktopTurnFailure(thread, reference, "桌面已确认本轮响应失败，可能已有部分操作完成")
	case "interrupted":
		c.sendDesktopTurnFailure(thread, reference, "桌面已确认本轮中断，已执行操作不会回滚")
	}
}
