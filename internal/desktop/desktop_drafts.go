package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"im-agent/internal/drafts"
)

// Wails只接收字符串时间DTO，避免将time.Time作为未知结构生成前端绑定。
type DesktopDraftRecord struct {
	ID        string `json:"id"`
	EventID   string `json:"event_id"`
	ThreadID  string `json:"thread_id"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type DesktopDraftList struct {
	Records []DesktopDraftRecord `json:"records"`
	Warning string               `json:"warning"`
}

// 仅本机配置客户端可读；不注册daemon/IM远程草稿读取接口。
func (a *API) GetDesktopDrafts() (DesktopDraftList, error) {
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return DesktopDraftList{}, err
	}
	snapshot, err := a.app.store.Read()
	a.app.mu.Unlock()
	if err != nil {
		return DesktopDraftList{}, err
	}
	records, listErr := drafts.New(snapshot.Config.StateDir).List()
	view := DesktopDraftList{Records: make([]DesktopDraftRecord, 0, len(records))}
	for _, record := range records {
		view.Records = append(view.Records, DesktopDraftRecord{
			ID: record.ID, EventID: record.EventID, ThreadID: record.ThreadID,
			State: record.State, Reason: record.Reason,
			CreatedAt: record.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: record.UpdatedAt.Format(time.RFC3339Nano),
		})
	}
	if listErr != nil {
		view.Warning = "部分草稿记录不可读取或有未完成写入，异常文件保持不变；可用记录仍可查看。若无可用记录，请先检查本机快照目录，勿清空或覆盖。"
	}
	return view, nil
}

// 查看必须显式确认；仅返回所选一条快照，不自动粘贴到桌面或发送到会话。
func (a *API) ReadDesktopDraft(id string, confirmed bool) (string, error) {
	if !confirmed {
		return "", errors.New("请先确认在本机显示这条草稿原文")
	}
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return "", err
	}
	snapshot, err := a.app.store.Read()
	a.app.mu.Unlock()
	if err != nil {
		return "", err
	}
	return drafts.New(snapshot.Config.StateDir).Read(id)
}

// ExportDesktopDraft 是用户显式选择的本机明文导出，不经textarea或剪贴板归一化。
// 拒绝覆盖已有文件；不能防止用户选中的目录被其它同步软件上传，前端须明确提醒。
func (a *API) ExportDesktopDraft(id string, confirmed bool) (string, error) {
	if !confirmed {
		return "", errors.New("请先确认将所选草稿导出为本机明文文件")
	}
	text, err := a.ReadDesktopDraft(id, true)
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
		Title:           "导出草稿原文（明文，请选择非云同步目录和新文件名）",
		DefaultFilename: "desktop-draft.txt",
		Filters:         []wruntime.FileFilter{{DisplayName: "纯文本", Pattern: "*.txt"}},
	})
	if err != nil {
		return "", errors.New("未能选择草稿导出位置")
	}
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("导出位置必须是本机绝对路径")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("无法创建草稿导出文件；不覆盖已有文件，请选择新的文件名")
	}
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return "", errors.New("无法确认导出文件权限；可能已创建空文件，请自行检查")
	}
	n, err := file.WriteString(text)
	if err != nil || n != len(text) {
		return "", errors.New("草稿导出未完整写入；可能存在部分文件，加密快照仍保留")
	}
	if err = file.Sync(); err != nil {
		return "", errors.New("草稿导出同步未确认；加密快照仍保留")
	}
	if err = file.Close(); err != nil {
		return "", errors.New("草稿导出关闭未确认；加密快照仍保留")
	}
	return path, nil
}
