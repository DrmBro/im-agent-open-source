// 命令行只运行客户端保存的 SQLite 配置，不再从 JSON/环境变量获取 IM 凭证。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"im-agent/internal/channels"
	"im-agent/internal/channels/feishu"
	"im-agent/internal/daemon"
	"im-agent/internal/preferences"
	"im-agent/internal/service"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	root, e := os.UserConfigDir()
	if e != nil {
		return errors.New("无法找到客户端数据目录")
	}
	background := flag.Bool("service", false, "运行本机后台控制服务")
	dir := flag.String("data-dir", filepath.Join(root, "IM Agent"), "客户端 SQLite 所在目录（不导入 JSON 或环境凭证）")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("已取消 JSON 启动配置；请先在桌面客户端导入、填写凭证并保存，然后运行 im-agent-cli")
	}
	if *background {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return daemon.Run(ctx, *dir)
	}
	registry := channels.NewRegistry()
	if e = feishu.Register(registry); e != nil {
		return e
	}
	p, e := preferences.New(*dir, registry)
	if e != nil {
		return errors.New("无法打开客户端配置数据库")
	}
	defer p.Close()
	cfg, revision, e := p.Runtime()
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	control := service.New(registry)
	if e = control.Start(ctx, cfg, revision); e != nil {
		return e
	}
	fmt.Fprintln(os.Stdout, "已使用客户端配置启动桥接；IM 凭证仅来自 SQLite。按 Control+C 停止。")
	if e = control.Wait(context.Background()); e != nil {
		return e
	}
	state := control.Status()
	if state.Phase == "failed" {
		return errors.New(state.Message)
	}
	return nil
}
