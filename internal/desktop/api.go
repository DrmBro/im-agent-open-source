package desktop

import (
	"context"
	"im-agent/internal/bridge"
	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/desktoplaunch"
	"im-agent/internal/preferences"
	"im-agent/internal/service"
	"im-agent/internal/store"
	"time"
)

// API 是唯一绑定对象，生命周期和内部存储不会暴露为前端方法。
type API struct{ app *App }

func NewAPI(a *App) *API                                                    { return &API{app: a} }
func (a *API) GetView() (View, error)                                       { return a.app.GetView() }
func (a *API) GetStatus() (service.Status, error)                           { return a.app.GetStatus() }
func (a *API) Save(r preferences.SaveRequest) (preferences.Snapshot, error) { return a.app.Save(r) }
func (a *API) StartService() error                                          { return a.app.StartService() }
func (a *API) StopService() error                                           { return a.app.StopService() }
func (a *API) ChooseDirectory() (string, error)                             { return a.app.ChooseDirectory() }
func (a *API) ChooseExecutable() (string, error)                            { return a.app.ChooseExecutable() }
func (a *API) ImportJSON() (*Imported, error)                               { return a.app.ImportJSON() }

// SetDirty 仅记录是否需要退出提示，不接受或持久化草稿内容。
func (a *API) SetDirty(dirty bool) { a.app.mu.Lock(); defer a.app.mu.Unlock(); a.app.dirty = dirty }

func (a *API) InspectCodex(c config.Codex, workDir string) (codex.LocalInfo, error) {
	return a.app.InspectCodex(c, workDir)
}

func (a *API) GetClosures() (ClosureView, error) { return a.app.GetClosures() }
func (a *API) RetryClosure(thread string) error  { return a.app.RetryClosure(thread) }

func (a *API) GetPresentations() ([]store.Presentation, error) { return a.app.GetPresentations() }
func (a *API) RetryPresentation(thread string) error           { return a.app.RetryPresentation(thread) }

func (a *API) GetEventDiagnostics() ([]store.EventDiagnostic, error) {
	return a.app.GetEventDiagnostics()
}

func (a *API) GetTaskCenter() (store.TaskCenter, error) {
	a.app.mu.Lock()
	defer a.app.mu.Unlock()
	if e := a.app.ready(); e != nil {
		return store.TaskCenter{}, e
	}
	snapshot, e := a.app.store.Read()
	if e != nil {
		return store.TaskCenter{}, e
	}
	return store.TaskCenterSnapshot(snapshot.Config.StateDir)
}
func (a *API) ResolveDelivery(r store.DeliveryResolution) error {
	a.app.mu.Lock()
	defer a.app.mu.Unlock()
	if e := a.app.ready(); e != nil {
		return e
	}
	return a.app.controller.ResolveDelivery(r)
}

func (a *API) ChooseAttachment() (string, error) { return a.app.ChooseAttachment() }
func (a *API) SendFile(r bridge.SendFileRequest) error {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return e
	}
	client := a.app.controller
	a.app.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return client.SendFile(ctx, r)
}

type BackgroundInfo struct {
	AutoStart bool   `json:"auto_start"`
	Message   string `json:"message"`
}

func (a *API) GetBackgroundInfo() (BackgroundInfo, error) {
	a.app.mu.Lock()
	defer a.app.mu.Unlock()
	if e := a.app.ready(); e != nil {
		return BackgroundInfo{}, e
	}
	return BackgroundInfo{AutoStart: a.app.controller.AutoStartEnabled(), Message: "关闭窗口不停止后台；登录自启可由系统在进程退出后重启。停止桥接只停止业务，不退出控制服务。"}, nil
}
func (a *API) SetAutoStart(enabled bool) error {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return e
	}
	client := a.app.controller
	a.app.mu.Unlock()
	return client.ConfigureAutoStart(enabled)
}

func (a *API) UpdateBackground() error {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return e
	}
	client := a.app.controller
	a.app.mu.Unlock()
	ctx, stop := context.WithTimeout(context.Background(), 45*time.Second)
	defer stop()
	return client.RefreshBinary(ctx)
}

func (a *API) OpenCreationForm(thread string) error {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return e
	}
	client := a.app.controller
	a.app.mu.Unlock()
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	return client.OpenCreationForm(ctx, thread)
}

func (a *API) OpenSessionStatus(thread string) error {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return e
	}
	client := a.app.controller
	a.app.mu.Unlock()
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	return client.OpenSessionStatus(ctx, thread)
}

// 桌面启动与后台桥接分开控制；不在GetView或状态轮询中自动启动应用。
func (a *API) InspectDesktopApplication(path string) (desktoplaunch.Status, error) {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return desktoplaunch.Status{}, e
	}
	snapshot, e := a.app.store.Read()
	a.app.mu.Unlock()
	if e != nil {
		return desktoplaunch.Status{}, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return desktoplaunch.Inspect(ctx, path, snapshot.Config.Codex.CdpPort)
}
func (a *API) LaunchDesktopApplication(path string, confirmed bool) (desktoplaunch.Status, error) {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return desktoplaunch.Status{}, e
	}
	snapshot, e := a.app.store.Read()
	a.app.mu.Unlock()
	if e != nil {
		return desktoplaunch.Status{}, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return desktoplaunch.Launch(ctx, path, snapshot.Config.Codex.CdpPort, confirmed)
}
