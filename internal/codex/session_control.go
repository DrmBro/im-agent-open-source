package codex

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	case "thread/read", "thread/resume", "thread/turns/list", "turn/start", "turn/interrupt", "turn/steer":
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
		code, _ := result["code"].(float64)
		reason, _ := result["reason"].(string)
		detail := "桌面执行服务拒绝会话控制请求"
		switch reason {
		case "not_found":
			detail = "目标会话不存在或已被移除"
		case "invalid_params":
			detail = "桌面拒绝会话控制参数"
		case "authentication":
			detail = "桌面登录或授权状态不可用"
		}
		return nil, diagnostics.UserError(fmt.Sprintf("%s（code=%d）", detail, int(code)))
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
			if item.Type == "agentMessage" && publicAssistantPhase(item.Phase) {
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
	if hooks.Model != "" && (!telemetryModelName.MatchString(hooks.Model) || hooks.Effort == "") {
		return "", diagnostics.UserError("模型或思考程度无效，本条未提交")
	}
	method, activeTurn := "turn/start", ""
	if hooks.AllowSteer {
		var err error
		activeTurn, err = c.ActiveControlTurn(ctx, thread)
		if err != nil {
			return "", err
		}
		if activeTurn != "" {
			method = "turn/steer"
		}
	}
	if activeTurn == "" {
		if err := c.waitSessionReady(ctx, thread, hooks); err != nil {
			return "", err
		}
	}
	path, offset := findDesktopRollout(c.runtimeConfig.Codex.Home, thread)
	if hooks.BeforeSubmit != nil {
		if err := hooks.BeforeSubmit(path, offset); err != nil {
			return "", err
		}
	}
	params := map[string]any{"threadId": thread, "input": []map[string]any{{"type": "text", "text": text}}}
	if activeTurn != "" {
		params["expectedTurnId"] = activeTurn
	}
	if hooks.Model != "" && activeTurn == "" {
		params["model"], params["effort"] = hooks.Model, hooks.Effort
	}
	raw, e := c.sessionControlCall(ctx, method, params)
	if e != nil {
		var known diagnostics.UserError
		if errors.As(e, &known) {
			return "", known
		}
		return "", &SessionControlUnconfirmed{}
	}
	var response struct {
		TurnID string `json:"turnId"`
		Turn   struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return "", &SessionControlUnconfirmed{}
	}
	if activeTurn != "" {
		if response.TurnID != activeTurn {
			return "", &SessionControlUnconfirmed{}
		}
		response.Turn.ID = response.TurnID
	}
	if response.Turn.ID == "" {
		return "", &SessionControlUnconfirmed{}
	}
	if !c.desktopMode() || hooks.ExternalObserver {
		return response.Turn.ID, nil
	}
	watchID := "desktop-" + storeHash(thread + response.Turn.ID + time.Now().String())[:16]
	watchCtx, cancel := context.WithCancel(ctx)
	c.desktopTurnsMu.Lock()
	if old := c.desktopTurns[thread]; old != nil {
		old()
	}
	c.desktopTurns[thread] = cancel
	c.desktopTurnsMu.Unlock()
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
