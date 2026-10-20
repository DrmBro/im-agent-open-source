// Package service 管理桌面桥接服务生命周期；保存配置与运行快照互相独立。
package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"im-agent/internal/bridge"
	"im-agent/internal/channels"
	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/qa"
	"im-agent/internal/store"
)

type Status struct {
	PolicyNotice string                `json:"policy_notice"`
	Phase        string                `json:"phase"`
	Message      string                `json:"message"`
	Revision     int64                 `json:"revision"`
	CodexChanged bool                  `json:"codex_changed"`
	Connections  []im.ConnectionStatus `json:"connections"`
	Diagnostics  bridge.Diagnostics    `json:"diagnostics"`
}
type Controller struct {
	operations       sync.WaitGroup
	operationContext context.Context
	mu               sync.Mutex
	status           Status
	cancel           context.CancelFunc
	done             chan struct{}
	registry         *channels.Registry
	localConfig      config.Codex
	workDir          string
	localInfo        codex.LocalInfo
	bridge           *bridge.Bridge
	qa               *qa.Runner
	adapters         map[string]im.Adapter
	policyNotice     string
}

func New(r *channels.Registry) *Controller {
	return &Controller{registry: r, status: Status{Phase: "stopped", Message: "桥接服务未启动"}}
}
func (c *Controller) Status() Status {
	c.mu.Lock()
	status, settings, work, original := c.status, c.localConfig, c.workDir, c.localInfo
	status.PolicyNotice = c.policyNotice
	status.Connections = []im.ConnectionStatus{}
	for _, a := range c.adapters {
		if p, ok := a.(im.StatusProvider); ok {
			status.Connections = append(status.Connections, p.ConnectionStatus())
		}
	}
	if c.bridge != nil {
		status.Diagnostics = c.bridge.Diagnostics()
	}
	if c.qa != nil {
		phase, message, at := c.qa.Diagnostics()
		if c.bridge == nil {
			status.Diagnostics.ScanPhase = "disabled"
			status.Diagnostics.ScanMessage = "钉钉使用群内问答，不扫描桌面会话"
			status.Diagnostics.Intake = "钉钉 Stream 收件在持久化后确认"
			status.Diagnostics.LiveSync = "引用回答续聊；CLI 会话不写入桌面历史"
		}
		if at >= status.Diagnostics.MessageAt {
			status.Diagnostics.MessagePhase = phase
			status.Diagnostics.Message = message
			status.Diagnostics.MessageAt = at
		}
	}
	c.mu.Unlock()
	if status.Phase == "running" || status.Phase == "starting" {
		latest := codex.InspectLocal(settings, work)
		status.CodexChanged = !latest.Ready || latest.Fingerprint() != original.Fingerprint()
	}
	return status
}
func (c *Controller) Start(ctx context.Context, cfg config.Config, revision int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("桥接正在运行或停止中，请等待停止完成后再启动")
	}
	original := cfg.Codex
	resolved, info, e := codex.ResolveLocal(cfg)
	if e != nil {
		return e
	}
	cfg = resolved
	qaConfig := resolved
	hasBridge, hasQA := false, false
	for _, connection := range cfg.Connections {
		if connection.Type == "dingtalk" {
			hasQA = true
		} else {
			hasBridge = true
		}
	}
	notice := ""
	if hasBridge {
		cfg, notice, e = codex.PreparePolicy(ctx, cfg)
		if e != nil {
			return e
		}
		if cfg.Codex.SubmissionMode == "native_queue" || cfg.Codex.SubmissionMode == "desktop_queue" {
			notice += " 桌面输入路径由桌面会话自身的权限和审批策略控制；本桥接的沙箱设置只约束辅助 App Server，不能收紧桌面现有权限。"
		}
	}
	if hasQA {
		notice += " 钉钉使用独立 CLI 业务会话，复用本机模型与认证；所有人员按同一业务范围回答，主机工具与外部连接器关闭，文档和进度在本机问答面板查看。"
	}
	c.policyNotice = notice
	// 构建时不发网络请求；跨连接复用同一个应用会竞争飞书事件，拒绝启动。
	apps := map[string]bool{}
	adapters := map[string]im.Adapter{}
	for _, x := range cfg.Connections {
		id, _ := x.Credential("app_id")
		key := x.Type + "\x00" + id
		if id != "" && apps[key] {
			return errors.New("同一 IM 应用不能同时配置多个连接")
		}
		apps[key] = true
		a, e := c.registry.Build(x)
		if e != nil {
			return errors.New("IM 凭证或配置未完整，请检查连接设置")
		}
		adapters[x.ID] = a
	}
	c.adapters = adapters
	c.localConfig = original
	c.workDir = cfg.WorkDir
	c.localInfo = info
	run, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.done = make(chan struct{})
	done := c.done
	c.status = Status{Phase: "starting", Message: "正在初始化数据库、Codex 和 IM 接收服务", Revision: revision}
	go func() {
		err := c.run(run, cfg, qaConfig, adapters)
		c.mu.Lock()
		defer c.mu.Unlock()
		if run.Err() != nil {
			c.status = Status{Phase: "stopped", Message: "桥接服务已停止", Revision: revision}
		} else if err != nil {
			c.status = Status{Phase: "failed", Message: err.Error(), Revision: revision}
		} else {
			c.status = Status{Phase: "stopped", Message: "桥接服务已结束", Revision: revision}
		}
		cancel()
		c.cancel = nil
		close(done)
	}()
	return nil
}
func (c *Controller) run(parent context.Context, cfg, qaConfig config.Config, adapters map[string]im.Adapter) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	db, e := store.New(cfg.StateDir)
	if e != nil {
		return errors.New("业务数据库初始化失败，请检查目录权限及是否已有桥接实例")
	}
	defer db.Close()
	legacy, ding := map[string]im.Adapter{}, map[string]im.Adapter{}
	for id, a := range adapters {
		if a.Identity().Channel == "dingtalk" {
			ding[id] = a
		} else {
			legacy[id] = a
		}
	}
	connections := make([]string, 0, len(ding))
	for id := range ding {
		connections = append(connections, id)
	}
	if e = db.QAReconcileConnections(connections); e != nil {
		return errors.New("钉钉问答连接记录核对失败，未启动服务")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var b *bridge.Bridge
	var q *qa.Runner
	if len(legacy) > 0 {
		cx, err := codex.Start(ctx, cfg)
		if err != nil {
			return errors.New("Codex 初始化失败，请检查 CLI 和本机配置")
		}
		defer cx.Close()
		b = bridge.New(cfg, db, cx, legacy, logger)
	}
	if len(ding) > 0 {
		cx, err := codex.StartQA(ctx, qaConfig)
		if err != nil {
			return errors.New("钉钉 CLI 初始化失败，请检查本机 Codex 配置")
		}
		defer cx.Close()
		q = qa.New(qaConfig, db, cx, ding)
	}
	operationCtx, stopOperations := context.WithCancel(ctx)
	c.mu.Lock()
	c.bridge = b
	c.qa = q
	c.operationContext = operationCtx
	if c.status.Phase != "stopping" {
		c.status.Phase = "running"
		c.status.Message = "后台进程运行中；IM 正在连接或重连，此状态不保证模型接口可用"
	}
	c.mu.Unlock()
	defer func() {
		stopOperations()
		c.mu.Lock()
		c.bridge = nil
		c.qa = nil
		c.operationContext = nil
		c.mu.Unlock()
		c.operations.Wait()
	}()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	if b != nil {
		wg.Add(1)
		go func() { defer wg.Done(); results <- b.Start(ctx) }()
	}
	if q != nil {
		wg.Add(1)
		go func() { defer wg.Done(); results <- q.Start(ctx) }()
	}
	if b == nil && q == nil {
		return errors.New("没有可运行的 IM 连接")
	}
	select {
	case <-parent.Done():
		e = nil
	case e = <-results:
		if e == nil && parent.Err() == nil {
			e = errors.New("IM 运行器提前停止")
		}
	}
	cancel()
	wg.Wait()
	if e != nil && parent.Err() == nil {
		return errors.New("后台连接或问答运行异常，已停止；请检查 IM、CLI 和本机配置")
	}
	return nil
}
func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		c.status.Phase = "stopping"
		c.status.Message = "正在中断任务并关闭连接，请稍候"
		c.cancel()
	}
}
func (c *Controller) Wait(ctx context.Context) error {
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Controller) RetryClosure(thread string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bridge == nil || c.status.Phase != "running" {
		return errors.New("请先启动桥接，再重试待同步任务")
	}
	return c.bridge.RetryClosure(thread)
}
func (c *Controller) LifecycleWarning() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bridge == nil {
		return ""
	}
	return c.bridge.LifecycleWarning()
}

func (c *Controller) RetryPresentation(thread string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bridge == nil || c.status.Phase != "running" {
		return errors.New("请先启动桥接")
	}
	return c.bridge.RetryPresentation(thread)
}

func (c *Controller) ResolveDelivery(r store.DeliveryResolution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bridge == nil || c.status.Phase != "running" {
		return errors.New("请先启动后台桥接再处理投递")
	}
	return c.bridge.ResolveDelivery(r)
}

func (c *Controller) SendFile(ctx context.Context, r bridge.SendFileRequest) error {
	c.mu.Lock()
	b, phase, parent := c.bridge, c.status.Phase, c.operationContext
	if b == nil || phase != "running" || parent == nil {
		c.mu.Unlock()
		return errors.New("后台桥接未运行")
	}
	c.operations.Add(1)
	c.mu.Unlock()
	defer c.operations.Done()
	op, stop := context.WithCancel(ctx)
	defer stop()
	detach := context.AfterFunc(parent, stop)
	defer detach()
	return b.SendFile(op, r)
}

func (c *Controller) OpenCreationForm(ctx context.Context, thread string) error {
	c.mu.Lock()
	b, phase, parent := c.bridge, c.status.Phase, c.operationContext
	if b == nil || phase != "running" || parent == nil {
		c.mu.Unlock()
		return errors.New("后台桥接未运行")
	}
	c.operations.Add(1)
	c.mu.Unlock()
	defer c.operations.Done()
	op, stop := context.WithCancel(ctx)
	defer stop()
	detach := context.AfterFunc(parent, stop)
	defer detach()
	return b.OpenCreationFormForThread(op, thread)
}

func (c *Controller) OpenSessionStatus(ctx context.Context, thread string) error {
	c.mu.Lock()
	b, phase, parent := c.bridge, c.status.Phase, c.operationContext
	if b == nil || phase != "running" || parent == nil {
		c.mu.Unlock()
		return errors.New("后台桥接未运行")
	}
	c.operations.Add(1)
	c.mu.Unlock()
	defer c.operations.Done()
	op, stop := context.WithCancel(ctx)
	defer stop()
	detach := context.AfterFunc(parent, stop)
	defer detach()
	return b.OpenSessionStatusForThread(op, thread)
}
