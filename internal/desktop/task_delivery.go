package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"im-agent/internal/bridge"
	"im-agent/internal/redact"
	"im-agent/internal/store"
)

// 只读取已封存回复；不从桌面/模型重新请求内容，也不扫描项目文件。
func (a *API) GetTaskDelivery(thread, event string) (store.TaskDelivery, error) {
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return store.TaskDelivery{}, err
	}
	view, err := a.app.store.Read()
	parent := a.app.ctx
	a.app.mu.Unlock()
	if err != nil {
		return store.TaskDelivery{}, err
	}
	ctx, stop := context.WithTimeout(parent, 5*time.Second)
	defer stop()
	delivery, err := store.TaskDeliverySnapshot(ctx, view.Config.StateDir, thread, event)
	if errors.Is(err, store.ErrTaskDeliveryPruned) {
		return store.TaskDelivery{}, store.ErrTaskDeliveryPruned
	}
	if err != nil {
		return store.TaskDelivery{}, errors.New("本请求的完整回复证据不足、会话已关闭或报告超限，暂不能查看；不会调用模型补齐")
	}
	filter := redact.New()
	delivery.Summary = filter.Text(delivery.Summary)
	delivery.Report = filter.Text(delivery.Report)
	return delivery, nil
}

func (a *API) SendTaskReport(thread, event string, confirmed bool) error {
	if !confirmed {
		return errors.New("请先确认将本次报告分享至对应群，群成员均可见")
	}
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return err
	}
	client, parent := a.app.controller, a.app.ctx
	a.app.mu.Unlock()
	ctx, stop := context.WithTimeout(parent, 90*time.Second)
	defer stop()
	return client.SendTaskReport(ctx, bridge.SendTaskReportRequest{ThreadID: thread, EventID: event})
}

// 原文仅来自GetTaskDelivery的明确任务；本机保存由用户选路径，禁止覆盖已有文件。
func (a *API) ExportTaskReport(thread, event string, confirmed bool) (string, error) {
	if !confirmed {
		return "", errors.New("请先确认把本次报告保存到可信本机目录")
	}
	delivery, err := a.GetTaskDelivery(thread, event)
	if err != nil {
		return "", err
	}
	a.app.mu.Lock()
	if err = a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return "", err
	}
	ctx := a.app.ctx
	a.app.mu.Unlock()
	path, err := wruntime.SaveFileDialog(ctx, wruntime.SaveDialogOptions{
		Title: "保存任务报告（请选择可信本机目录和新文件名）", DefaultFilename: delivery.ReportName,
		Filters: []wruntime.FileFilter{{DisplayName: "Markdown报告", Pattern: "*.md"}},
	})
	if err != nil {
		return "", errors.New("未能选择报告保存位置")
	}
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("报告保存位置必须是绝对路径")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("无法创建报告文件，不覆盖已有文件，请选择新的文件名")
	}
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return "", errors.New("报告文件权限未确认，可能已创建空文件，请自行检查")
	}
	n, err := file.WriteString(delivery.Report)
	if err != nil || n != len(delivery.Report) {
		return "", errors.New("报告写入不完整，可能留有部分文件；原任务回复不受影响")
	}
	if err = file.Sync(); err != nil {
		return "", errors.New("报告文件同步未确认；原任务回复不受影响")
	}
	if err = file.Close(); err != nil {
		return "", errors.New("报告文件关闭未确认；原任务回复不受影响")
	}
	return path, nil
}
