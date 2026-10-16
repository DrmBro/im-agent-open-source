package codex

import (
	"context"
	"encoding/json"
	"errors"
	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"strings"
)

type InputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type Item struct {
	Content json.RawMessage `json:"content"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Phase   string          `json:"phase"`
	Command string          `json:"command"`
	Status  string          `json:"status"`
	Changes json.RawMessage `json:"changes"`
}
type Turn struct {
	ItemsView string `json:"itemsView"`
	ID        string `json:"id"`
	Status    string `json:"status"`
	Items     []Item `json:"items"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}
type Thread struct {
	ProjectID   string `json:"projectId"`
	Model       string `json:"model"`
	Effort      string `json:"reasoningEffort"`
	HistoryMode string `json:"historyMode"`
	Name        string `json:"name"`
	ID          string `json:"id"`
	Cwd         string `json:"cwd"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	Turns       []Turn `json:"turns"`
	Status      struct {
		Type string `json:"type"`
	} `json:"status"`
}

func Options(c config.Config) map[string]any {
	p := map[string]any{"sandbox": c.Codex.Sandbox, "approvalPolicy": c.Codex.ApprovalPolicy, "approvalsReviewer": "user"}
	if c.WorkDir != "" {
		p["cwd"] = c.WorkDir
	}
	if c.Codex.Mode == "custom" && c.Codex.Model != "" {
		p["model"] = c.Codex.Model
	}
	return p
}
func (c *Client) Thread(ctx context.Context, method, id string, cfg config.Config) (Thread, error) {
	p := Options(cfg)
	if method == "thread/start" {
		p["ephemeral"] = false
	}
	if method == "thread/resume" {
		p["excludeTurns"] = true
	}
	if id != "" {
		p["threadId"] = id
	}
	r, e := c.Call(ctx, method, p)
	if e != nil {
		return Thread{}, e
	}
	var v struct {
		Thread Thread `json:"thread"`
	}
	if json.Unmarshal(r, &v) != nil || v.Thread.ID == "" {
		return Thread{}, errors.New("Codex 返回的会话无效")
	}
	return v.Thread, nil
}
func (c *Client) Read(ctx context.Context, id string) (Thread, error) {
	return c.readThread(ctx, id, true)
}

// ReadMetadata 查询目录时不拉取完整历史对话。
func (c *Client) ReadMetadata(ctx context.Context, id string) (Thread, error) {
	return c.readThread(ctx, id, false)
}
func (c *Client) readThread(ctx context.Context, id string, includeTurns bool) (Thread, error) {
	r, e := c.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": includeTurns})
	if e != nil {
		return Thread{}, e
	}
	var v struct {
		Thread Thread `json:"thread"`
	}
	if json.Unmarshal(r, &v) != nil || v.Thread.ID == "" {
		return Thread{}, errors.New("读取 Codex 会话失败")
	}
	return v.Thread, nil
}
func (c *Client) List(ctx context.Context, cwd string) ([]Thread, error) {
	return c.list(ctx, cwd, false)
}
func (c *Client) Archived(ctx context.Context) ([]Thread, error) { return c.list(ctx, "", true) }
func (c *Client) list(ctx context.Context, cwd string, archived bool) ([]Thread, error) {
	var all []Thread
	cursor := ""
	for page := 0; page < 100; page++ {
		p := map[string]any{"limit": 100, "archived": archived, "modelProviders": []string{}, "sourceKinds": []string{"cli", "vscode", "exec", "appServer"}}
		if archived {
			p["sourceKinds"] = []string{"cli", "vscode", "exec", "appServer", "unknown", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther"}
		}
		if cwd != "" {
			p["cwd"] = cwd
		}
		if cursor != "" {
			p["cursor"] = cursor
		}
		raw, e := c.Call(ctx, "thread/list", p)
		if e != nil {
			return nil, e
		}
		var result struct {
			Data       []Thread `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &result) != nil {
			return nil, errors.New("会话列表格式错误")
		}
		all = append(all, result.Data...)
		if result.NextCursor == nil || *result.NextCursor == "" {
			return all, nil
		}
		cursor = *result.NextCursor
	}
	return nil, errors.New("会话列表超过分页上限，缩小扫描范围")
}
func (c *Client) Interrupt(ctx context.Context, thread, turn string) error {
	if strings.HasPrefix(turn, "desktop-") {
		// 这是桥接观察 ID，不是桌面真实 turn_id，不能传给另一个 App Server。
		c.nativeMu.Lock()
		stop := c.desktopTurns[thread]
		c.nativeMu.Unlock()
		if stop != nil {
			stop()
		}
		return &DesktopOutcomeUnconfirmed{}
	}
	_, e := c.Call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return e
}

func (c *Client) StartThread(ctx context.Context, cfg config.Config) (Thread, error) {
	workDir, err := NewSessionDirectory(cfg.StateDir, cfg.WorkDir)
	if err != nil {
		return Thread{}, err
	}
	cfg.WorkDir = workDir
	thread, err := c.Thread(ctx, "thread/start", "", cfg)
	if err != nil {
		return Thread{}, err
	}
	actual, err := config.ExistingDirectory(thread.Cwd)
	if err != nil {
		return Thread{}, err
	}
	thread.Cwd = actual
	c.freshThreads.Store(thread.ID, thread)
	return thread, nil
}

func (c *Client) ResumeThread(ctx context.Context, cfg config.Config, id string) (Thread, error) {
	// thread/start 已在当前服务内创建会话，首轮之前不再尝试从未落盘的历史恢复。
	if value, ok := c.freshThreads.Load(id); ok {
		fresh := value.(Thread)
		if _, e := config.ExistingDirectory(fresh.Cwd); e != nil {
			return Thread{}, e
		}
		return fresh, nil
	}
	original, err := c.ReadMetadata(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	if c.runtimeConfig.Codex.SubmissionMode == "native_queue" || c.runtimeConfig.Codex.SubmissionMode == "desktop_queue" {
		return original, nil
	}
	if _, err = config.ExistingDirectory(original.Cwd); err != nil {
		return Thread{}, err
	}
	// 不传全局 cwd；由 Codex 保留原会话目录，避免恢复时串到另一个项目。
	cfg.WorkDir = ""
	resumed, err := c.Thread(ctx, "thread/resume", id, cfg)
	if err != nil {
		return Thread{}, err
	}
	actual, err := config.ExistingDirectory(resumed.Cwd)
	if err != nil {
		return Thread{}, err
	}
	resumed.Cwd = actual
	return resumed, nil
}

func (c *Client) ForkThread(ctx context.Context, cfg config.Config, id, lastTurnID string) (Thread, error) {
	original, e := c.ReadMetadata(ctx, id)
	if e != nil {
		return Thread{}, e
	}
	if _, e = config.ExistingDirectory(original.Cwd); e != nil {
		return Thread{}, e
	}
	cfg.WorkDir = ""
	p := Options(cfg)
	p["threadId"] = id
	p["excludeTurns"] = true
	if lastTurnID != "" {
		p["lastTurnId"] = lastTurnID
	}
	raw, err := c.Call(ctx, "thread/fork", p)
	if err != nil {
		return Thread{}, err
	}
	var v struct {
		Thread Thread `json:"thread"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Thread.ID == "" {
		return Thread{}, errors.New("Codex 返回的分叉会话无效")
	}
	actual, e := config.ExistingDirectory(v.Thread.Cwd)
	if e != nil {
		return Thread{}, e
	}
	v.Thread.Cwd = actual
	return v.Thread, nil
}

type TurnHooks struct {
	Model, Effort       string
	RequestID           string
	DraftRecoveryNeeded func()
	Progress            func(string, string)
	BeforeSubmit        func(string, int64) error
}

func (c *Client) Turn(ctx context.Context, threadID, text string, progress ...func(string, string)) (string, error) {
	hooks := TurnHooks{}
	if len(progress) > 0 {
		hooks.Progress = progress[0]
	}
	return c.TurnWithHooks(ctx, threadID, text, hooks)
}

func (c *Client) TurnWithHooks(ctx context.Context, threadID, text string, hooks TurnHooks) (string, error) {
	if hooks.Model != "" {
		return c.selectedModelTurn(ctx, threadID, text, hooks)
	}
	if c.runtimeConfig.Codex.SubmissionMode == "native_queue" || c.runtimeConfig.Codex.SubmissionMode == "desktop_queue" {
		return c.desktopQueueTurn(ctx, threadID, text, hooks)
	}
	if hooks.BeforeSubmit != nil {
		path, offset := findDesktopRollout(c.runtimeConfig.Codex.Home, threadID)
		if err := hooks.BeforeSubmit(path, offset); err != nil {
			return "", err
		}
	}
	if c.transport != nil {
		return c.nativeTurn(ctx, threadID, text)
	}
	raw, err := c.Call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    []map[string]any{{"type": "text", "text": text}},
	})
	if err != nil {
		var rpc *RPCError
		if !errors.As(err, &rpc) {
			c.freshThreads.Delete(threadID)
		}
		return "", err
	}
	var v struct {
		Turn Turn `json:"turn"`
	}
	c.freshThreads.Delete(threadID)
	if json.Unmarshal(raw, &v) != nil || v.Turn.ID == "" {
		return "", errors.New("Codex 返回的执行任务无效")
	}
	return v.Turn.ID, nil
}

func (c *Client) Archive(ctx context.Context, threadID string) error {
	_, err := c.Call(ctx, "thread/archive", map[string]any{"threadId": threadID})
	// 桌面持有写入锁时不能由辅助App Server强行归档；保留待处理意图，绝不终止桌面。
	var rpc *RPCError
	if errors.As(err, &rpc) && rpc.Code == -32600 && strings.Contains(rpc.Message, "already has an active writer") {
		return diagnostics.UserError("会话仍被 Codex 桌面或其他进程占用，归档尚未完成。请结束任务并关闭该会话，或在桌面手动归档；释放占用后桥接会按原重试机制继续核对，不会强制关闭桌面或删除历史")
	}
	return err
}
