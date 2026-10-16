package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"im-agent/internal/daemon"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"im-agent/internal/desktop"
)

//go:embed frontend/index.html frontend/app.js frontend/style.css
var assets embed.FS

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--background-start" {
		if err := startBackground(); err != nil {
			fmt.Fprintln(os.Stderr, "后台启动未确认，请打开 IM Agent 查看运行状态：", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "--service" {
		root, err := os.UserConfigDir()
		if err != nil {
			os.Exit(1)
		}
		flags := flag.NewFlagSet("service", flag.ExitOnError)
		dir := flags.String("data-dir", filepath.Join(root, "IM Agent"), "本机配置目录")
		_ = flags.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err = daemon.Run(ctx, *dir); err != nil {
			fmt.Fprintln(os.Stderr, "后台退出：", err)
			os.Exit(1)
		}
		return
	}

	app := desktop.New()
	ui, err := fs.Sub(assets, "frontend")
	if err != nil {
		fmt.Fprintln(os.Stderr, "无法加载界面资源")
		os.Exit(1)
	}
	err = wails.Run(&options.App{
		Title: "IM Agent", Width: 1220, Height: 820, MinWidth: 900, MinHeight: 660,
		BackgroundColour: options.NewRGB(246, 247, 249),
		AssetServer:      &assetserver.Options{Assets: ui},
		OnStartup:        app.Startup, OnShutdown: app.Shutdown, OnBeforeClose: app.BeforeClose,
		Bind:     []interface{}{desktop.NewAPI(app)},
		LogLevel: logger.ERROR, LogLevelProduction: logger.ERROR,
		SingleInstanceLock: &options.SingleInstanceLock{UniqueId: "io.im-agent.desktop"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "桌面客户端启动失败，请检查系统 WebView 与运行环境")
		os.Exit(1)
	}
}
