package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

// watchCodex 是可选的 Codex→IM 发现器。Codex CLI 没有新会话事件，因此以受限轮询替代。
// 有可选默认项目时限制该目录；留空时可导入多个目录，逐条保留实际 cwd。
func (b *Bridge) watchCodex(ctx context.Context) {
	watchKey := "watch_started:" + b.cfg.Watch.ConnectionID
	started, err := b.store.GetMeta(watchKey)
	if err != nil || started == "" {
		_ = b.store.SetMeta(watchKey, fmt.Sprintf("%d", time.Now().Unix()))
	}
	interval := time.Duration(b.cfg.Watch.IntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.importCodexThreads(ctx)
		}
	}
}
func (b *Bridge) importCodexThreads(ctx context.Context) {
	b.bindMu.Lock()
	defer b.bindMu.Unlock()
	b.scanDiagnostic("listing", "正在查询 Codex 会话列表")
	scanCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	threads, err := b.codex.List(scanCtx, b.cfg.WorkDir)
	cancel()
	if err != nil {
		b.scanDiagnostic("failed", "查询 Codex 会话失败："+diagnostics.Error(err))
		return
	}
	cc := b.conn(b.cfg.Watch.ConnectionID)
	adapter := b.channels[cc.ID]
	startedText, _ := b.store.GetMeta("watch_started:" + cc.ID)
	started, _ := strconv.ParseInt(startedText, 10, 64)
	if adapter == nil {
		b.scanDiagnostic("failed", "自动发现的目标 IM 连接不存在")
		return
	}
	bound, missing, filtered, created := 0, 0, 0, 0
	for _, thread := range threads {
		if thread.ID == "" || (b.cfg.WorkDir != "" && thread.Cwd != b.cfg.WorkDir) {
			filtered++
			continue
		}
		if !b.cfg.Watch.ImportExisting && started > 0 && thread.CreatedAt < started && thread.UpdatedAt < started {
			filtered++
			continue
		}
		if _, err := b.store.ByThread(thread.ID); err == nil {
			bound++
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			b.scanDiagnostic("failed", "读取会话映射数据库失败")
			return
		}
		if owned, e := b.store.CreationOwnedThread(thread.ID); e != nil {
			b.scanDiagnostic("failed", "读取创建表单的会话归属失败")
			return
		} else if owned {
			filtered++
			continue
		}
		// 桌面主会话可以继续聊天，历史 cwd 不存在不影响按会话 ID 建群。
		// 只保存 Codex 原目录元数据，不新建/替换项目目录，不再逐会话放行。
		workDir := thread.Cwd
		if b.cfg.Codex.SubmissionMode != "native_queue" && b.cfg.Codex.SubmissionMode != "desktop_queue" {
			var e error
			workDir, e = config.ExistingDirectory(thread.Cwd)
			if e != nil {
				missing++
				continue
			}
		}
		owner := ""
		if len(cc.AdminUsers) > 0 {
			owner = cc.AdminUsers[0]
		}
		b.scanDiagnostic("creating_chat", fmt.Sprintf("已发现 %d 个会话，正在请求 IM 创建群聊/话题", len(threads)))
		address, err := b.newSessionAddress(ctx, adapter, im.Message{Address: adapter.Identity(), ID: "watch:" + thread.ID, UserID: owner}, thread)
		if err != nil {
			b.scanDiagnostic("failed", "创建 IM 会话容器失败："+diagnostics.Error(err)+"。请检查机器人建群权限及管理员 Open ID 是否属于同一应用")
			return
		}
		if err := b.store.Save(store.Session{ThreadID: thread.ID, Address: address, WorkDir: workDir, Owner: owner, Managed: true, GroupName: b.threadName(thread, cc.ID)}); err != nil {
			b.scanDiagnostic("failed", "群聊/话题已创建，但会话映射保存失败")
			return
		}
		created++
		_, _ = adapter.SendText(ctx, address, fmt.Sprintf("已同步 Codex 会话 `%s`。%s", shortID(thread.ID), sessionClosureHint(address)), "")
	}
	b.scanDiagnostic("completed", fmt.Sprintf("本轮发现 %d 个会话；新增映射 %d，已有映射 %d，目录失效 %d，条件过滤 %d", len(threads), created, bound, missing, filtered))
}
