package codex

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"im-agent/internal/diagnostics"
	"im-agent/internal/drafts"
)

//go:embed desktop_guard.js
var desktopGuardScript string

//go:embed desktop_draft_guard.js
var desktopDraftGuardScript string

// 一个桌面输入设备只有一份焦点；跨话题准备/输入/发送必须串行。
var desktopSubmissionGate = make(chan struct{}, 1)

type cdpTarget struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	WebSocket string `json:"webSocketDebuggerUrl"`
}
type cdpClient struct {
	ws   *websocket.Conn
	next atomic.Uint64
}

func discoverDesktopCDP(ctx context.Context, preferred int) (*cdpClient, error) {
	ports := []int{}
	if preferred > 0 {
		ports = append(ports, preferred)
	}
	for p := 9222; p <= 9235; p++ {
		if p != preferred {
			ports = append(ports, p)
		}
	}
	httpClient := &http.Client{Timeout: 700 * time.Millisecond}
	for _, port := range ports {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/json", nil)
		if err != nil {
			continue
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			continue
		}
		var targets []cdpTarget
		err = json.NewDecoder(resp.Body).Decode(&targets)
		resp.Body.Close()
		if err != nil {
			continue
		}
		sort.SliceStable(targets, func(i, j int) bool {
			score := func(t cdpTarget) int {
				if t.URL == "app://-/index.html" {
					return 3
				}
				if strings.HasPrefix(t.URL, "app://-/") {
					return 2
				}
				if strings.Contains(strings.ToLower(t.Title), "codex") {
					return 1
				}
				return 0
			}
			return score(targets[i]) > score(targets[j])
		})
		for _, target := range targets {
			if target.WebSocket != "" && (target.URL == "app://-/index.html" || strings.HasPrefix(target.URL, "app://-/")) {
				ws, _, err := websocket.DefaultDialer.DialContext(ctx, target.WebSocket, nil)
				if err == nil {
					return &cdpClient{ws: ws}, nil
				}
			}
		}
	}
	// 尚未取得端点，sendPrompt根本没有调用；这是确认未发送，不是回执不确定。
	return nil, diagnostics.UserError("未发现可连接的 Codex 桌面入口，本条未发送。请按此前 Codex++ 方式启动桌面客户端并开启本机连接入口，再提交新问题；仅重启 IM Agent 不会开启该入口。")
}
func (c *cdpClient) close() {
	if c != nil && c.ws != nil {
		_ = c.ws.Close()
	}
}
func (c *cdpClient) command(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.ws.SetWriteDeadline(deadline)
	_ = c.ws.SetReadDeadline(deadline)
	id := c.next.Add(1)
	if err := c.ws.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		var msg struct {
			ID     uint64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  any             `json:"error"`
		}
		if err := c.ws.ReadJSON(&msg); err != nil {
			return nil, err
		}
		if msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("CDP %s error", method)
		}
		return msg.Result, nil
	}
}

func (c *cdpClient) evaluate(ctx context.Context, expression string) (any, error) {
	raw, err := c.command(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true})
	if err != nil {
		return nil, err
	}
	var result struct {
		Exception json.RawMessage `json:"exceptionDetails"`
		Result    struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil, errors.New("CDP Runtime.evaluate 返回格式无效")
	}
	if len(result.Exception) > 0 && string(result.Exception) != "null" {
		return nil, errors.New("桌面会话检查脚本执行异常，未继续提交")
	}
	return result.Result.Value, nil
}

func (c *cdpClient) guardValue(ctx context.Context, action, token, thread, prompt string, requireDraft ...bool) (map[string]any, error) {
	args, err := json.Marshal([]string{action, token, thread, prompt})
	if err != nil {
		return nil, err
	}
	required := len(requireDraft) > 0 && requireDraft[0]
	expression := "(" + desktopGuardScript + ")(..." + string(args) + ",null," + strconv.FormatBool(required) + ")"
	if strings.HasPrefix(action, "draft_") {
		expression = "(" + desktopGuardScript + ")(..." + string(args) + ",(" + desktopDraftGuardScript + ")," + strconv.FormatBool(required) + ")"
	}
	value, err := c.evaluate(ctx, expression)
	if err != nil {
		return nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("桌面检查响应无效，未继续提交")
	}
	return result, nil
}

func (c *cdpClient) guard(ctx context.Context, action, token, thread, prompt string, requireDraft ...bool) error {
	result, err := c.guardValue(ctx, action, token, thread, prompt, requireDraft...)
	if err != nil {
		return err
	}
	if result["ok"] == true {
		return nil
	}
	reason, _ := result["reason"].(string)
	return desktopGuardFailure(action, reason)
}

// 固定提交阶段与检查类别，用于本机持久诊断；不保存桌面文本或DOM。
type DesktopGuardFailure struct {
	Action string
	Reason string
	cause  error
}

func (e *DesktopGuardFailure) Error() string { return e.cause.Error() }
func (e *DesktopGuardFailure) Unwrap() error { return e.cause }

func desktopGuardFailure(action, reason string) error {
	switch action {
	case "prepare", "focus_ready", "ready", "receipt", "cleanup", "rollback":
	default:
		action = "guard"
	}
	category := reason
	switch reason {
	case "focus_changed", "thread_changed", "thread_unverified", "invalid_thread", "composer_detached", "composer_replaced",
		"input_target_changed", "user_interaction", "draft_state_changed", "draft_policy_lost", "submission_expired", "target_changed",
		"composer_missing", "composer_target_ambiguous", "composer_ambiguous", "composer_has_draft", "composer_unavailable",
		"insertion_unconfirmed", "composer_content_mismatch", "composer_changed", "submission_busy", "navigation_busy", "submission_unconfirmed":
	default:
		category = "unclassified_guard"
	}
	return &DesktopGuardFailure{Action: action, Reason: category, cause: desktopGuardRejection(reason)}
}

func desktopGuardRejection(reason string) error {
	switch reason {
	case "draft_policy_lost":
		return diagnostics.UserError("草稿保护状态已失效或页面已重载，未继续提交；请检查桌面和本机草稿恢复记录")
	case "thread_unverified", "invalid_thread":
		return diagnostics.UserError("未能确认桌面当前会话与此话题相同，未发送；请在 Codex 打开此话题绑定的会话")
	case "composer_missing":
		return diagnostics.UserError("尚未找到聊天输入框，本条未发送；请确认目标会话的聊天区域已加载，其他面板的输入框不会代替聊天框")
	case "composer_target_ambiguous", "composer_ambiguous":
		return diagnostics.UserError("无法确认唯一的聊天输入目标，本条未发送；已不要求整个页面只有一个输入框，但不会任选其它编辑区发送")
	case "composer_has_draft":
		return diagnostics.UserError("桌面输入框已有草稿或不可编辑，未覆盖；请先处理桌面草稿")
	case "navigation_target_missing":
		return diagnostics.UserError("没有找到此会话的已加载侧栏条目，未发送；请先在 Codex 打开该会话一次或展开其所在项目，再发送新消息")
	case "navigation_interrupted":
		return diagnostics.UserError("等待或切换期间桌面会话被手动切换，已停止自动提交；请检查桌面，不会抢回窗口或自动重发")
	case "composer_unavailable":
		return diagnostics.UserError("桌面输入框当前不可编辑，未发送")
	case "insertion_unconfirmed":
		return diagnostics.UserError("桌面未确认完整输入事件，本条未发送；请检查输入框是否残留附件说明或消息草稿")
	case "composer_content_mismatch":
		return diagnostics.UserError("桌面编辑器中的逻辑文本与待发内容不一致，本条未发送；为保护草稿未覆盖，请检查输入框")
	case "composer_changed":
		return diagnostics.UserError("填入消息后输入框又被编辑，本条未发送；用户改动已保留，不会自动覆盖或重发")
	case "focus_changed":
		return diagnostics.UserError("目标聊天输入框焦点已变化且未能安全恢复，本条未发送；未覆盖其它输入框")
	case "thread_changed":
		return diagnostics.UserError("提交期间桌面实际会话身份发生变化，本条未发送；不会发送到其它会话")
	case "composer_detached", "composer_replaced":
		return diagnostics.UserError("桌面聊天编辑器在提交期间被重新加载或替换，本条未发送；待目标输入区稳定后再提交")
	case "input_target_changed":
		return diagnostics.UserError("原生输入事件目标不是已核对的聊天编辑器，本条未发送；未改写其它编辑区")
	case "user_interaction":
		return diagnostics.UserError("提交期间检测到用户点击、键盘或输入法操作，本条未发送；保留用户操作，不抢回焦点")
	case "draft_state_changed":
		return diagnostics.UserError("提交期间草稿备份或附件保护状态发生变化，本条未发送；原草稿保留待核对")
	case "submission_expired":
		return diagnostics.UserError("桌面输入事务已结束或页面重载，本条未发送；不会复用失效事务重发")
	case "target_changed":
		return diagnostics.UserError("旧桌面检查返回目标变化但没有细分原因，本条未发送；需核对原桌面状态")
	case "submission_busy":
		return diagnostics.UserError("桌面上一条输入仍在提交，未重复发送；请稍候检查桌面")
	case "navigation_busy":
		return diagnostics.UserError("桌面会话切换暂被占用，本条未发送；这不代表上一条消息仍在提交")
	default:
		return diagnostics.UserError("桌面会话、焦点或草稿在提交期间发生变化，已停止提交；请先检查桌面，不要直接重发")
	}
}

type desktopSendOptions struct {
	PreserveDraft    bool
	AutoRestoreDraft bool
	RequestID        string
	Drafts           *drafts.Journal
	OnDraftRetained  func()
	OpenThread       func(context.Context, string) error
	AutoSwitch       bool
	BeforeSend       func() error
	Progress         func(string, string)
}

func (c *cdpClient) sendPrompt(parent context.Context, threadID, prompt string, options desktopSendOptions) (sendErr error) {
	// 只观察原有提交锁，不新增队列、超时或发送重试。
	if options.Progress != nil {
		options.Progress("waiting_desktop_submit", "等待本机桌面提交入口；本条尚未发送，不代表模型服务端排队。")
	}
	select {
	case desktopSubmissionGate <- struct{}{}:
		defer func() { <-desktopSubmissionGate }()
	case <-parent.Done():
		return parent.Err()
	}
	if options.Progress != nil {
		options.Progress("checking_desktop", "已取得本机桌面提交入口，正在确认目标会话与输入区；本条尚未发送。")
	}
	token := storeHash(threadID + time.Now().String())
	var preservation *draftPreservation
	if options.PreserveDraft {
		preservation = &draftPreservation{client: c, options: options, token: token, thread: threadID}
	}
	enterAttempted := false
	defer func() {
		if sendErr != nil && !enterAttempted {
			// 仅撤回本次、未提交、未修改且目标仍正确的程序草稿；不清理历史遗留内容。
			rollbackCtx, stopRollback := context.WithTimeout(context.Background(), time.Second)
			_, _ = c.guardValue(rollbackCtx, "rollback", token, threadID, "", options.PreserveDraft)
			stopRollback()
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		cleanupErr := c.guard(cleanupCtx, "cleanup", token, threadID, "", options.PreserveDraft)
		if preservation != nil {
			preservation.finish(parent, cleanupErr == nil && (!enterAttempted || sendErr == nil))
		}
	}()
	var onDraft func(context.Context) (bool, error)
	if preservation != nil {
		onDraft = preservation.suspend
		// 先处理同目标会话的草稿，包含现有trim匹配无法区分的纯空白草稿。
		if _, err := onDraft(parent); err != nil {
			return err
		}
	}
	if options.AutoSwitch {
		if err := c.navigateThread(parent, token, threadID, options.Progress, options.OpenThread, onDraft); err != nil {
			return err
		}
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if preservation != nil {
		if err := preservation.ready(parent); err != nil {
			return err
		}
	}
	if options.Progress != nil {
		options.Progress("submitting", "正在提交输入；取得提交入口不等于已经发送，仍需确认发送回执。")
	}
	// 导航/草稿等待结束后才启动发送事务的短超时，保留原有输入保护。
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	prepare, err := c.guardValue(ctx, "prepare", token, threadID, prompt, options.PreserveDraft)
	if err != nil {
		return err
	}
	if prepare["ok"] != true {
		reason, _ := prepare["reason"].(string)
		if reason != "thread_unverified" || !options.AutoSwitch {
			return desktopGuardFailure("prepare", reason)
		}
		// 尚未输入时允许按精确会话 ID 再定位一次；不跳过身份、草稿或焦点保护。
		// 已触发用户手动切换时 navigateThread 会拒绝继续抢回。
		if err := c.navigateThread(parent, token, threadID, options.Progress, options.OpenThread, onDraft); err != nil {
			return err
		}
		if preservation != nil {
			if err := preservation.ready(parent); err != nil {
				return err
			}
		}
		retryCtx, stopRetry := context.WithTimeout(parent, 10*time.Second)
		defer stopRetry()
		ctx = retryCtx
		prepare, err = c.guardValue(ctx, "prepare", token, threadID, prompt, options.PreserveDraft)
		if err != nil {
			return err
		}
		if prepare["ok"] != true {
			reason, _ = prepare["reason"].(string)
			return desktopGuardFailure("prepare", reason)
		}
	}
	// 所有会话和输入区检查完成后才登记来源；重定位可能新建日志文件。
	// 输入之前仍有一次持久化故障边界，失败只清理本次空编辑器事务，不重复提交。
	if options.BeforeSend != nil {
		if err := options.BeforeSend(); err != nil {
			return err
		}
	}
	// 登记来源可能触发异步页面更新，输入之前再次核对并仅恢复同一空编辑器焦点。
	if err := c.guard(ctx, "focus_ready", token, threadID, "", options.PreserveDraft); err != nil {
		return err
	}
	if _, err := c.command(ctx, "Input.insertText", map[string]any{"text": prompt}); err != nil {
		return err
	}
	if err := c.guard(ctx, "ready", token, threadID, "", options.PreserveDraft); err != nil {
		return err
	}
	keyDown := map[string]any{"type": "keyDown", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13}
	// Enter 发出后发生通信异常，不能断言未执行，也不能自动重发。
	enterAttempted = true
	if _, err := c.command(ctx, "Input.dispatchKeyEvent", keyDown); err != nil {
		return &DesktopOutcomeUnconfirmed{}
	}
	keyUp := map[string]any{"type": "keyUp", "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13}
	if _, err := c.command(ctx, "Input.dispatchKeyEvent", keyUp); err != nil {
		return &DesktopOutcomeUnconfirmed{}
	}
	if err := c.guard(ctx, "receipt", token, threadID, "", options.PreserveDraft); err != nil {
		return &DesktopOutcomeUnconfirmed{}
	}
	return nil
}
