package codex

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"

	"im-agent/internal/diagnostics"
)

//go:embed desktop_thread_request.js
var desktopThreadRequestScript string

// 创建请求已发出但未取得回执时不能自动换通道或再创建一份。
type DesktopCreationUnconfirmed struct{}

func (*DesktopCreationUnconfirmed) Error() string {
	return "桌面会话创建或保存结果未确认，可能已创建；未自动重建或提交首问，请先查看桌面和创建记录"
}

func (c *cdpClient) threadRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method != "thread/start" && method != "thread/name/set" {
		return nil, errors.New("不允许通过桌面创建入口调用此方法")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	request, err := json.Marshal(map[string]any{"id": "im-agent-create-" + hex.EncodeToString(nonce[:]), "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	value, err := c.evaluate(ctx, "("+desktopThreadRequestScript+")("+string(request)+")")
	if err != nil {
		return nil, &DesktopCreationUnconfirmed{}
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, &DesktopCreationUnconfirmed{}
	}
	switch result["state"] {
	case "completed":
		return json.Marshal(result["result"])
	case "unavailable", "rejected":
		return nil, diagnostics.UserError("当前桌面版本不支持此创建入口，未改用辅助进程创建；请检查桌面版本")
	case "rpc_error":
		code, _ := result["code"].(float64)
		return nil, &RPCError{Method: method, Code: int(code), Message: "桌面拒绝创建或保存会话"}
	default:
		return nil, &DesktopCreationUnconfirmed{}
	}
}
