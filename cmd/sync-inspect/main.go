// sync-inspect 输出会话最新消息和状态的一致性元数据，不输出正文。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"time"

	"im-agent/internal/buildinfo"
	"im-agent/internal/codex"
)

func main() {
	root, _ := os.UserHomeDir()
	home := flag.String("home", filepath.Join(root, ".codex"), "本机 Codex 日志目录")
	flag.Parse()
	type row struct {
		codex.LogInspection
		Error string `json:"error,omitempty"`
	}
	rows := []row{}
	failed := false
	for _, thread := range flag.Args() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		result, err := codex.InspectSessionLog(ctx, *home, thread)
		cancel()
		next := row{LogInspection: result}
		if err != nil {
			next.Error = err.Error()
			failed = true
		}
		rows = append(rows, next)
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Build    buildinfo.Info `json:"build"`
		Sessions []row          `json:"sessions"`
	}{buildinfo.Current(), rows})
	if failed {
		os.Exit(1)
	}
}
