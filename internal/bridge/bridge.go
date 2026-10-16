package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/queue"
	"im-agent/internal/store"
)

type Bridge struct {
	controlJobs         *queue.Queue
	controlLocks        sync.Map
	controlScheduled    sync.Map
	inboundScheduled    sync.Map
	reportJobs          *queue.Queue
	reportScheduled     sync.Map
	reportOperations    map[string]map[string]context.CancelFunc // 由mu保护，与关闭意图同步。
	statusCardDirty     sync.Map
	statusCardJobs      *queue.Queue
	statusCardScheduled sync.Map
	statusCardLocks     sync.Map

	liveLocks         sync.Map
	imageLocks        sync.Map
	cfg               config.Config
	store             *store.Store
	codex             *codex.Client
	channels          map[string]im.Adapter
	jobs              *queue.Queue
	log               *slog.Logger
	mu                sync.Mutex
	turns             map[string]string
	approvals         map[string]chan approvalResult
	active            map[string]*execution
	closingWake       chan struct{}
	lifecycleStatusMu sync.Mutex
	lifecycleWarning  string
	bindMu            sync.Mutex
	topicMu           sync.Mutex
	diagMu            sync.Mutex
	diag              Diagnostics
}
type approvalResult struct{ Value any }

func New(cfg config.Config, db *store.Store, cx *codex.Client, adapters map[string]im.Adapter, logger *slog.Logger) *Bridge {
	return &Bridge{cfg: cfg, store: db, codex: cx, channels: adapters, log: logger, turns: map[string]string{}, approvals: map[string]chan approvalResult{}, active: map[string]*execution{}, closingWake: make(chan struct{}, 1)}
}

// Start 管理所有连接、队列和扫描器，停止后等待它们退出，再交还数据库所有权。
func (b *Bridge) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	b.jobs = queue.New(ctx, b.cfg.Limits.Workers, b.cfg.Limits.QueueSize)
	if err := b.store.RecoverInboundStates(); err != nil {
		return err
	}
	b.statusCardJobs = queue.New(ctx, 2, 32)
	b.reportJobs = queue.New(ctx, 1, 8)
	b.controlJobs = queue.New(ctx, 2, 32)

	b.codex.SetHandler(b.handleCodexRequest)
	for _, adapter := range b.channels {
		if lifecycle, ok := adapter.(im.LifecycleAdapter); ok {
			lifecycle.SetClosedHandler(b.conversationClosed)
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, len(b.channels))
	wg.Add(1)
	go func() { defer wg.Done(); b.syncClosures(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.watchArchiveEvents(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncPresentations(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncLiveMessages(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.watchImageDrafts(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncSessionStatusCards(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncRequestRecovery(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncHistoryRetention(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncRequestTelemetry(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncRequestOrigins(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncInboundQueue(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.syncInboundHistory(ctx) }()

	if b.cfg.Watch.Enabled {
		wg.Add(1)
		go func() { defer wg.Done(); b.watchCodex(ctx) }()
	}
	for id, adapter := range b.channels {
		id, adapter := id, adapter
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := adapter.Start(ctx, b.receiveMessage, b.receiveInteraction)
			if ctx.Err() == nil {
				b.log.Error("IM 连接退出", "connection", id)
				if err == nil {
					err = errors.New("IM 连接意外停止")
				}
				select {
				case failures <- err:
				default:
				}
			}
		}()
	}
	var result error
	select {
	case <-parent.Done():
		result = parent.Err()
	case <-b.codex.Done():
		result = errors.New("Codex App Server 已退出")
	case result = <-failures:
	}
	cancel()
	wg.Wait()
	b.jobs.Wait()
	b.statusCardJobs.Wait()
	b.reportJobs.Wait()
	b.controlJobs.Wait()
	return result
}
func (b *Bridge) receiveMessage(ctx context.Context, msg im.Message) error {
	adapter, ok := b.channels[msg.Address.Connection]
	if !ok {
		return errors.New("未找到消息连接")
	}
	if !b.allowed(msg.Address.Connection, msg.UserID) {
		return nil
	}
	target, retired, err := b.store.RetiredTarget(msg.Address)
	if err != nil {
		return err
	}
	if retired {
		location := "独立群"
		if target.IsTopic() {
			location = "话题"
		}
		name := target.ChatID
		if binding, e := b.store.ByChat(target); e == nil {
			if binding.Status != "active" {
				_, err = adapter.SendText(ctx, msg.Address, "此入口仅保留历史，对应会话已归档或正在关闭，不再执行消息。", msg.ID+":migrated")
				return err
			}
			if presentation, e := b.store.Presentation(binding.ThreadID); e == nil && presentation.Name != "" {
				name = presentation.Name
			}
		}
		_, err = adapter.SendText(ctx, msg.Address, "此入口已迁移，只保留历史，不再执行消息或命令。请到"+location+"「"+name+"」继续。", msg.ID+":migrated")
		return err
	}
	if len([]rune(msg.Text)) > b.cfg.Limits.MaxInputChars {
		_, _ = adapter.SendText(ctx, msg.Address, fmt.Sprintf("消息超过 %d 个字符，请拆分后重试。", b.cfg.Limits.MaxInputChars), msg.ID)
		return nil
	}
	intake := b.imageIntakeLock(msg)
	intake.Lock()
	defer intake.Unlock()
	b.messageDiagnostic("received", "已收到白名单用户的消息，正在处理")
	claimed, err := b.store.ClaimInbound(msg)
	if err != nil || !claimed {
		return err
	}
	return b.processClaimedInbound(ctx, adapter, msg)
}

// 由接收回调或启动后的持久收件箱调用；调用方持有该地址与用户的图片锁。
func (b *Bridge) processClaimedInbound(ctx context.Context, adapter im.Adapter, msg im.Message) error {
	if err := b.store.EventStage(msg.ID, msg.Address.Key(), "received", ""); err != nil {
		return err
	}
	b.expireImages(ctx)
	prepared, handled, prepareErr := b.prepareImageBatch(ctx, adapter, msg)
	if prepareErr != nil {
		_ = b.store.EventStage(msg.ID, msg.Address.Key(), "image_batch_failed", diagnostics.Error(prepareErr))
		_ = b.store.EventDone(msg.ID, "failed")
		_, _ = adapter.SendText(ctx, msg.Address, "附件组合未提交："+diagnostics.Error(prepareErr), msg.ID+":image-error")
		return prepareErr
	}
	if handled {
		return nil
	}
	msg = prepared
	if len(msg.Attachments) == 0 && strings.HasPrefix(msg.Text, "/") {
		if err := b.store.EventStage(msg.ID, msg.Address.Key(), "command_running", ""); err != nil {
			return err
		}
		if err := b.command(ctx, adapter, msg); err != nil {
			_ = b.store.EventStage(msg.ID, msg.Address.Key(), "command_failed", diagnostics.Error(err))
			b.messageDiagnostic("failed", "命令处理失败："+diagnostics.Error(err)+"；请查看同步阶段及飞书权限")
			_, _ = adapter.SendText(ctx, msg.Address, "命令处理失败："+diagnostics.Error(err)+"。请回客户端查看运行诊断。", store.Hash(msg.Address.Key() + msg.ID + "error")[:32])
			_ = b.store.EventDone(msg.ID, "failed")
			_ = b.store.FinishImageDraft(msg.ID, "failed")
			return err
		}
		_ = b.store.EventStage(msg.ID, msg.Address.Key(), "command_completed", "")
		b.messageDiagnostic("completed", "命令已处理")
		return b.store.EventDone(msg.ID, "done")
	}
	session, err := b.getOrCreateSession(ctx, adapter, msg)
	if err != nil {
		_ = b.store.EventStage(msg.ID, msg.Address.Key(), "binding_failed", diagnostics.Error(err))
		b.messageDiagnostic("failed", "创建或读取会话失败："+diagnostics.Error(err))
		hint := "创建会话失败：" + diagnostics.Error(err) + "。请检查客户端诊断及建群权限。"
		var userHint diagnostics.UserError
		if errors.As(err, &userHint) {
			hint = string(userHint)
		}
		_, _ = adapter.SendText(ctx, msg.Address, hint, store.Hash(msg.Address.Key() + msg.ID + "error")[:32])
		_ = b.store.EventDone(msg.ID, "failed")
		_ = b.store.FinishImageDraft(msg.ID, "failed")
		return err
	}
	if err = b.store.QueueInbound(session, msg); err != nil {
		return err // 原始输入已落盘，不丢弃；不能假装排队成功。
	}
	if err = b.scheduleInbound(store.QueuedInbound{EventID: msg.ID, ThreadID: session.ThreadID,
		AddressKey: store.Hash(session.Address.Key()), Message: msg}); err != nil {
		// 不丢弃持久收件：队列满等待容量；其它暂时故障由扫描器重试并报告。
		b.log.Warn("请求已持久化，等待调度", "event", msg.ID, "error", diagnostics.Error(err))
	}
	_ = b.enqueueSessionStatus(session.ThreadID, false, msg.ID)
	return nil // 入站内容已持久化；队列满只推迟调度，不要求平台再次投递。
}
func (b *Bridge) allowed(connection, user string) bool {
	for _, c := range b.cfg.Connections {
		if c.ID == connection {
			return config.Contains(c.AllowedUsers, user)
		}
	}
	return false
}
func (b *Bridge) conn(id string) config.Connection {
	for _, c := range b.cfg.Connections {
		if c.ID == id {
			return c
		}
	}
	return config.Connection{}
}
func (b *Bridge) getOrCreateSession(ctx context.Context, adapter im.Adapter, msg im.Message) (store.Session, error) {
	// 已有群的消息无需等待后台扫描/建群持有的全局创建锁。
	if existing, e := b.store.ByChat(msg.Address); e == nil {
		if existing.Status != "active" {
			return store.Session{}, diagnostics.UserError("此会话已关闭或正在归档，不再接受执行任务")
		}
		return existing, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return store.Session{}, e
	}
	b.bindMu.Lock()
	defer b.bindMu.Unlock()
	s, err := b.store.ByChat(msg.Address)
	if err == nil {
		if s.Status != "active" {
			return store.Session{}, diagnostics.UserError("此会话已关闭或正在归档，不再接受执行任务")
		}
		return s, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return store.Session{}, err
	}
	if msg.Address.ChatID == "" {
		return store.Session{}, errors.New("消息没有会话 ID")
	}
	if msg.Address.IsTopic() {
		return store.Session{}, diagnostics.UserError("此话题未绑定 Codex，请用 /new 创建会话话题；不会自动绑定其他会话")
	}
	shared, checkErr := b.isSharedChat(msg.Address)
	if checkErr != nil {
		return store.Session{}, checkErr
	}
	if shared || (!msg.IsDirect && b.conn(msg.Address.Connection).ConversationMode == "topic") {
		return store.Session{}, diagnostics.UserError("请在已绑定的话题内发送消息，或发送 /new 创建新话题；群主界面不会执行模型任务")
	}
	t, err := b.codex.StartThread(ctx, b.cfg)
	if err != nil {
		return store.Session{}, err
	}
	address := msg.Address
	if msg.IsDirect {
		address, err = b.newSessionAddress(ctx, adapter, msg, t)
		if err != nil {
			return store.Session{}, err
		}
	}
	s = store.Session{ThreadID: t.ID, Address: address, WorkDir: t.Cwd, Owner: msg.UserID, Managed: msg.IsDirect && address.Key() != msg.Address.Key(), GroupName: b.threadName(t, msg.Address.Connection), Fresh: true}
	if err := b.store.Save(s); err != nil {
		return store.Session{}, err
	}
	b.sessionCreatedNotice(ctx, adapter, msg, s)
	b.mu.Lock()
	b.turns[t.ID] = ""
	b.mu.Unlock()
	_, _ = adapter.SendText(ctx, address, fmt.Sprintf("已创建 Codex 会话 `%s`。", shortID(t.ID)), msg.ID)
	return s, nil
}
func (b *Bridge) runTurn(ctx context.Context, adapter im.Adapter, s store.Session, msg im.Message) (runErr error) {
	// 仅为返回错误附加已确认的阶段，下面的发送/中断调用与顺序不变。
	failureKind := requestNotSent
	defer func() {
		if runErr != nil {
			runErr = &requestFailure{kind: failureKind, cause: runErr}
		}
	}()
	_ = b.store.EventStage(msg.ID, s.Address.Key(), "starting", "")
	_, _ = adapter.SendText(ctx, s.Address, "已收到，正在处理…", msg.ID)
	turnCtx, finish, err := b.beginExecution(ctx, s.ThreadID)
	if err != nil {
		return err
	}
	defer finish()
	archived, err := b.codex.Archived(turnCtx)
	if err != nil {
		return diagnostics.UserError("无法确认归档状态，暂不恢复该会话")
	}
	for _, t := range archived {
		if t.ID == s.ThreadID {
			_ = b.requestClose(s.ThreadID, "codex", true, false)
			return diagnostics.UserError("Codex 会话已归档，已停止恢复并同步关闭")
		}
	}
	_ = b.store.EventStage(msg.ID, s.Address.Key(), "resuming", "")
	resumed, err := b.codex.ResumeThread(turnCtx, b.cfg, s.ThreadID)
	if err != nil {
		return err
	}
	if err = b.store.UpdateWorkDir(s.ThreadID, resumed.Cwd); err != nil {
		return err
	}
	notices, closeSub, err := b.codex.Subscribe(s.ThreadID)
	if err != nil {
		return err
	}
	defer closeSub()
	if len(msg.Attachments) > 0 {
		_ = b.store.EventStage(msg.ID, s.Address.Key(), "downloading_attachment", "")
		text, e := b.receiveAttachments(turnCtx, adapter, s, msg)
		if e != nil {
			return e
		}
		msg.Text = text
	}
	_ = b.store.EventStage(msg.ID, s.Address.Key(), "submitting", "")
	failureKind = requestSubmissionUnknown
	// 提交回调只发布有界快照，不在桌面提交锁内等待数据库或网络。
	progress, stopProgress := b.observeDesktopProgress(turnCtx, adapter, s, msg)
	defer stopProgress()
	draftRecoveryNeeded := false
	modelPreference, preferenceErr := b.store.SessionModel(s)
	if preferenceErr != nil {
		failureKind = requestNotSent
		return diagnostics.UserError("无法读取会话模型设置，本条未提交")
	}
	turnID, err := b.codex.TurnWithHooks(turnCtx, s.ThreadID, msg.Text, codex.TurnHooks{
		Model: modelPreference.Model, Effort: modelPreference.Effort,
		Progress:            progress,
		RequestID:           msg.ID,
		DraftRecoveryNeeded: func() { draftRecoveryNeeded = true },
		BeforeSubmit: func(path string, offset int64) error {
			return b.registerLiveOrigin(turnCtx, s, msg, path, offset)
		},
	})
	// 此时原桌面提交锁已经释放。先停止并收拢观察，再写提交结果，避免迟到覆盖。
	stopProgress()
	if draftRecoveryNeeded {
		b.enqueueDraftRecoveryNotice(ctx, adapter, s, msg)
	}
	if err != nil {
		var guardFailure *codex.DesktopGuardFailure
		if errors.As(err, &guardFailure) {
			// 只保存固定阶段/类别，旧错误不再丢失是哪项检查失败的证据。
			if savedErr := b.store.SetMeta("desktop-submit-diagnostic:"+msg.ID, store.JSON(map[string]any{
				"action": guardFailure.Action, "reason": guardFailure.Reason, "at": time.Now().Unix(),
			})); savedErr != nil {
				b.log.Warn("桌面提交检查类别持久化失败")
			}
		}
		var known diagnostics.UserError
		// 当前TurnWithHooks中的固定UserError均来自发送前保护；
		// Enter后不确定回执有独立类型，不可推断为未发送。RPC/通信错误保守处理。
		var unknown *codex.DesktopOutcomeUnconfirmed
		if errors.As(err, &known) && !errors.As(err, &unknown) {
			failureKind = requestNotSent
		}
		if failureKind == requestNotSent {
			_ = b.store.CancelMirrorOrigin(msg.ID)
		}
		return err
	}
	failureKind = requestOutcomeUnknown
	if e := b.store.RecordRequestSubmission(s, msg.ID, turnID); e != nil {
		// 输入已经提交，记录失败不能触发第二次提交；继续原有轮次观察。
		b.log.Warn("请求提交回执持久化失败，重启后可能无法恢复观察", "event", msg.ID, "error", diagnostics.Error(e))
	}
	_ = b.store.EventStage(msg.ID, s.Address.Key(), "waiting_model", "")
	_ = b.enqueueSessionStatus(s.ThreadID, false, msg.ID)
	b.messageDiagnostic("running", "输入已提交，等待桌面接收并完成对应轮次；当前会话忙碌时可能排队，不自动重发")
	var answer strings.Builder
	var replies []codex.HistoryMessage
	completedItems := map[string]bool{}
	replyIdentityIncomplete := false
	for {
		select {
		case <-turnCtx.Done():
			interruptCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			interruptErr := b.codex.Interrupt(interruptCtx, s.ThreadID, turnID)
			stop()
			var unconfirmed *codex.DesktopOutcomeUnconfirmed
			if errors.As(interruptErr, &unconfirmed) {
				return unconfirmed
			}
			return turnCtx.Err()
		case n, ok := <-notices:
			if !ok {
				return diagnostics.UserError("Codex 会话连接已关闭")
			}
			ev, ok := codex.DecodeThreadEvent(n)
			if !ok || (ev.TurnID != "" && ev.TurnID != turnID) {
				continue
			}
			switch ev.Kind {
			case "text_delta":
				b.messageDiagnostic("generating", "Codex 正在生成回复")
			case "item":
				if ev.ItemID == "" || ev.TurnID != turnID || ev.ThreadID != "" && ev.ThreadID != s.ThreadID {
					replyIdentityIncomplete = true
				}
				// item/completed 已包含完整正文，不再重复拼接之前的流式增量。
				if ev.ItemID != "" && completedItems[ev.ItemID] {
					continue
				}
				if ev.ItemID != "" {
					completedItems[ev.ItemID] = true
				}
				if ev.Text != "" {
					replies = append(replies, codex.HistoryMessage{ID: ev.ItemID, Role: "assistant", Text: ev.Text})
					if answer.Len() > 0 {
						answer.WriteString("\n\n")
					}
					answer.WriteString(ev.Text)
				}
			case "desktop_unconfirmed":
				return &codex.DesktopOutcomeUnconfirmed{}
			case "turn_failed":
				failureKind = requestExecutionFailed
				return diagnostics.UserError("Codex 本轮失败：" + diagnostics.Error(errors.New(ev.Text)))
			case "turn_completed":
				if ev.Status == "interrupted" {
					failureKind = requestExecutionFailed
					return diagnostics.UserError("Codex 本轮已中断")
				}
				if ev.Status != "completed" {
					if ev.Status == "failed" {
						failureKind = requestExecutionFailed
					}
					return diagnostics.UserError("Codex 本轮未成功完成：" + diagnostics.Error(errors.New(ev.Text)))
				}
				if strings.TrimSpace(answer.String()) == "" {
					answer.WriteString("Codex 已完成，但没有返回可发送的文本。")
				}
				if len(replies) == 0 {
					replies = []codex.HistoryMessage{{ID: "im-event:" + msg.ID + ":empty", Role: "assistant", Text: strings.TrimSpace(answer.String())}}
				}
				// 先核验完成通知自身的轮次，再独立核验回复身份；不能因缺少回复ID否认完成证据。
				if ev.TurnID == "" || ev.TurnID != turnID || ev.ThreadID != "" && ev.ThreadID != s.ThreadID {
					return diagnostics.UserError("完成回执缺少明确轮次关联，未猜测结果")
				}
				failureKind = requestReplyUnknown
				if e := b.store.RecordExecutionCompletion(s, msg.ID, turnID); e != nil {
					return diagnostics.UserError("已收到完成回执，但执行完成记录保存失败；不会重跑原任务")
				}
				if replyIdentityIncomplete {
					return diagnostics.UserError("已确认执行完成，但回复消息身份不完整；回复同步待核对，不会重跑")
				}
				if e := b.captureRequestReplies(turnCtx, s, msg.ID, turnID, replies); e != nil {
					return diagnostics.UserError("已收到完成回执，但回复清单未能安全保存：" + diagnostics.Error(e))
				}
				err = b.deliverLiveReplies(turnCtx, s, replies)
				if err == nil {
					b.refreshRecentConversation(turnCtx, s)
				}
				return err
			}
		}
	}
}
func (b *Bridge) command(ctx context.Context, adapter im.Adapter, msg im.Message) error {
	parts := strings.Fields(msg.Text)
	if len(parts) == 0 {
		return nil
	}
	switch parts[0] {
	case "/status":
		return b.sessionStatusCommand(ctx, adapter, msg)
	case "/report":
		return b.taskReportCommand(ctx, adapter, msg)
	case "/sendfile":
		return b.fileCommand(ctx, msg)
	case "/new":
		return b.openCreationForm(ctx, adapter, msg)
	case "/fork":
		b.bindMu.Lock()
		defer b.bindMu.Unlock()
		oldSession, e := b.store.ByChat(msg.Address)
		if e != nil {
			return errors.New("当前群聊尚未绑定 Codex 会话")
		}
		if oldSession.Status != "active" {
			return errors.New("关闭中或已归档会话不自动分叉，请显式创建新会话")
		}
		thread, e := b.codex.ForkThread(ctx, b.cfg, oldSession.ThreadID, "")
		if e != nil {
			return e
		}
		newAddress, e := b.newSessionAddress(ctx, adapter, msg, thread)
		if e != nil {
			return e
		}
		newSession := store.Session{ThreadID: thread.ID, Address: newAddress, WorkDir: thread.Cwd, Owner: msg.UserID, Managed: newAddress.Key() != msg.Address.Key(), GroupName: b.threadName(thread, msg.Address.Connection)}
		if e = b.store.Save(newSession); e != nil {
			return e
		}
		b.sessionCreatedNotice(ctx, adapter, msg, newSession)
		_, _ = adapter.SendText(ctx, newAddress, "已从会话 "+shortID(oldSession.ThreadID)+" 分叉。\n继承的工作目录："+thread.Cwd+"\n"+sessionClosureHint(newAddress), "")
		return nil
	case "/archive":
		s, e := b.store.ByChat(msg.Address)
		if e != nil {
			return e
		}
		if msg.UserID != s.Owner && !config.Contains(b.conn(msg.Address.Connection).AdminUsers, msg.UserID) {
			return errors.New("只有会话创建者或管理员可以归档")
		}
		if e = b.requestClose(s.ThreadID, "command", false, false); e != nil {
			return e
		}
		text := "已请求归档 Codex 会话，并解散本客户端创建的对应群聊。同步进度可在客户端的会话同步页查看。"
		if s.Address.IsTopic() {
			text = "已请求归档此话题对应的 Codex 会话，将更新入口卡片；共享群及其他话题保持不变。"
		} else if !s.Managed {
			text = "已请求归档 Codex 会话。该群没有客户端创建证明，将保留原群；进度可在客户端查看。"
		}
		_, _ = adapter.SendText(ctx, msg.Address, text, "")
		return nil
	default:
		_, _ = adapter.SendText(ctx, msg.Address, "支持命令：/new、/fork、/status、/archive。话题会话归档只标记入口；独立会话归档可能解散对应自建群。", msg.ID)
		return nil
	}
}
func (b *Bridge) getOrCreateNew(ctx context.Context, adapter im.Adapter, msg im.Message) (store.Session, error) {
	b.bindMu.Lock()
	defer b.bindMu.Unlock()
	t, e := b.codex.StartThread(ctx, b.cfg)
	if e != nil {
		return store.Session{}, e
	}
	a, e := b.newSessionAddress(ctx, adapter, msg, t)
	if e != nil {
		return store.Session{}, e
	}
	s := store.Session{ThreadID: t.ID, Address: a, WorkDir: t.Cwd, Owner: msg.UserID, Managed: a.Key() != msg.Address.Key(), GroupName: b.threadName(t, msg.Address.Connection), Fresh: true}
	if e = b.store.Save(s); e != nil {
		return store.Session{}, e
	}
	b.sessionCreatedNotice(ctx, adapter, msg, s)
	return s, nil
}
func (b *Bridge) createSessionChat(ctx context.Context, adapter im.Adapter, msg im.Message, name string) (im.Address, error) {
	cc := b.conn(msg.Address.Connection)
	if !adapter.Capabilities().CreateConversation {
		return im.Address{}, errors.New("当前 IM 不支持创建独立会话，不能复用已有群破坏映射")
	}
	intentKey := "chat-intent-v2:" + msg.Address.Connection + ":" + msg.ID + ":" + name
	if previous, err := b.store.GetMeta(intentKey); err == nil && previous != "" {
		var saved im.Address
		if json.Unmarshal([]byte(previous), &saved) == nil && saved.ChatID != "" {
			return saved, nil
		}
	}
	members := append([]string{}, cc.AdminUsers...)
	if !config.Contains(members, msg.UserID) {
		members = append(members, msg.UserID)
	}
	address, err := adapter.CreateConversation(ctx, im.Conversation{Name: name, Members: members, Owner: msg.UserID, IdempotencyKey: store.Hash(msg.Address.Key() + "\x00" + name)})
	if err != nil {
		return im.Address{}, err
	}
	if raw, marshalErr := json.Marshal(address); marshalErr == nil {
		_ = b.store.SetMeta(intentKey, string(raw))
	}
	return address, nil
}
func (b *Bridge) receiveInteraction(ctx context.Context, in im.Interaction) error {
	if strings.HasPrefix(in.ActionID, "session:") || strings.HasPrefix(in.ActionID, "session-form:") {
		return b.receiveSessionControl(ctx, in)
	}
	if strings.HasPrefix(in.ActionID, "report:") {
		return b.receiveTaskReport(ctx, in)
	}
	// 回退后旧手机任务控制卡不再执行；历史消息保留，不重放。
	if strings.HasPrefix(in.ActionID, "task:") || strings.HasPrefix(in.ActionID, "mobile:") {
		return diagnostics.UserError("已回退到基础版本，旧任务控制卡不再执行。请发送新消息；未重放旧任务。")
	}
	if strings.HasPrefix(in.ActionID, "new:") {
		if !b.allowed(in.Address.Connection, in.UserID) {
			return errors.New("无权操作创建表单")
		}
		return b.receiveCreationForm(ctx, in)
	}
	if !b.allowed(in.Address.Connection, in.UserID) {
		return errors.New("无权操作或审批已失效")
	}
	action, err := b.store.Action(in.ActionID)
	if err != nil || action.Status != "pending" || action.ExpiresAt <= time.Now().Unix() || action.UserID != in.UserID || action.Address.BaseKey() != in.Address.BaseKey() || (in.Address.TopicID != "" && action.Address.TopicID != in.Address.TopicID) || action.MessageID == "" || action.MessageID != in.MessageID {
		return errors.New("无权操作或审批已失效")
	}
	if err := validateActionChoice(action, in.Choice); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := b.approvals[in.ActionID]
	if ch == nil {
		return errors.New("无权操作或审批已失效")
	}
	select {
	case ch <- approvalResult{Value: in.Choice}:
		delete(b.approvals, in.ActionID)
		return nil
	default:
		return errors.New("该选择已提交，请勿重复操作")
	}
}
func (b *Bridge) handleCodexRequest(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p struct {
		ThreadID    string          `json:"threadId"`
		TurnID      string          `json:"turnId"`
		ItemID      string          `json:"itemId"`
		Command     string          `json:"command"`
		Reason      string          `json:"reason"`
		Permissions json.RawMessage `json:"permissions"`
		Questions   []struct {
			ID       string `json:"id"`
			Question string `json:"question"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("Codex 审批请求格式无效")
	}
	s, err := b.store.ByThread(p.ThreadID)
	if err != nil {
		return nil, errors.New("未绑定 IM 会话的 Codex 请求")
	}
	if s.Status != "active" {
		return nil, errors.New("会话关闭中，不再允许审批")
	}
	b.mu.Lock()
	running := b.active[s.ThreadID]
	b.mu.Unlock()
	if running == nil {
		return nil, errors.New("没有桥接执行上下文，审批已失效")
	}
	ctx = running.ctx
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	adapter := b.channels[s.Address.Connection]
	if adapter == nil {
		return nil, errors.New("审批对应的 IM 连接不存在")
	}
	kind, title, text := "approval", "Codex 需要确认", "Codex 请求继续执行。"
	buttons := []im.Button{{Label: "确认", Value: "accept"}, {Label: "取消", Value: "decline", Destructive: true}}
	switch method {
	case "item/commandExecution/requestApproval":
		kind, title, text = "command", "确认执行命令", "Codex 请求执行命令：\n\n"+p.Command
	case "item/fileChange/requestApproval":
		kind, title, text = "file", "确认修改文件", "Codex 请求修改工作区文件。"
	case "item/permissions/requestApproval":
		kind, title, text = "permissions", "确认扩大权限", "Codex 请求扩大当前任务权限。默认拒绝，只有会话创建者可以确认。\n\n"+p.Reason
	case "item/tool/requestUserInput":
		return b.requestUserInput(ctx, adapter, s, raw)
	default:
		return nil, errors.New("不支持的 Codex 交互请求")
	}
	actionID := store.ID()
	payload := string(raw)
	if err := b.store.PutAction(store.Action{ID: actionID, ThreadID: p.ThreadID, Address: s.Address, UserID: s.Owner, Kind: kind, Payload: payload, ExpiresAt: time.Now().Add(time.Duration(b.cfg.Limits.ApprovalTimeoutSeconds) * time.Second).Unix()}); err != nil {
		return nil, err
	}
	ch := make(chan approvalResult, 1)
	b.mu.Lock()
	b.approvals[actionID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.approvals, actionID)
		b.mu.Unlock()
		_ = b.store.Expire(actionID, "timeout")
	}()
	messageID, err := adapter.SendCard(ctx, s.Address, im.Card{Title: title, Text: text, ActionID: actionID, Buttons: buttons}, "")
	if err != nil {
		return nil, err
	}
	if err = b.store.ActionMessage(actionID, messageID); err != nil {
		return nil, errors.New("审批卡已发送，但保存回执失败，未允许操作")
	}
	timer := time.NewTimer(time.Duration(b.cfg.Limits.ApprovalTimeoutSeconds) * time.Second)
	defer timer.Stop()
	select {
	case result := <-ch:
		choice, _ := result.Value.(string)
		if choice == "" {
			choice = "decline"
		}
		resolved, resolveErr := b.store.Resolve(actionID, choice)
		if resolveErr != nil || !resolved || ctx.Err() != nil {
			return nil, errors.New("审批已过期或会话正在关闭")
		}
		stateText := "已取消或拒绝。"
		if choice == "accept" {
			stateText = "已确认。"
		}
		_ = adapter.UpdateCard(ctx, s.Address, messageID, im.Card{Title: title, Text: stateText, ActionID: actionID})
		if choice != "accept" {
			choice = "decline"
		}
		if kind == "permissions" {
			if choice != "accept" || len(p.Permissions) == 0 || string(p.Permissions) == "null" {
				return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil
			}
			var requested any
			if json.Unmarshal(p.Permissions, &requested) != nil {
				return nil, errors.New("权限请求内容无效，拒绝扩大权限")
			}
			return map[string]any{"permissions": requested, "scope": "turn"}, nil
		}
		return map[string]string{"decision": choice}, nil
	case <-timer.C:
		_ = adapter.UpdateCard(ctx, s.Address, messageID, im.Card{Title: title, Text: "已超时，默认拒绝。", ActionID: actionID})
		return map[string]string{"decision": "decline"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (b *Bridge) sendTextLimited(ctx context.Context, adapter im.Adapter, address im.Address, text, key string) error {
	max := b.cfg.Limits.MaxMessageChars
	if max <= 0 {
		max = 2000
	}
	runes := []rune(text)
	if len(runes) == 0 {
		_, err := adapter.SendText(ctx, address, "（无文本输出）", key)
		return err
	}
	for offset, part := 0, 0; offset < len(runes); part++ {
		end := offset + max
		if end > len(runes) {
			end = len(runes)
		}
		suffix := ""
		if end < len(runes) {
			suffix = fmt.Sprintf("\n\n（第 %d 段，后续消息继续）", part+1)
		}
		id := ""
		if key != "" {
			id = store.Hash(key + fmt.Sprintf(":reply-part:%d", part))[:32]
		}
		if _, err := adapter.SendText(ctx, address, string(runes[offset:end])+suffix, id); err != nil {
			return err
		}
		offset = end
	}
	return nil
}
func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
func safeError(e error) string {
	if e == nil {
		return "未知错误"
	}
	return e.Error()
}

// refreshRecentConversation 只更新现有历史卡片，不发送新的历史消息；失败不影响本轮已发送的模型回复。
func (b *Bridge) refreshRecentConversation(ctx context.Context, s store.Session) {
	if !s.Managed || s.Status != "active" {
		return
	}
	p, e := b.store.Presentation(s.ThreadID)
	if e != nil || p.HistoryState != "sent" || p.MessageID == "" {
		return
	}
	op, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, messages, e := b.codex.RecentConversation(op, s.ThreadID)
	if e != nil || len(messages) == 0 {
		return
	}
	body := b.historyText(messages, s.ThreadID)
	source := presentationSourceFromMessages(messages)
	pruned, checkErr := b.store.PresentationSourcePruned(s.ThreadID, source)
	if checkErr != nil || pruned {
		return
	}
	a := b.channels[s.Address.Connection]
	if a == nil {
		return
	}
	if body != p.Payload {
		if e = a.UpdateCard(op, s.Address, p.MessageID, im.Card{Title: "最新对话", Text: body}); e != nil {
			return
		}
	}
	// 同文不同消息ID仍更新来源与保留时间，但不重复更新远端卡片。
	_ = b.store.HistoryUpdated(s.ThreadID, body, len(messages), source)
}
