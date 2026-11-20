package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) liveLock(thread string) *sync.Mutex {
	value, _ := b.liveLocks.LoadOrStore(thread, &sync.Mutex{})
	return value.(*sync.Mutex)
}
func (b *Bridge) ensureLiveCursor(ctx context.Context, s store.Session) (store.MirrorCursor, error) {
	c, err := b.store.MirrorCursor(s.ThreadID)
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	path, offset, err := b.engineForThread(s.ThreadID).LiveBaseline(ctx, s.ThreadID)
	if err != nil {
		return c, err
	}
	if err = b.store.InitMirrorCursor(s.ThreadID, path, offset); err != nil {
		return c, err
	}
	return b.store.MirrorCursor(s.ThreadID)
}
func (b *Bridge) registerLiveOrigin(ctx context.Context, s store.Session, msg im.Message, path string, offset int64) error {
	if !s.Managed && !config.UsesDirectCodex(s.Address.Channel) {
		return nil
	}
	lock := b.liveLock(s.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	if path == "" {
		if config.UsesDirectCodex(s.Address.Channel) && !b.desktopDispatch(s.ThreadID) {
			return b.store.RegisterNativeSubmissionIntent(s, msg.ID)
		}
		return diagnostics.UserError("尚未找到会话日志，无法登记消息来源；本条未提交")
	}
	if _, err := b.ensureLiveCursor(ctx, s); err != nil {
		return err
	}
	original, rendered := codex.DesktopInputForms(msg.Text)
	return b.store.RegisterSubmissionIntent(s, msg.ID, path, offset, store.Hash(original), store.Hash(rendered))
}

func (b *Bridge) markdownEnabled(s store.Session) bool {
	_, ok := b.channels[s.Address.Connection].(im.MarkdownAdapter)
	return ok
}

func (b *Bridge) mirrorEntry(message codex.HistoryMessage, offset int64, markdown bool, personal ...bool) store.MirrorEntry {
	body := message.Text
	if message.Role == "user" {
		body = codex.UserMessageTextForDisplay(body)
	}
	body = b.presentationFilter().Text(body)
	if strings.TrimSpace(body) == "" {
		body = "（无可展示文本）"
	}
	label := "🤖 Codex 回复"
	if message.Role == "user" {
		label = "👤 桌面用户"
	}
	max := b.cfg.Limits.MaxMessageChars - 80
	if max < 120 {
		max = 120
	}
	var chunks []string
	if message.Role == "assistant" && markdown {
		chunks = splitMarkdown(body, max)
	} else {
		chars := []rune(body)
		for start := 0; start < len(chars); start += max {
			end := start + max
			if end > len(chars) {
				end = len(chars)
			}
			chunks = append(chunks, string(chars[start:end]))
		}
	}
	parts := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		title := label
		if len(chunks) > 1 {
			title += fmt.Sprintf("（%d/%d）", i+1, len(chunks))
		}
		separator := "\n"
		if message.Role == "assistant" && markdown {
			separator = "\n\n"
		}
		if len(personal) > 0 && personal[0] {
			parts = append(parts, chunk)
		} else {
			parts = append(parts, title+separator+chunk)
		}
	}
	entry := store.MirrorEntry{ItemID: message.ID, Role: message.Role, Fingerprint: store.Hash(strings.TrimSpace(message.Text)), Offset: offset, Parts: parts}
	if message.Role == "user" {
		for _, part := range parts {
			entry.OriginUnknownParts = append(entry.OriginUnknownParts,
				strings.Replace(part, "👤 桌面用户", "👤 会话用户（来源待核对）", 1))
		}
	}
	return entry
}

// 调用者持有同线程锁；只把增量消息写入账本，不在此处发送网络请求。
func (b *Bridge) ingestLive(ctx context.Context, s store.Session) error {
	if config.IsPersonalChannel(s.Address.Channel) {
		return nil
	}
	// 只在当前会话没有历史同步游标时建立基线，不回填启用前的旧文件。
	_, oldErr := b.store.MirrorCursor(s.ThreadID)
	initial := errors.Is(oldErr, sql.ErrNoRows)
	if oldErr != nil && !initial {
		return oldErr
	}
	baseline, err := b.ensureLiveCursor(ctx, s)
	if err != nil {
		return err
	}
	files, scanErr := b.engineForThread(s.ThreadID).LiveRolloutFiles(ctx, s.ThreadID)
	if len(files) == 0 && scanErr != nil {
		return scanErr
	}
	if len(files) == 0 {
		return diagnostics.UserError("会话日志暂不可用，未跳过未读消息")
	}
	var foundBaseline bool
	for _, path := range files {
		if path == baseline.Path || filepath.Base(path) == filepath.Base(baseline.Path) {
			foundBaseline = true
		}
	}
	if !foundBaseline && scanErr == nil {
		// 旧文件已不可读时不能宣称该段完整；仍处理其它身份核验通过的独立文件。
		scanErr = diagnostics.UserError("已记录的会话日志缺失，旧文件尾部待核对；后续文件继续同步")
	}
	for _, path := range files {
		header, size, err := b.engineForThread(s.ThreadID).LiveRolloutInfo(ctx, s.ThreadID, path)
		if err != nil {
			if scanErr == nil {
				scanErr = err
			}
			continue
		}
		if err = b.store.RecordSyncSource(s.ThreadID, path, size); err != nil {
			return err
		}
		start := header
		if initial && path == baseline.Path {
			start = baseline.Offset // 首次启用只从当前文件的最后完整行后读取。
		}
		if initial && filepath.Base(path) < filepath.Base(baseline.Path) {
			start = size // 首次启用前的历史文件不作为新消息重新投递。
		}
		if !initial && path != baseline.Path && filepath.Base(path) == filepath.Base(baseline.Path) {
			start = baseline.Offset // 原文件移动到归档目录，继续同一文件的未读尾部。
		}
		cursor, err := b.store.MirrorFileCursor(s.ThreadID, path, start)
		if err != nil {
			if scanErr == nil {
				scanErr = err
			}
			continue
		}
		for batch := 0; batch < 32; batch++ {
			expected, checkpointErr := b.store.MirrorCheckpoint(s.ThreadID, cursor.Path, cursor.Offset)
			if checkpointErr != nil {
				return checkpointErr
			}
			messages, next, checkpoint, readErr := b.engineForThread(s.ThreadID).ReadLiveMessagesChecked(ctx, s.ThreadID, cursor.Path, cursor.Offset, expected)
			if readErr != nil {
				if scanErr == nil {
					scanErr = readErr
				}
				break
			}
			if next == cursor.Offset && len(messages) == 0 && expected != "" {
				break
			}
			entries := make([]store.MirrorEntry, 0, len(messages))
			issues := make([]store.MirrorReadIssue, 0)
			for _, message := range messages {
				if message.Issue != nil {
					issues = append(issues, store.MirrorReadIssue{Offset: message.Offset, Length: message.Issue.Length,
						Kind: message.Issue.Kind, Fingerprint: message.Issue.Fingerprint})
					continue
				}
				entries = append(entries, b.mirrorEntryForThread(s.ThreadID, message.HistoryMessage, message.Offset, b.markdownEnabled(s)))
			}

			if err = b.store.CommitMirrorBatch(s.ThreadID, cursor, next, entries, issues, checkpoint); err != nil {
				return err
			}
			cursor.Offset = next
			if batch == 31 {
				return diagnostics.UserError("实时消息积压较多，下一轮继续逐文件读取")
			}
		}
	}
	return scanErr
}
func (b *Bridge) flushLive(ctx context.Context, s store.Session) error {
	adapter := b.channels[s.Address.Connection]
	if adapter == nil {
		return diagnostics.UserError("实时消息的 IM 连接未配置")
	}
	if adapter.Capabilities().PersonalInbox {
		if err := b.store.RefreshPersonalDelivery(s.ThreadID); err != nil {
			return err
		}
		if err := b.preparePersonalReviews(ctx, s); err != nil {
			return err
		}
		defer b.store.RefreshPersonalDelivery(s.ThreadID)
	}
	parts, err := b.store.MirrorParts(s.ThreadID, adapter.Capabilities().PersonalInbox)
	if err != nil {
		return err
	}
	var firstFailure error
	blockedItems := map[string]bool{}
	for _, part := range parts {
		if blockedItems[part.ItemID] {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		current, err := b.store.ByThread(s.ThreadID)
		if err != nil {
			return err
		}
		if current.Status != "active" || current.Address.Key() != s.Address.Key() {
			return diagnostics.UserError("会话已关闭或映射改变，停止实时消息投递")
		}
		if part.State != "pending" || part.NextAttempt > time.Now().Unix() {
			continue // 已在账本中隔离或重试等待，不影响其它消息。
		}
		if adapter.Capabilities().PersonalInbox {
			allowed, err := b.store.PersonalPartApproved(s.ThreadID, part.ItemID, b.personalAutoPeers(s))
			if err != nil {
				return err
			}
			if !allowed {
				continue
			}
		}
		claimed, err := b.store.ClaimMirrorPart(part)
		if err != nil {
			return err
		}
		if !claimed {
			return diagnostics.UserError("实时消息已被其他投递流程领取，等待确认")
		}
		key := store.Hash(s.Address.Key() + "\x00" + s.ThreadID + "\x00" + part.ItemID + fmt.Sprintf(":live:%d", part.Index))[:32]
		if adapter.Capabilities().PersonalInbox {
			if part.Role != "assistant" {
				return diagnostics.UserError("个人渠道不转发用户历史消息")
			}
			key = "personal-reply:" + key
		}
		var id string
		var sendErr error
		if markdown, ok := adapter.(im.MarkdownAdapter); ok && part.Role == "assistant" {
			id, sendErr = markdown.SendMarkdown(ctx, s.Address, part.Body, key)
		} else {
			id, sendErr = adapter.SendText(ctx, s.Address, part.Body, key)
		}
		if sendErr != nil || id == "" {
			var uncertain *im.DeliveryUncertain
			unknown := id == "" && sendErr == nil || errors.As(sendErr, &uncertain) || errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded)
			hint := "实时消息投递失败：" + diagnostics.Error(sendErr)
			if unknown {
				hint = "本片段送达结果不确定，可能已送达；保留待核对，不自动重发，后续消息继续同步"
			}
			if err = b.store.MirrorPartFailed(part, hint, unknown); err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if unknown {
				// 只跳过未知片段；本次请求是否完整送达仍由回复清单逐项核验。
				continue
			}
			blockedItems[part.ItemID] = true
			if firstFailure == nil {
				firstFailure = diagnostics.UserError(hint)
			}
			continue
		}
		if err = b.store.MirrorPartSent(part, id); err != nil {
			return err
		}
		if adapter.Capabilities().PersonalInbox && !personalDirect(s.Address) {
			if e := b.store.RecordGroupReplyReference(s, part.ItemID, id); e != nil {
				b.log.Warn("群回复已发送，引用关联保存失败；不重发消息")
			}
		}
	}
	return firstFailure
}
func (b *Bridge) syncLiveThread(ctx context.Context, s store.Session) error {
	lock := b.liveLock(s.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	defer b.reconcileRequestReplies(s)
	// 各阶段独立预算；读取或旧记录恢复失败时，已入账消息仍获得投递机会。
	readCtx, stopRead := context.WithTimeout(ctx, 10*time.Second)
	readErr := b.ingestLive(readCtx, s)
	stopRead()
	if err := b.store.RecordSyncStage(s.ThreadID, "read", syncErrorDetail(readErr)); err != nil {
		return err
	}
	writeCtx, stopWrite := context.WithTimeout(ctx, 20*time.Second)
	writeErr := b.flushLive(writeCtx, s)
	stopWrite()
	if err := b.store.RecordSyncStage(s.ThreadID, "delivery", syncErrorDetail(writeErr)); err != nil {
		return err
	}
	recoveryCtx, stopRecovery := context.WithTimeout(ctx, 3*time.Second)
	recoveryErr := b.recoverLiveCompletions(recoveryCtx, s)
	stopRecovery()
	if err := b.store.RecordSyncStage(s.ThreadID, "recovery", syncErrorDetail(recoveryErr)); err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	if writeErr != nil {
		return writeErr
	}
	return recoveryErr
}
func (b *Bridge) deliverLiveReplies(ctx context.Context, s store.Session, replies []codex.HistoryMessage) error {
	lock := b.liveLock(s.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	defer b.reconcileRequestReplies(s)
	if err := ctx.Err(); err != nil {
		return err
	}
	if config.IsPersonalChannel(s.Address.Channel) {
		return b.flushLive(ctx, s)
	}
	for _, reply := range replies {
		if reply.ID == "" {
			return diagnostics.UserError("助手回复缺少消息 ID，无法安全关联投递；不会自动重发任务")
		}
		if err := b.store.EnqueueMirrorReply(s.ThreadID, b.mirrorEntry(reply, 0, b.markdownEnabled(s), config.IsPersonalChannel(s.Address.Channel))); err != nil {
			return err
		}
	}
	if err := b.flushLive(ctx, s); err != nil {
		return err
	}
	for _, reply := range replies {
		sent, err := b.store.MirrorReplySent(s.ThreadID, reply.ID)
		if err != nil {
			return err
		}
		if !sent {
			return diagnostics.UserError("回复已排入实时投递队列，尚未全部确认送达；请勿重发任务")
		}
	}
	return nil
}
func syncErrorDetail(err error) string {
	if err == nil {
		return ""
	}
	return diagnostics.Error(err)
}

func (b *Bridge) syncLiveMessages(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		sessions, err := b.store.LiveMirrorSessions()
		if err == nil {
			jobs := make(chan store.Session)
			var workers sync.WaitGroup
			for n := 0; n < 3; n++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for s := range jobs {
						op, cancel := context.WithTimeout(ctx, 35*time.Second)
						_ = b.syncLiveThread(op, s)
						cancel()
					}
				}()
			}
			for _, s := range sessions {
				if ctx.Err() != nil {
					break
				}
				jobs <- s
			}
			close(jobs)
			workers.Wait()
		}
		rows, healthErr := b.store.SyncHealth()
		b.diagMu.Lock()
		b.diag.SyncCheckedAt = time.Now().Unix()
		if err != nil || healthErr != nil {
			b.diag.LivePhase = "degraded"
			b.diag.LiveSync = "读取同步任务或诊断账本失败，等待下一轮重试"
		} else {
			b.diag.Sessions = rows
			attention, pending, unknown := 0, 0, 0
			for _, r := range rows {
				pending += r.Pending
				unknown += r.Unknown
				if r.ReadAt == 0 || time.Now().Unix()-r.ReadAt > 90 || r.ReadError != "" || r.DeliveryError != "" || r.RecoveryError != "" || r.Unknown > 0 || r.ReadIssues > 0 || r.HistoryError != "" {
					attention++
				}
			}
			b.diag.LivePhase = "running"
			if attention > 0 {
				b.diag.LivePhase = "degraded"
			}
			b.diag.LiveSync = fmt.Sprintf("%d 个会话；%d 个需核对；待发送 %d 个片段，送达不确定 %d 个片段。详细读取和投递进度见会话同步页。", len(rows), attention, pending, unknown)
		}
		b.diagMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// 只改变投递账本，不提交或恢复模型任务；与原线程发送共用锁。
func (b *Bridge) ResolveDelivery(r store.DeliveryResolution) error {
	lock := b.liveLock(r.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	if e := b.store.ResolveDelivery(r); e != nil {
		return e
	}
	if s, e := b.store.ByThread(r.ThreadID); e == nil && s.Status == "active" {
		b.reconcileRequestReplies(s)
	}
	return nil
}

func (b *Bridge) recoverLiveCompletions(ctx context.Context, s store.Session) error {
	items, err := b.store.PendingCompletionRecoveries(s.ThreadID)
	if err != nil {
		return err
	}
	var firstFailure error
	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		replies, readErr := b.engineForThread(s.ThreadID).RecoverLiveCompletion(ctx, s.ThreadID, item.Path, item.Offset, item.Length, item.Fingerprint)
		if ctx.Err() != nil {
			// 已尝试的慢记录也必须让出位置，不能每轮耗尽预算后仍排在队首。
			if err = b.store.RetryCompletionRecovery(item, diagnostics.Error(ctx.Err())); err != nil {
				return err
			}
			return ctx.Err()
		}
		if readErr != nil && !codex.CompletionEvidenceRejected(readErr) {
			if err = b.store.RetryCompletionRecovery(item, diagnostics.Error(readErr)); err != nil {
				return err
			}
			if firstFailure == nil {
				firstFailure = readErr
			}
			continue
		}
		var entries []store.MirrorEntry
		if readErr == nil {
			for _, reply := range replies {
				entries = append(entries, b.mirrorEntryForThread(s.ThreadID, reply.HistoryMessage, reply.Offset, b.markdownEnabled(s)))
			}
		}
		if err = b.store.SaveCompletionRecovery(item, entries, readErr == nil); err != nil {
			return err
		}
	}
	return firstFailure
}
