package codex

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"im-agent/internal/diagnostics"
)

//go:embed desktop_archive_request.js
var desktopArchiveScript string
var archiveThreadID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type ArchiveSendResult struct {
	State  string // confirmed / rejected / unknown：只有confirmed可作为成功事实。
	Detail string
}

// ClosedChatArchivePlan持有同一连接，防止探测和执行静默切换到另一桌面。
// 只能由桥接在群已明确解散、归档尝试已持久化之后调用Send。
type ClosedChatArchivePlan struct {
	client  *Client
	desktop *cdpClient
	thread  string
	route   string
}

func (p *ClosedChatArchivePlan) Route() string { return p.route }
func (p *ClosedChatArchivePlan) Close() {
	if p.desktop != nil {
		p.desktop.close()
	}
}

func (c *cdpClient) archiveRequest(ctx context.Context, method string, params any) (json.RawMessage, string, error) {
	if method != "thread/loaded/list" && method != "thread/read" && method != "thread/archive" {
		return nil, "not_sent", errors.New("不允许的归档通道方法")
	}
	if e := ctx.Err(); e != nil {
		return nil, "not_sent", e
	}
	var nonce [16]byte
	if _, e := rand.Read(nonce[:]); e != nil {
		return nil, "not_sent", e
	}
	request, e := json.Marshal(map[string]any{"id": "im-agent-archive-" + hex.EncodeToString(nonce[:]), "method": method, "params": params})
	if e != nil {
		return nil, "not_sent", e
	}
	value, e := c.evaluate(ctx, "("+desktopArchiveScript+")("+string(request)+")")
	if e != nil {
		return nil, "unknown", e
	}
	v, ok := value.(map[string]any)
	if !ok {
		return nil, "unknown", errors.New("桌面归档回执格式未确认")
	}
	state, _ := v["state"].(string)
	switch state {
	case "completed":
		result, exists := v["result"]
		if !exists {
			return nil, "unknown", errors.New("桌面缺少方法回执")
		}
		raw, e := json.Marshal(result)
		return raw, state, e
	case "not_sent":
		return nil, state, diagnostics.UserError("当前桌面版本没有可用的归档兼容入口，尚未发送归档")
	case "rpc_error":
		code, ok := v["code"].(float64)
		if !ok || code != float64(int(code)) {
			return nil, "unknown", errors.New("桌面归档错误回执无效")
		}
		return nil, state, &RPCError{Method: method, Code: int(code), Message: "桌面内部兼容入口拒绝请求"}
	default:
		return nil, "unknown", diagnostics.UserError("桌面归档回执未确认，不重复发送或换通道")
	}
}
func (c *cdpClient) archiveTargetLoaded(ctx context.Context, thread string) (bool, error) {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 30; page++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, _, e := c.archiveRequest(ctx, "thread/loaded/list", params)
		if e != nil {
			return false, e
		}
		var v struct {
			Data []string `json:"data"`
			Next *string  `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Data == nil {
			return false, errors.New("桌面已加载会话列表无效，未推断目标不在桌面")
		}
		for _, id := range v.Data {
			if id == thread {
				return true, nil
			}
		}
		if v.Next == nil || *v.Next == "" {
			return false, nil
		}
		cursor = *v.Next
		if len(cursor) > 8192 || seen[cursor] {
			return false, errors.New("桌面会话列表游标异常")
		}
		seen[cursor] = true
	}
	return false, errors.New("桌面会话列表未完整读取，未推断归档路由")
}
func (c *cdpClient) archiveTargetIdle(ctx context.Context, thread string) error {
	raw, _, e := c.archiveRequest(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
	if e != nil {
		return e
	}
	var v struct {
		Thread Thread `json:"thread"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Thread.ID != thread {
		return errors.New("桌面归档目标元数据不匹配")
	}
	if v.Thread.Status.Type != "idle" {
		return diagnostics.UserError("目标会话正在执行或桌面状态未确认，尚未发送归档；等待任务结束后重试，不中断桌面任务")
	}
	return nil
}
func (c *Client) PrepareClosedChatArchive(ctx context.Context, thread string) (*ClosedChatArchivePlan, error) {
	if !archiveThreadID.MatchString(thread) {
		return nil, errors.New("归档目标会话ID无效")
	}
	if e := c.checkArchiveDescendants(ctx, thread); e != nil {
		return nil, e
	}
	plan := &ClosedChatArchivePlan{client: c, thread: thread, route: "independent"}
	if c.runtimeConfig.Codex.SubmissionMode != "native_queue" && c.runtimeConfig.Codex.SubmissionMode != "desktop_queue" {
		return plan, nil
	}
	desktop, e := discoverDesktopCDP(ctx, c.runtimeConfig.Codex.CdpPort)
	if e != nil {
		return nil, diagnostics.UserError("群已解散，但桌面归档入口不可用。归档仍待处理，未猜测会话归属或强制关闭桌面")
	}
	plan.desktop = desktop
	loaded, e := desktop.archiveTargetLoaded(ctx, thread)
	if e != nil {
		plan.Close()
		return nil, fmt.Errorf("无法确认桌面归档目标，未发送：%s", diagnostics.Error(e))
	}
	if loaded {
		if e = desktop.archiveTargetIdle(ctx, thread); e != nil {
			plan.Close()
			return nil, e
		}
		plan.route = "desktop_compat"
	}
	return plan, nil
}
func (p *ClosedChatArchivePlan) Send(ctx context.Context) ArchiveSendResult {
	if ctx.Err() != nil {
		return ArchiveSendResult{"rejected", "归档发送前已停止，未发出请求"}
	}
	if p.desktop != nil {
		// 预留持久化可能耗时，发送前重新确认归属；不在同一次尝试中换路由。
		loaded, e := p.desktop.archiveTargetLoaded(ctx, p.thread)
		if e != nil {
			return ArchiveSendResult{"rejected", "发送前目标核对失败，尚未发出归档"}
		}
		if loaded != (p.route == "desktop_compat") {
			return ArchiveSendResult{"rejected", "目标会话归属已变化，尚未发出归档"}
		}
	}
	if e := p.client.checkArchiveDescendants(ctx, p.thread); e != nil {
		return ArchiveSendResult{"rejected", diagnostics.Error(e)}
	}
	if p.route == "desktop_compat" {
		if e := p.desktop.archiveTargetIdle(ctx, p.thread); e != nil {
			return ArchiveSendResult{"rejected", diagnostics.Error(e)}
		}
		raw, state, e := p.desktop.archiveRequest(ctx, "thread/archive", map[string]any{"threadId": p.thread})
		if state == "not_sent" {
			return ArchiveSendResult{"rejected", diagnostics.Error(e)}
		}
		if e != nil {
			return archiveRPCOutcome(e)
		}
		var result map[string]json.RawMessage
		if json.Unmarshal(raw, &result) != nil || result == nil {
			return ArchiveSendResult{"unknown", "桌面归档成功回执结构未确认，仅等待归档事实"}
		}
		return ArchiveSendResult{"confirmed", "桌面所属服务已返回归档成功回执"}
	}
	_, e := p.client.Call(ctx, "thread/archive", map[string]any{"threadId": p.thread})
	if e != nil {
		return archiveRPCOutcome(e)
	}
	return ArchiveSendResult{"confirmed", "独立App Server已返回归档成功回执"}
}
func archiveRPCOutcome(e error) ArchiveSendResult {
	var rpc *RPCError
	if errors.As(e, &rpc) && (rpc.Code == -32600 || rpc.Code == -32601 || rpc.Code == -32602) {
		return ArchiveSendResult{"rejected", fmt.Sprintf("归档请求被明确拒绝（code=%d），保持待处理；未强制解除占用", rpc.Code)}
	}
	return ArchiveSendResult{"unknown", "归档请求可能已发出，但未取得明确回执。仅核对归档状态，不自动重发或改用其他通道"}
}

// 归档API可能级联处理派生线程；自动群归档不扩大为子线程批量操作。
// 只读SQLite元数据列表，不扫描修补历史、不创建或加载会话。
func (c *Client) checkArchiveDescendants(ctx context.Context, thread string) error {
	raw, e := c.Call(ctx, "thread/list", map[string]any{
		"ancestorThreadId": thread, "archived": false, "useStateDbOnly": true, "limit": 1,
		"modelProviders": []string{},
		"sourceKinds":    []string{"cli", "vscode", "exec", "appServer", "unknown", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther"},
	})
	if e != nil {
		return diagnostics.UserError("无法确认归档对子会话的影响范围，尚未发送；请在桌面核对")
	}
	var v struct {
		Data []json.RawMessage `json:"data"`
		Next *string           `json:"nextCursor"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Data == nil {
		return diagnostics.UserError("归档范围查询返回无效，未发送归档")
	}
	if len(v.Data) > 0 || v.Next != nil && *v.Next != "" {
		return diagnostics.UserError("该会话存在未归档的派生子会话，自动归档可能产生连带影响；请在桌面确认后手动归档")
	}
	return nil
}
