package bridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"im-agent/internal/attachments"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

// 参数只有明确的会话和请求，不接受文件路径、不按“最近任务”猜测。
type SendTaskReportRequest struct {
	ThreadID string `json:"thread_id"`
	EventID  string `json:"event_id"`
}

func validTaskReportRequest(r SendTaskReportRequest) bool {
	return r.ThreadID != "" && len(r.ThreadID) <= 128 && r.EventID != "" && len(r.EventID) <= 256 &&
		!strings.ContainsAny(r.ThreadID+r.EventID, "\x00\r\n\t ")
}

func (b *Bridge) SendTaskReport(parent context.Context, r SendTaskReportRequest) error {
	if !validTaskReportRequest(r) {
		return diagnostics.UserError("任务报告参数无效，必须指定会话与请求")
	}
	ctx, s, stop, err := b.beginTaskReport(parent, r.ThreadID)
	if err != nil {
		return err
	}
	defer stop()
	files, ok := b.channels[s.Address.Connection].(im.GuardedFileAdapter)
	if !ok {
		return diagnostics.UserError("目标连接不支持发送前复核的报告文件发送")
	}
	delivery, err := b.store.ReadTaskDelivery(ctx, r.ThreadID, r.EventID)
	if errors.Is(err, store.ErrTaskDeliveryPruned) {
		return diagnostics.UserError("该任务历史已按7天保留策略清理，无法再生成完整报告；不会重跑任务")
	}
	if err != nil {
		return diagnostics.UserError("本次请求没有可完整核对的报告，可能尚未完成、关联不足或超出报告上限；未发送")
	}
	body := b.presentationFilter().Text(delivery.Report)
	if len(body) == 0 || int64(len(body)) > attachments.MaxBytes {
		return diagnostics.UserError("本次报告为空或超过附件上限，未发送")
	}
	if err = attachments.SafeName(delivery.ReportName); err != nil {
		return diagnostics.UserError("报告文件名不符合安全要求，未发送")
	}
	// 读取后、预留前重新核对地址；不把旧卡报告送去改绑后的其它群。
	current, err := b.store.ByThread(r.ThreadID)
	if err != nil || current.Status != "active" || current.Address.Key() != s.Address.Key() {
		return diagnostics.UserError("会话状态或绑定地址发生变化，报告未发送")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	id := store.TaskReportTransferID(r.ThreadID, r.EventID, s.Address.Key())
	claimed, err := b.store.BeginTransfer(store.Transfer{ID: id, ThreadID: s.ThreadID, Direction: "out", Kind: "file", Name: delivery.ReportName})
	if err != nil {
		return diagnostics.UserError("报告投递记录无法保存，未发送")
	}
	if !claimed {
		previous, readErr := b.store.TaskReportTransfer(r.ThreadID, r.EventID, s.Address.Key())
		if readErr == nil && previous.State == "sent" {
			return nil // 已有平台回执，重复点击只保留原消息，不重新上传。
		}
		return diagnostics.UserError("此报告已有发送记录或送达结果待核对，请查看附件记录与群消息；不会自动重发或重新执行任务")
	}
	current, err = b.store.ByThread(r.ThreadID)
	if err != nil || current.Status != "active" || current.Address.Key() != s.Address.Key() || ctx.Err() != nil {
		_ = b.store.FinishTransfer(id, "failed", "", "", "发送前会话状态、绑定地址或后台状态变化，尚未发送")
		return diagnostics.UserError("发送前会话或后台状态变化，报告未发送；不会自动重发")
	}
	// BeginTransfer之后仍不自动重试；停止/网络不确定由独立附件账本保留。
	platform, sendErr := files.SendAttachmentGuarded(ctx, s.Address, delivery.ReportName, "file", strings.NewReader(body), store.Hash(id)[:32], func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		current, err := b.store.ByThread(s.ThreadID)
		if err != nil || current.Status != "active" || current.Address.Key() != s.Address.Key() || ctx.Err() != nil {
			return diagnostics.UserError("报告上传后会话已关闭、改绑或操作已取消，未提交群消息")
		}
		return nil
	})
	if sendErr != nil || platform == "" {
		state := "failed"
		var unknown *im.DeliveryUncertain
		if platform == "" && sendErr == nil || errors.As(sendErr, &unknown) || errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
			state = "unknown"
		}
		_ = b.store.FinishTransfer(id, state, "", "", diagnostics.Error(sendErr))
		_ = b.enqueueSessionStatus(s.ThreadID, false, r.EventID)
		return diagnostics.UserError("报告未确认送达，请查看附件记录及群消息；原任务与原回复状态不受影响，不自动重发")
	}
	if err = b.store.FinishTransfer(id, "sent", "", platform, ""); err != nil {
		return diagnostics.UserError("平台已返回报告回执，但本地保存未确认，请先查看群消息，不要重发")
	}
	_ = b.enqueueSessionStatus(s.ThreadID, false, r.EventID)
	return nil
}

func (b *Bridge) reportInteractionSession(in im.Interaction) (store.Session, string, error) {
	if !b.allowed(in.Address.Connection, in.UserID) || in.Choice != "report" {
		return store.Session{}, "", diagnostics.UserError("无权获取此任务报告")
	}
	event := strings.TrimPrefix(in.ActionID, "report:")
	s, err := b.store.ByChat(in.Address)
	if err != nil || s.Status != "active" || s.Address.Key() != in.Address.Key() || !validTaskReportRequest(SendTaskReportRequest{s.ThreadID, event}) {
		return store.Session{}, "", diagnostics.UserError("报告卡片与当前活跃会话不一致")
	}
	card, err := b.store.StatusCard(s.ThreadID, event)
	if err != nil || card.State != "sent" || card.MessageID == "" || card.MessageID != in.MessageID || card.AddressKey != store.Hash(in.Address.Key()) {
		return store.Session{}, "", diagnostics.UserError("任务报告卡片回执或绑定不匹配，未发送")
	}
	return s, event, nil
}

func (b *Bridge) receiveTaskReport(ctx context.Context, in im.Interaction) error {
	s, event, err := b.reportInteractionSession(in)
	if err != nil {
		return err
	}
	return b.queueTaskReport(ctx, s, event, func() error {
		_, _, checkErr := b.reportInteractionSession(in)
		return checkErr
	})
}

func (b *Bridge) taskReportCommand(ctx context.Context, adapter im.Adapter, msg im.Message) error {
	parts := strings.Fields(msg.Text)
	if len(parts) != 2 {
		_, err := adapter.SendText(ctx, msg.Address, "用法：/report 请求ID。也可点击本次交付卡的“获取任务报告”；不会执行新模型任务。", msg.ID+":report-help")
		return err
	}
	s, err := b.store.ByChat(msg.Address)
	if err != nil || s.Status != "active" || !b.allowed(msg.Address.Connection, msg.UserID) {
		return diagnostics.UserError("当前入口未绑定活跃会话或无权获取报告")
	}
	event := parts[1]
	if !validTaskReportRequest(SendTaskReportRequest{s.ThreadID, event}) {
		return diagnostics.UserError("请求ID格式无效")
	}
	return b.queueTaskReport(ctx, s, event, func() error {
		current, err := b.store.ByChat(msg.Address)
		if err != nil || current.Status != "active" || current.ThreadID != s.ThreadID || current.Address.Key() != s.Address.Key() || !b.allowed(msg.Address.Connection, msg.UserID) {
			return diagnostics.UserError("报告请求的会话或权限已变化")
		}
		return nil
	})
}

// 文件上传使用独立有界队列，既不占用模型队列，也不长时间阻塞原状态卡刷新。
func (b *Bridge) queueTaskReport(ctx context.Context, s store.Session, event string, authorize func() error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if b.reportJobs == nil {
		return diagnostics.UserError("报告发送队列尚未就绪")
	}
	key := store.TaskReportTransferID(s.ThreadID, event, s.Address.Key())
	if _, exists := b.reportScheduled.LoadOrStore(key, true); exists {
		return nil
	}
	err := b.reportJobs.Push(s.ThreadID, func(parent context.Context) {
		defer b.reportScheduled.Delete(key)
		if parent.Err() != nil || authorize() != nil {
			return
		}
		err := b.SendTaskReport(parent, SendTaskReportRequest{ThreadID: s.ThreadID, EventID: event})
		if err == nil || parent.Err() != nil {
			return
		}
		current, checkErr := b.store.ByThread(s.ThreadID)
		adapter := b.channels[s.Address.Connection]
		if checkErr != nil || adapter == nil || current.Status != "active" || current.Address.Key() != s.Address.Key() {
			return
		}
		notice, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		_, _ = adapter.SendText(notice, s.Address, "任务报告："+diagnostics.Error(err), key+":notice")
	})
	if err != nil {
		b.reportScheduled.Delete(key)
		return diagnostics.UserError("报告队列暂满或后台停止，未执行新的模型任务，请稍后查看")
	}
	return nil
}

// 原卡完成后增强交付信息；读取失败只跳过增强，不失败原任务或猜测执行证据。
// 报告作为独立附件获取，状态卡只保留简短入口，不展开摘要和核验详情。
func (b *Bridge) addTaskDelivery(ctx context.Context, view store.SessionStatus, card *im.Card) {
	if view.Result == nil || view.Result.Execution != "completed" || view.Task.ID == "" {
		return
	}
	if _, err := b.store.ReadTaskDelivery(ctx, view.Session.ThreadID, view.Task.ID); err != nil {
		return
	}
	transfer, err := b.store.TaskReportTransfer(view.Session.ThreadID, view.Task.ID, view.Session.Address.Key())
	if err != nil {
		return
	}
	if transfer.ID != "" {
		labels := map[string]string{"sent": "已发送", "sending": "发送中", "unknown": "送达待核对", "failed": "发送失败"}
		if label := labels[transfer.State]; label != "" {
			card.Text += "\n报告附件：" + label
		}
		return
	}
	card.ActionID = "report:" + view.Task.ID
	card.Buttons = []im.Button{{Label: "获取任务报告", Value: "report"}}
}

// 与requestClose共用mu，检查活跃与登记取消不能被关闭意图插入打断。
// 不登记为模型active执行，不阻止同会话正常模型任务；结束不调用Interrupt。
func (b *Bridge) beginTaskReport(parent context.Context, thread string) (context.Context, store.Session, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.store.ByThread(thread)
	if err != nil || s.Status != "active" || parent.Err() != nil {
		return nil, store.Session{}, nil, diagnostics.UserError("会话不可用或操作已取消，不发送报告")
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	if b.reportOperations == nil {
		b.reportOperations = make(map[string]map[string]context.CancelFunc)
	}
	if b.reportOperations[thread] == nil {
		b.reportOperations[thread] = make(map[string]context.CancelFunc)
	}
	id := store.ID()
	b.reportOperations[thread][id] = cancel
	return ctx, s, func() {
		cancel()
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.reportOperations[thread], id)
		if len(b.reportOperations[thread]) == 0 {
			delete(b.reportOperations, thread)
		}
	}, nil
}
