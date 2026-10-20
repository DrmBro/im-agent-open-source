package desktop

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"im-agent/internal/channels"
	"im-agent/internal/channels/dingtalk"
	"im-agent/internal/channels/feishu"
	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/daemon"
	"im-agent/internal/preferences"
	"im-agent/internal/service"
	"im-agent/internal/store"
)

// App 仅暴露配置 DTO 和白名单操作，不提供执行任意命令或读取任意文件的前端 API。
type App struct {
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	store      *preferences.Store
	controller *daemon.Client
	registry   *channels.Registry
	initError  string
	closed     bool
	dirty      bool
}
type View struct {
	Snapshot preferences.Snapshot  `json:"snapshot"`
	Channels []channels.Descriptor `json:"channels"`
	Status   service.Status        `json:"status"`
	Codex    codex.LocalInfo       `json:"codex"`
}
type Imported struct {
	Config   config.Config `json:"config"`
	FileName string        `json:"file_name"`
}

func New() *App { return &App{registry: channels.NewRegistry()} }
func (a *App) Startup(parent context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ctx, a.cancel = context.WithCancel(parent)
	if e := feishu.Register(a.registry); e != nil {
		a.initError = "渠道注册失败"
		return
	}
	if e := dingtalk.Register(a.registry); e != nil {
		a.initError = "渠道注册失败"
		return
	}
	root, e := os.UserConfigDir()
	if e != nil {
		a.initError = "无法找到系统用户配置目录"
		return
	}
	dir := filepath.Join(root, "IM Agent")
	a.store, e = preferences.New(dir, a.registry)
	if e != nil {
		a.initError = "无法打开本地数据库；请检查用户配置目录 IM Agent 的权限。原有数据未被覆盖。"
		return
	}
	a.controller = daemon.NewClient(dir)
}
func (a *App) ready() error {
	if a.closed {
		return errors.New("应用正在退出")
	}
	if a.initError != "" {
		return errors.New(a.initError)
	}
	if a.store == nil || a.controller == nil {
		return errors.New("应用尚未初始化")
	}
	return nil
}
func (a *App) GetView() (View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return View{}, e
	}
	v, e := a.store.Read()
	if e != nil {
		return View{}, errors.New("读取已保存配置失败，请检查数据库")
	}
	return View{Snapshot: v, Channels: a.registry.Descriptors(), Status: a.controller.Status(), Codex: codex.InspectLocal(v.Config.Codex, v.Config.WorkDir)}, nil
}
func (a *App) GetStatus() (service.Status, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return service.Status{}, e
	}
	return a.controller.Status(), nil
}
func (a *App) Save(req preferences.SaveRequest) (preferences.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return preferences.Snapshot{}, e
	}
	return a.store.Save(req)
}
func (a *App) StartService() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return e
	}
	c, revision, e := a.store.Runtime()
	if e != nil {
		return e
	}
	return a.controller.Start(a.ctx, c, revision)
}
func (a *App) StopService() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return e
	}
	return a.controller.Stop()
}
func (a *App) ChooseDirectory() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return "", e
	}
	return wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{Title: "选择目录", CanCreateDirectories: true})
}
func (a *App) ChooseExecutable() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return "", e
	}
	return wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{Title: "选择 Codex CLI 可执行文件"})
}
func (a *App) ImportJSON() (*Imported, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return nil, e
	}
	path, e := wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{Title: "导入旧配置（只预览，保存后才生效）", Filters: []wruntime.FileFilter{{DisplayName: "JSON 配置", Pattern: "*.json"}}})
	if e != nil {
		return nil, errors.New("无法打开文件选择窗口")
	}
	if path == "" {
		return nil, nil
	}
	stat, e := os.Stat(path)
	if e != nil || !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return nil, errors.New("请选择不超过 1 MB 的普通 JSON 文件")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, errors.New("无法读取所选文件")
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil || len(data) > 1<<20 {
		return nil, errors.New("导入文件过大或不可读")
	}
	c, e := config.Decode(data)
	if e != nil {
		return nil, e
	}
	// 导入中的相对路径按原 JSON 所在位置解释，不按应用当前目录猜测。
	for _, p := range []*string{&c.WorkDir, &c.Codex.Home} {
		if *p != "" && !filepath.IsAbs(*p) && (*p)[0] != '~' {
			*p = filepath.Join(filepath.Dir(path), *p)
		}
	}
	c, e = a.store.Validate(c)
	if e != nil {
		return nil, e
	}
	return &Imported{Config: c, FileName: filepath.Base(path)}, nil
}
func (a *App) BeforeClose(ctx context.Context) bool {
	a.mu.Lock()
	dirty := a.dirty
	a.mu.Unlock()
	if dirty {
		answer, e := wruntime.MessageDialog(ctx, wruntime.MessageDialogOptions{Type: wruntime.QuestionDialog, Title: "关闭配置窗口？", Message: "未保存的配置会丢失。后台桥接继续运行，不会中断任务。", Buttons: []string{"取消", "关闭窗口"}, DefaultButton: "取消", CancelButton: "取消"})
		if e != nil || answer != "关闭窗口" {
			return true
		}
	}
	return false
}
func (a *App) Shutdown(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	if a.cancel != nil {
		a.cancel()
	}
	// 生命周期仅属于 UI。后台运行由本机独立进程管理，退出窗口不能停止它。
	if a.store != nil {
		_ = a.store.Close()
	}
}

// InspectCodex 只读检测草稿路径，不执行命令、不返回密钥或修改本机配置。
func (a *App) InspectCodex(c config.Codex, workDir string) (codex.LocalInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return codex.LocalInfo{}, e
	}
	return codex.InspectLocal(c, workDir), nil
}

type ClosureView struct {
	Items   []store.Closure `json:"items"`
	Warning string          `json:"warning"`
}

func (a *App) GetClosures() (ClosureView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return ClosureView{}, e
	}
	rows, e := a.store.ClosureStatus()
	if e != nil {
		return ClosureView{}, errors.New("读取双向同步记录失败，请稍后刷新")
	}
	return ClosureView{Items: rows, Warning: a.controller.LifecycleWarning()}, nil
}
func (a *App) RetryClosure(thread string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return e
	}
	if len(thread) < 1 || len(thread) > 128 {
		return errors.New("会话 ID 无效")
	}
	return a.controller.RetryClosure(thread)
}

func (a *App) GetPresentations() ([]store.Presentation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return nil, e
	}
	rows, e := a.store.PresentationStatus()
	if e != nil {
		return nil, errors.New("读取群名和历史补齐状态失败")
	}
	return rows, nil
}
func (a *App) RetryPresentation(thread string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return e
	}
	if thread == "" || len(thread) > 128 {
		return errors.New("会话 ID 无效")
	}
	return a.controller.RetryPresentation(thread)
}

func (a *App) GetEventDiagnostics() ([]store.EventDiagnostic, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return nil, e
	}
	rows, e := a.store.EventDiagnostics()
	if e != nil {
		return nil, errors.New("读取消息处理记录失败")
	}
	return rows, nil
}

func (a *App) ChooseAttachment() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.ready(); e != nil {
		return "", e
	}
	return wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{Title: "选择会话工作目录内的文件（上限 20 MiB）"})
}
