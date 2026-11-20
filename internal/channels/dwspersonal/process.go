package dwspersonal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"im-agent/internal/im"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// DWS v1.0.68 同时存在原始 payload 与统一 outcome/data 信封。
func successPayload(raw []byte) ([]byte, error) {
	var envelope struct {
		Outcome string          `json:"outcome"`
		OK      bool            `json:"ok"`
		DryRun  bool            `json:"dry_run"`
		Data    json.RawMessage `json:"data"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.DryRun || len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return nil, errors.New("DWS 响应未确认成功")
	}
	if envelope.Outcome != "" {
		if !envelope.OK || envelope.Outcome != "success" || len(envelope.Data) == 0 {
			return nil, errors.New("DWS 操作未完成")
		}
		return envelope.Data, nil
	}
	return raw, nil
}

// 输出只供内存解析，禁止透传包含个人消息或 OAuth 信息的原始错误。
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("DWS 输出超过限制")
	}
	return b.Buffer.Write(p)
}

func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	all := append([]string{"--profile", c.profile, "--format", "json", "--timeout", "20"}, args...)
	cmd := exec.CommandContext(ctx, c.binary, all...)
	cmd.Env = []string{}
	// 不继承调用环境中的默认账号、App Secret 或 MCP 地址覆盖。
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(key, "DWS_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

func (c *Client) run(ctx context.Context, input string, args ...string) ([]byte, error) {
	return c.runInDirectory(ctx, input, "", 35*time.Second, args...)
}
func (c *Client) runInDirectory(ctx context.Context, input, dir string, budget time.Duration, args ...string) ([]byte, error) {
	op, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd := c.command(op, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdin = strings.NewReader(input)
	var out limitedBuffer
	out.limit = 1 << 20
	cmd.Stdout = &out
	var diagnostic limitedBuffer
	diagnostic.limit = 64 << 10
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		// CLI 的错误信封写入 stderr；只识别固定错误类别，不输出原文。
		if rejectedDWSRequest(out.Bytes()) || rejectedDWSRequest(diagnostic.Bytes()) {
			return nil, &im.DeliveryRejected{Code: "invalid_request"}
		}
		return nil, errors.New("DWS 调用未确认成功；请检查登录、权限和本机状态")
	}
	return out.Bytes(), nil
}

// runPartial 与 runInDirectory 相同，但命令以非零码退出时仍返回标准输出，
// 供调用方解析 DWS 的 partial_failure 结果。
func (c *Client) runPartial(ctx context.Context, budget time.Duration, args ...string) ([]byte, error) {
	op, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd := c.command(op, args...)
	var out limitedBuffer
	out.limit = 4 << 20
	cmd.Stdout = &out
	var diagnostic limitedBuffer
	diagnostic.limit = 64 << 10
	cmd.Stderr = &diagnostic
	if err := cmd.Run(); err != nil {
		if out.Len() > 0 {
			return out.Bytes(), errors.New("DWS 只返回了部分结果")
		}
		return nil, errors.New("DWS 调用未确认成功；请检查登录、权限和本机状态")
	}
	return out.Bytes(), nil
}

func rejectedDWSRequest(raw []byte) bool {
	var failure struct {
		Error struct {
			Category string `json:"category"`
			Reason   string `json:"reason"`
			Origin   string `json:"origin"`
			Stage    string `json:"stage"`
		} `json:"error"`
	}
	// stderr 可能先有运行环境提示，从最后一个独立 JSON 对象起点读取。
	start := bytes.LastIndex(raw, []byte("\n{\n"))
	if start >= 0 {
		raw = raw[start+1:]
	}
	if json.Unmarshal(raw, &failure) != nil {
		return false
	}
	return failure.Error.Category == "api" && failure.Error.Reason == "invalid_request"
}
