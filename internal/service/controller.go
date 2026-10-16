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
	cfg, notice, e := codex.PreparePolicy(ctx, cfg)
	if e != nil {
		return e
	}
	if cfg.Codex.SubmissionMode == "native_queue" || cfg.Codex.SubmissionMode == "desktop_queue" {
		notice += " 桌面输入路径由桌面会话自身的权限和审批策略控制；本桥接的沙箱设置只约束辅助 App Server，不能收紧桌面现有权限。"
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
		err := c.run(run, cfg, adapters)
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
func (c *Controller) run(ctx context.Context, cfg config.Config, adapters map[string]im.Adapter) error {
	db, e := store.New(cfg.StateDir)
	if e != nil {
		return errors.New("业务数据库初始化失败，请检查目录权限及是否已有桥接实例")
	}
	defer db.Close()
	cx, e := codex.Start(ctx, cfg)
	if e != nil {
		return errors.New("Codex 初始化失败；请确认 CLI 版本、CODEX_HOME 及本机 Codex 配置兼容")
	}
	defer cx.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	if c.status.Phase != "stopping" {
		c.status.Phase = "running"
		c.status.Message = "桥接进程运行中；IM 正在连接或重连，此状态不保证模型接口可用"
	}
	c.mu.Unlock()
	// 不向 UI 转发 SDK 原始日志、用户对话或模型响应，避免凭证和业务数据泄露。
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	operationCtx, stopOperations := context.WithCancel(ctx)
	defer stopOperations()
	b := bridge.New(cfg, db, cx, adapters, logger)
	c.mu.Lock()
	c.bridge = b
	c.operationContext = operationCtx
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.bridge = nil
		c.operationContext = nil
		c.mu.Unlock()
		stopOperations()
		c.operations.Wait()
	}()
	e = b.Start(ctx)
	if e != nil && ctx.Err() == nil {
		return errors.New("桥接运行异常，连接已停止；请检查 IM 权限、网络或 Codex 状态")
	}
	return e
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
