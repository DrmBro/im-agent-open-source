package codex

import (
	"context"
	"encoding/json"
	"errors"
)

// ActiveControlTurn 查询实际桌面执行服务；不根据本地队列猜测轮次。
func (c *Client) ActiveControlTurn(ctx context.Context, thread string) (string, error) {
	raw, err := c.sessionControlCall(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	if err != nil {
		return "", err
	}
	state, err := controlSessionState(raw, thread)
	if err != nil {
		return "", err
	}
	if state != "active" {
		return "", nil
	}
	raw, err = c.sessionControlCall(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded"})
	if err != nil {
		return "", err
	}
	var result struct {
		Data []ControlTurn `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Data) != 1 {
		return "", errors.New("当前轮次身份不可核对")
	}
	if result.Data[0].Status != "inProgress" {
		return "", nil
	}
	if result.Data[0].ID == "" {
		return "", errors.New("当前轮次缺少ID")
	}
	return result.Data[0].ID, nil
}
func (c *Client) SteerExactTurn(ctx context.Context, thread, turn, text string) error {
	if thread == "" || turn == "" || text == "" {
		return errors.New("补充消息缺少会话、轮次或正文")
	}
	raw, err := c.sessionControlCall(ctx, "turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn, "input": []map[string]any{{"type": "text", "text": text}}})
	if err != nil {
		return err
	}
	var r struct {
		TurnID string `json:"turnId"`
	}
	if json.Unmarshal(raw, &r) != nil || r.TurnID != turn {
		return &SessionControlUnconfirmed{}
	}
	return nil
}
