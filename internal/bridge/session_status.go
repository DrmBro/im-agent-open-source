package bridge

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func sessionStatusStage(stage string) string {
	labels := map[string]string{
		"queued": "本地排队", "starting": "准备处理", "resuming": "准备会话",
		"downloading_attachment": "处理附件", "submitting": "提交消息",
		"waiting_desktop_submit": "等待桌面提交", "checking_desktop": "检查桌面输入区",
		"preserving_draft": "备份桌面草稿", "restoring_draft": "恢复桌面草稿",
		"waiting_draft": "等待草稿处理", "waiting_desktop": "等待桌面就绪",
		"switching_desktop": "切换会话", "waiting_approval": "等待确认",
		"waiting_editor": "等待输入区", "waiting_dialog": "等待弹窗处理",
		"waiting_desktop_approval": "等待桌面确认",
	}
	if label := labels[stage]; label != "" {
		return label
	}
	return "状态待核对"
}

// 状态只说明本次请求已确认的边界；桌面提交不等于模型轮次已开始。
func readonlySessionCard(view store.SessionStatus, _ time.Time) im.Card {
	result := view.Result
	state := "状态待核对"
	switch {
	case view.Task.ID == "":
		state = "尚无请求记录"
	case view.Telemetry != nil && view.Telemetry.Snapshot.Outcome == "failed":
		state = "本轮响应失败"
	case view.Telemetry != nil && view.Telemetry.Snapshot.Outcome == "interrupted":
		state = "本轮已中断"
	case result != nil && result.Delivery == "sent":
		state = "已回复"
	case result != nil && result.Execution == "completed" && result.Delivery == "awaiting_replies":
		state = "模型已完成，回复待核对"
	case result != nil && result.Execution == "completed":
		state = "模型已完成，回复同步中"
	case result != nil && result.Execution == "failed":
		state = "模型执行失败"
	case view.Task.Status == "done":
		state = "已回复"
	case view.Task.Status == "uncertain" || view.Task.Status == "interrupted":
		state = "结果待核对"
	case view.Task.Status == "failed" || view.Task.Status == "queued_failed":
		if notice, ok := failureNoticeForStage(view.Task.Stage); ok {
			state = notice.Title
		} else {
			state = "任务失败"
		}
	case view.Task.Status == "processing" && view.Task.Stage == "waiting_model":
		if view.Telemetry != nil && view.Telemetry.Snapshot.StartedAt > 0 && view.Telemetry.Snapshot.Issue != "identity_conflict" {
			state = "已进入对应轮次，模型处理中"
		} else {
			state = "已提交，等待模型开始"
		}
	case view.Task.Status == "processing" && view.Task.Stage == "delivering_reply":
		state = "回复发送中"
	case view.Task.Status == "processing":
		state = "尚未交给模型 · " + sessionStatusStage(view.Task.Stage)
	}
	text := "当前状态：" + state
	if view.Task.ID != "" {
		latest := "暂无可确认输出"
		if view.Telemetry != nil && view.Telemetry.Snapshot.LastOutputAt > 0 && view.Telemetry.Snapshot.Issue != "identity_conflict" {
			latest = telemetryTime(view.Telemetry.Snapshot.LastOutputAt)
		}
		text += "\n本轮最近可确认输出：" + latest
		if result != nil && result.Delivery == "sent" && result.DeliveredAt > 0 {
			text += "\n回复送达：" + time.Unix(result.DeliveredAt, 0).Format("01-02 15:04:05")
		}
	}
	if view.Task.Status != "processing" && view.Task.Error != "" {
		reason := []rune(view.Task.Error)
		if len(reason) > 80 {
			reason = append(reason[:80], '…')
		}
		text += "\n说明：" + string(reason)
	}
	if view.Task.Status == "uncertain" || view.Task.Status == "interrupted" {
		text += "\n请先核对，不要重发。"
	}
	return im.Card{Title: "任务状态", Text: text}
}
func (b *Bridge) statusCardLock(thread string) *sync.Mutex {
	v, _ := b.statusCardLocks.LoadOrStore(thread, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// 唯一涉及平台写入的是展示卡消息，本函数没有模型调用或任务队列入口。
func (b *Bridge) refreshSessionStatus(ctx context.Context, thread string, force bool) error {
	return b.refreshRequestStatus(ctx, thread, force, nil)
}

func (b *Bridge) refreshRequestStatus(ctx context.Context, thread string, force bool, request *string) error {
	lock := b.statusCardLock(thread)
	lock.Lock()
	defer lock.Unlock()
	var view store.SessionStatus
	var e error
	if request == nil {
		view, e = b.store.ReadSessionStatus(thread)
	} else {
		view, e = b.store.ReadSessionRequestStatus(thread, *request)
	}
	if e != nil {
		return e
	}
	session := view.Session
	if session.Status != "active" || !session.Managed {
		return nil
	}
	a := b.channels[session.Address.Connection]
	if a == nil {
		return errors.New("状态卡对应连接不存在")
	}
	if !a.Capabilities().InteractiveCards || !a.Capabilities().UpdateCard {
		return nil
	}
	existing, e := b.store.StatusCard(thread, view.Task.ID)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if errors.Is(e, sql.ErrNoRows) {
		enabled, e := b.store.StatusCardsEnabledAt()
		if e == nil && session.Address.IsTopic() {
			enabled, e = b.store.ControlCardsEnabledAt()
		}
		if e != nil {
			return e
		}
		if !force && (view.Task.ID == "" || view.Task.ReceivedAt < enabled) {
			return nil
		}
	} else {
		if existing.AddressKey != store.Hash(session.Address.Key()) {
			return diagnostics.UserError("状态卡绑定地址已改变，未向其他群发送或更新")
		}
		if existing.State == "creating" || existing.State == "unknown" {
			return diagnostics.UserError("状态卡送达结果待核对，未重复发卡；正常聊天不受影响")
		}
		if existing.NextAttempt > time.Now().Unix() {
			return nil
		}
	}
	// 编辑模型表单时冻结原卡，防止定时刷新重置用户尚未提交的选择。
	if !force {
		if _, err := b.store.OpenModelControl(thread, view.Task.ID); err == nil {
			return nil
		}
	}
	card := readonlySessionCard(view, time.Now())
	b.addTaskDelivery(ctx, view, &card)
	b.addSessionControls(view, &card)
	card.Text = b.presentationFilter().Text(card.Text)
	// 包含动作身份和按钮，不能因为正文相同而漏掉交付入口的更新。
	digest := store.Hash(store.JSON(card))
	if existing.MessageID != "" && existing.Digest == digest && !force && existing.Error == "" {
		return b.observeStatusCard(view)
	}
	// 网络请求前再次核对生命周期和原始地址，禁止改写其它会话卡片。
	current, e := b.store.ByThread(thread)
	if e != nil {
		return e
	}
	if current.Status != "active" || current.Address.Key() != session.Address.Key() {
		return nil
	}
	if existing.MessageID != "" {
		if e = a.UpdateCard(ctx, session.Address, existing.MessageID, card); e != nil {
			_ = b.store.StatusCardFailed(thread, diagnostics.Error(e), false, view.Task.ID)
			return e
		}
		if e = b.store.StatusCardUpdated(thread, existing.MessageID, digest, view.Task.ID, view.Task.ID); e != nil {
			return e
		}
		return b.observeStatusCard(view)
	}
	claimed, e := b.store.ReserveStatusCard(session, view.Task.ID)
	if e != nil || !claimed {
		return e
	}
	message, sendErr := a.SendCard(ctx, session.Address, card, "session-status:"+store.Hash(thread+"\x00"+session.Address.Key()+"\x00"+view.Task.ID))
	if sendErr != nil || message == "" {
		var unknown *im.DeliveryUncertain
		uncertain := message == "" && sendErr == nil || errors.As(sendErr, &unknown) || errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded)
		hint := diagnostics.Error(sendErr)
		if uncertain {
			hint = "状态卡送达结果不确定，未重复发卡；不会重新执行任务"
		}
		_ = b.store.StatusCardFailed(thread, hint, uncertain, view.Task.ID)
		if sendErr == nil {
			return errors.New(hint)
		}
		return sendErr
	}
	if e = b.store.StatusCardSent(thread, message, digest, view.Task.ID, view.Task.ID); e != nil {
		return e
	}
	return b.observeStatusCard(view)
}
func (b *Bridge) enqueueSessionStatus(thread string, force bool, request ...string) error {
	if b.statusCardJobs == nil {
		return diagnostics.UserError("状态卡展示队列尚未就绪，正常聊天不受影响")
	}
	key := thread + "\x00current"
	var requestID *string
	if len(request) > 0 {
		id := request[0]
		requestID = &id
		key = thread + "\x00request:" + id
	}
	if force {
		b.statusCardDirty.Store(key, true)
	} else {
		b.statusCardDirty.LoadOrStore(key, false)
	}
	if _, busy := b.statusCardScheduled.LoadOrStore(key, true); busy {
		return nil
	}
	e := b.statusCardJobs.Push(thread, func(parent context.Context) {
		defer func() {
			b.statusCardScheduled.Delete(key)
			pending, dirty := b.statusCardDirty.LoadAndDelete(key)
			if !dirty || parent.Err() != nil {
				return
			}
			followForce, _ := pending.(bool)
			if err := b.enqueueSessionStatus(thread, followForce, request...); err != nil {
				b.log.Warn("任务交付状态补刷排队失败", "thread", thread)
			}
		}()
		pending, _ := b.statusCardDirty.LoadAndDelete(key)
		refreshForce, _ := pending.(bool)
		refreshForce = refreshForce || force
		ctx, stop := context.WithTimeout(parent, 8*time.Second)
		defer stop()
		if err := b.refreshRequestStatus(ctx, thread, refreshForce, requestID); err != nil {
			b.log.Warn("只读状态卡更新失败", "thread", thread, "error", diagnostics.Error(err))
			b.notifyStatusCardFailure(parent, thread, requestID, refreshForce, err)
		}
	})
	if e != nil {
		b.statusCardScheduled.Delete(key)
		// 覆盖普通入队和在途补排的失败；只标记可更新原卡，未知/创建中状态保持不变。
		if requestID != nil {
			if markErr := b.store.MarkStatusCardRefreshPending(thread, *requestID); markErr != nil {
				b.log.Warn("原卡补刷标记未能保存", "thread", thread)
			}
		}
	}
	return e
}
func (b *Bridge) syncSessionStatusCards(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		sessions, e := b.store.LiveMirrorSessions()
		if e != nil {
			b.log.Warn("读取状态卡目标失败")
		} else {
			for _, s := range sessions {
				if ctx.Err() != nil {
					return
				}

				requests, readErr := b.store.PendingSessionStatusRequests(s)
				if readErr != nil {
					b.log.Warn("读取请求状态卡失败", "thread", s.ThreadID, "error", diagnostics.Error(readErr))
					continue
				}
				for _, request := range requests {
					if ctx.Err() != nil {
						return
					}
					_ = b.enqueueSessionStatus(s.ThreadID, false, request)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (b *Bridge) sessionStatusCommand(ctx context.Context, a im.Adapter, msg im.Message) error {
	s, e := b.store.ByChat(msg.Address)
	if e != nil {
		_, e = a.SendText(ctx, msg.Address, "当前会话尚未绑定 Codex。", msg.ID)
		return e
	}
	if s.Status != "active" {
		_, e = a.SendText(ctx, msg.Address, "会话已归档、关闭中或停用，不更新状态卡。", msg.ID)
		return e
	}
	if !s.Managed || !a.Capabilities().InteractiveCards || !a.Capabilities().UpdateCard {
		view, e := b.store.ReadSessionStatus(s.ThreadID)
		if e != nil {
			return e
		}
		_, e = a.SendText(ctx, msg.Address, readonlySessionCard(view, time.Now()).Text, msg.ID)
		return e
	}
	if e = b.enqueueSessionStatus(s.ThreadID, true); e != nil {
		_, _ = a.SendText(ctx, msg.Address, statusCardFailureText(e), msg.ID)
		return nil
	}
	return nil
}

// 供本地客户端显示当前会话状态，不进入执行队列、不开群、不运行模型。
func (b *Bridge) OpenSessionStatusForThread(ctx context.Context, thread string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s, e := b.store.ByThread(thread)
	if e != nil {
		return e
	}
	if s.Status != "active" || !s.Managed {
		return diagnostics.UserError("请选择活跃的会话群或话题")
	}
	return b.enqueueSessionStatus(thread, true)
}

// 只报告展示故障，不更改events或重放问题。通知失败也不会递归产生通知。
func (b *Bridge) notifyStatusCardFailure(parent context.Context, thread string, request *string, force bool, cause error) {
	if parent.Err() != nil {
		return
	}
	var view store.SessionStatus
	var e error
	if request != nil {
		view, e = b.store.ReadSessionRequestStatus(thread, *request)
	} else {
		view, e = b.store.ReadSessionStatus(thread)
	}
	if e != nil || view.Session.Status != "active" || !view.Session.Managed {
		return
	}
	session := view.Session
	card, e := b.store.StatusCard(thread, view.Task.ID)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return
	}
	if e == nil && card.AddressKey != store.Hash(session.Address.Key()) {
		return
	}
	adapter := b.channels[session.Address.Connection]
	if adapter == nil {
		return
	}
	if !force {
		if view.Task.ID == "" {
			return
		}
		reserved, reserveErr := b.store.ReserveStatusCardFailureNotice(thread, view.Task.ID)
		if reserveErr != nil || !reserved {
			return
		}
	}
	current, e := b.store.ByThread(thread)
	if e != nil || current.Status != "active" || current.Address.Key() != session.Address.Key() {
		return
	}
	noticeCtx, finish := context.WithTimeout(parent, 3*time.Second)
	defer finish()
	key := "status-notice:" + store.Hash(thread+"\x00"+session.Address.Key()+"\x00"+view.Task.ID)
	if force {
		key += ":" + store.ID()
	}
	_, e = adapter.SendText(noticeCtx, session.Address, b.presentationFilter().Text(statusCardFailureText(cause)), key)
	if e != nil {
		b.log.Warn("状态卡异常提醒未确认送达", "thread", thread, "error", diagnostics.Error(e))
	}
}
