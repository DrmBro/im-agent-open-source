package codex

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"im-agent/internal/config"
)

// PreparePolicy 不修改持久配置。新版移除了 untrusted 时收紧为只读沙箱，
// 不把旧策略静默改成可写 on-request，更不使用 never 或无沙箱。
func PreparePolicy(parent context.Context, cfg config.Config) (config.Config, string, error) {
	if cfg.Codex.ApprovalPolicy != "untrusted" {
		return cfg, "", nil
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, cfg.Codex.Binary, "--help")
	var output bytes.Buffer
	command.Stdout = &output
	if e := command.Run(); e != nil {
		return cfg, "", errors.New("无法确认 Codex 审批兼容性，未启动执行服务")
	}
	if strings.Contains(output.String(), "untrusted") {
		return cfg, "", nil
	}
	cfg.Codex.ApprovalPolicy = "on-request"
	cfg.Codex.Sandbox = "read-only"
	return cfg, "当前 Codex 已移除旧 untrusted 策略。本次运行收紧为只读沙箱，保留按需人工审批；未修改保存的配置，也未开启自动审批或工作区写入。", nil
}
