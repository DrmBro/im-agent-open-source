package codex

import (
	"errors"
	"os"
	"path/filepath"

	"im-agent/internal/config"
)

func privateDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("应用数据目录尚未正确初始化")
	}
	if e := os.MkdirAll(path, 0700); e != nil {
		return "", errors.New("无法创建自动工作目录，请检查应用数据目录权限")
	}
	st, e := os.Lstat(path)
	if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("自动工作目录不能是符号链接或普通文件")
	}
	if e = os.Chmod(path, 0700); e != nil {
		return "", errors.New("无法设置自动工作目录权限")
	}
	return config.ExistingDirectory(path)
}

// HostDirectory 只是 App Server 进程启动目录，不替代任何会话的 cwd。
func HostDirectory(stateDir string) (string, error) {
	return privateDirectory(filepath.Join(stateDir, "codex-host"))
}

// NewSessionDirectory 为不指定项目的每个新会话分配独立目录，不复用根目录。
func NewSessionDirectory(stateDir, project string) (string, error) {
	if project != "" {
		return config.ExistingDirectory(project)
	}
	root, e := privateDirectory(filepath.Join(stateDir, "workspaces"))
	if e != nil {
		return "", e
	}
	path, e := os.MkdirTemp(root, "session-")
	if e != nil {
		return "", errors.New("无法为新会话分配独立工作目录")
	}
	return path, nil
}
