package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"im-agent/internal/config"
	"im-agent/internal/service"
)

type Client struct{ Dir string }

func NewClient(dir string) *Client { return &Client{Dir: dir} }
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	info, e := os.Lstat(tokenPath(c.Dir))
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("后台令牌文件权限异常")
	}
	token, e := os.ReadFile(tokenPath(c.Dir))
	if e != nil {
		return e
	}
	var buf bytes.Buffer
	if body != nil {
		if e = json.NewEncoder(&buf).Encode(body); e != nil {
			return e
		}
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath(c.Dir))
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 95 * time.Second}
	req, e := http.NewRequestWithContext(ctx, method, "http://local"+path, &buf)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/json")
	resp, e := client.Do(req)
	if e != nil {
		return errors.New("后台服务未连接")
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if e != nil {
		return e
	}
	if resp.StatusCode != 200 {
		var failure response
		if json.Unmarshal(data, &failure) == nil && failure.Error != "" {
			return errors.New(failure.Error)
		}
		return errors.New("后台请求被拒绝")
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}
func (c *Client) Status() service.Status {
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	var s service.Status
	if e := c.call(ctx, "GET", "/status", nil, &s); e != nil {
		return service.Status{Phase: "stopped", Message: "后台服务未连接；启动后关闭客户端窗口不会停止桥接"}
	}
	return s
}
func (c *Client) Ping(ctx context.Context) error {
	var s service.Status
	return c.call(ctx, "GET", "/status", nil, &s)
}
func (c *Client) Start(ctx context.Context, _ config.Config, _ int64) error {
	if e := c.Ensure(ctx); e != nil {
		return e
	}
	return c.call(ctx, "POST", "/start", struct{}{}, nil)
}
func (c *Client) Stop() error { return c.call(context.Background(), "POST", "/stop", struct{}{}, nil) }
func (c *Client) Wait(ctx context.Context) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		phase := c.Status().Phase
		if phase == "stopped" || phase == "failed" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
func (c *Client) LifecycleWarning() string {
	var out struct {
		Warning string `json:"warning"`
	}
	_ = c.call(context.Background(), "GET", "/lifecycle", nil, &out)
	return out.Warning
}
func (c *Client) RetryClosure(thread string) error {
	return c.call(context.Background(), "POST", "/retry-closure", map[string]string{"thread": thread}, nil)
}
func (c *Client) RetryPresentation(thread string) error {
	return c.call(context.Background(), "POST", "/retry-presentation", map[string]string{"thread": thread}, nil)
}

// 使用稳定副本启动。运行中不替换二进制，不因关闭 UI 发送终止信号。
func (c *Client) Ensure(ctx context.Context) error {
	probe, stop := context.WithTimeout(ctx, time.Second)
	err := c.Ping(probe)
	stop()
	if err == nil {
		return nil
	}
	if e := secureDirectory(c.Dir); e != nil {
		return e
	}
	release, e := lockDaemon(filepath.Join(c.Dir, "launcher.lock"))
	if e != nil {
		return e
	}
	defer release()
	probe, stop = context.WithTimeout(ctx, time.Second)
	err = c.Ping(probe)
	stop()
	if err == nil {
		return nil
	}
	// 活跃的内核控制锁即使暂时没有响应也不能被判作死亡而替换程序。
	unlock, e := lockDaemon(filepath.Join(c.Dir, "daemon.lock"))
	if e != nil {
		return errors.New("后台进程仍在启动或忙碌，请稍候；未启动第二实例")
	}
	unlock()
	binDir := filepath.Join(c.Dir, "bin")
	if e = secureDirectory(binDir); e != nil {
		return e
	}
	executable, e := os.Executable()
	if e != nil {
		return e
	}
	stable := filepath.Join(binDir, "im-agent-service")
	if executable != stable {
		data, e := os.ReadFile(executable)
		if e != nil {
			return e
		}
		if e = writePrivate(stable, data, 0700); e != nil {
			return e
		}
	}
	cmd := exec.Command(stable, "--service", "--data-dir", c.Dir)
	cmd.Dir = c.Dir
	cmd.SysProcAttr = detachAttributes()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = os.Environ()
	if e = cmd.Start(); e != nil {
		return e
	}
	_ = cmd.Process.Release()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	wait, done := context.WithTimeout(ctx, 15*time.Second)
	defer done()
	for {
		probe, stop = context.WithTimeout(wait, time.Second)
		err = c.Ping(probe)
		stop()
		if err == nil {
			return nil
		}
		select {
		case <-wait.Done():
			return errors.New("后台启动未确认，请检查系统凭证库授权和客户端诊断；未重复启动")
		case <-tick.C:
		}
	}
}

// RefreshBinary 是显式升级操作，仅在业务已停止后执行，避免更新窗口造成任务中断。
func (c *Client) RefreshBinary(ctx context.Context) error {
	if e := c.requireStopped(ctx); e != nil {
		return e
	}
	wasAuto := c.AutoStartEnabled()
	if wasAuto {
		if e := c.ConfigureAutoStart(false); e != nil {
			return e
		}
	}
	if c.Ping(ctx) == nil {
		if e := c.call(ctx, "POST", "/shutdown", struct{}{}, nil); e != nil {
			return e
		}
	}
	wait, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	for {
		unlock, e := lockDaemon(filepath.Join(c.Dir, "daemon.lock"))
		if e == nil {
			unlock()
			break
		}
		select {
		case <-wait.Done():
			return errors.New("后台退出未确认，未替换程序")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if e := c.Ensure(ctx); e != nil {
		return e
	}
	if wasAuto {
		return c.ConfigureAutoStart(true)
	}
	return nil
}

// 状态读取失败不是停止证明。维护操作必须拿到 stopped/failed 或确认内核锁空闲。
func (c *Client) requireStopped(ctx context.Context) error {
	probe, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	var s service.Status
	if e := c.call(probe, "GET", "/status", nil, &s); e == nil {
		if s.Phase != "stopped" && s.Phase != "failed" {
			return errors.New("请先停止桥接，再修改后台服务；不会自动中断运行中的任务")
		}
		return nil
	}
	if e := secureDirectory(c.Dir); e != nil {
		return e
	}
	unlock, e := lockDaemon(filepath.Join(c.Dir, "daemon.lock"))
	if e != nil {
		return errors.New("后台状态无法确认且进程锁仍被持有，未进行维护操作")
	}
	unlock()
	return nil
}
