package daemon

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

const agentLabel = "io.im-agent.background"

func launchAgentXML(binary, dir string) []byte {
	esc := func(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>--service</string><string>--data-dir</string><string>%s</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>15</integer>
<key>ProcessType</key><string>Background</string></dict></plist>`, agentLabel, esc(binary), esc(dir)))
}
func agentPath() (string, error) {
	home, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist"), nil
}
func (c *Client) AutoStartEnabled() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	p, e := agentPath()
	if e != nil {
		return false
	}
	info, e := os.Lstat(p)
	return e == nil && info.Mode().IsRegular()
}

// 仅由明确的客户端操作触发。只在桥接停止时切换进程管理器，避免中断模型任务。
func (c *Client) ConfigureAutoStart(enabled bool) error {
	if runtime.GOOS != "darwin" {
		return errors.New("本版本只支持 macOS 登录自启")
	}
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	if e := c.requireStopped(ctx); e != nil {
		return e
	}
	path, e := agentPath()
	if e != nil {
		return e
	}
	target := "gui/" + strconv.Itoa(os.Getuid())
	if enabled {
		if c.AutoStartEnabled() {
			return nil
		}
		if e = c.Ensure(ctx); e != nil {
			return e
		}
		if e = c.call(ctx, "POST", "/shutdown", struct{}{}, nil); e != nil {
			return e
		}
		// 以内核锁释放为终止证据，而不是把一次 HTTP 超时当作进程已死。
		for {
			unlock, err := lockDaemon(filepath.Join(c.Dir, "daemon.lock"))
			if err == nil {
				unlock()
				break
			}
			select {
			case <-ctx.Done():
				return errors.New("原后台尚未退出，未安装第二实例")
			case <-time.After(100 * time.Millisecond):
			}
		}
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return e
		}
		if _, e = os.Lstat(path); !os.IsNotExist(e) {
			return errors.New("启动项已存在或不可确认，未覆盖")
		}
		if e = writePrivate(path, launchAgentXML(filepath.Join(c.Dir, "bin", "im-agent-service"), c.Dir), 0600); e != nil {
			return e
		}
		if e = exec.CommandContext(ctx, "/bin/launchctl", "bootstrap", target, path).Run(); e != nil {
			_ = os.Remove(path)
			return errors.New("系统未接受登录启动项；未启用，请手动启动后台")
		}
		return nil
	}
	if !c.AutoStartEnabled() {
		return nil
	}
	if e = exec.CommandContext(ctx, "/bin/launchctl", "bootout", target+"/"+agentLabel).Run(); e != nil {
		return errors.New("系统未确认卸载登录启动项，保留原文件供检查")
	}
	return os.Remove(path)
}
