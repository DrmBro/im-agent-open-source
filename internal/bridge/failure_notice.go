package bridge

import (
	"context"
	"errors"

	"im-agent/internal/codex"
	"im-agent/internal/diagnostics"
)

// 只描述已经确认的边界，不发起重试，也不改变底层提交/中断行为。
type requestFailureKind string

const (
	requestNotSent           requestFailureKind = "not_sent"
	requestSubmissionUnknown requestFailureKind = "submission_unconfirmed"
	requestOutcomeUnknown    requestFailureKind = "execution_unconfirmed"
	requestExecutionFailed   requestFailureKind = "execution_failed"
	requestReplyUnknown      requestFailureKind = "reply_delivery_unconfirmed"
)

type requestFailure struct {
	kind  requestFailureKind
	cause error
}

func (e *requestFailure) Error() string { return failureNoticeForError(e).Text() }
func (e *requestFailure) Unwrap() error { return e.cause }

type failureNotice struct {
	Stage, Status, Title, Detail, Advice string
}

func (n failureNotice) Text() string {
	text := "【" + n.Title + "】"
	if n.Detail != "" {
		text += "\n原因：" + n.Detail
	}
	return text + "\n" + n.Advice
}

func failureNoticeForStage(stage string) (failureNotice, bool) {
	n := failureNotice{Stage: stage, Status: "uncertain"}
	switch requestFailureKind(stage) {
	case requestNotSent:
		n.Status = "failed"
		n.Title = "尚未发送"
		n.Advice = "本条问题尚未提交给 Codex。处理上述原因后，可重新发送；不会自动重发。"
	case requestSubmissionUnknown:
		n.Title = "提交结果未确认"
		n.Advice = "本条问题可能已经提交。请先检查对应 Codex 会话及群内回复，不要直接重发；不会自动重放。"
	case requestOutcomeUnknown:
		n.Title = "已提交，执行结果未确认"
		n.Advice = "本条问题已提交，但桥接未确认最终结果；桌面可能仍在排队或执行。请先核对对应会话，不要直接重发；停止等待不代表任务已取消。"
	case requestExecutionFailed:
		n.Status = "failed"
		n.Title = "执行失败（已提交）"
		n.Advice = "本次执行已返回失败或中断，可能已有部分操作完成。请先核对桌面结果，再决定是否重新提交；不会自动重跑。"
	case requestReplyUnknown:
		n.Title = "执行已完成，回复同步未确认"
		n.Advice = "Codex 已返回完成，但回复尚未全部确认送达。请先在 Codex 查看回复，再核对群内消息或客户端投递记录；不要重新执行问题。"
	default:
		return failureNotice{}, false
	}
	return n, true
}

func failureNoticeForError(err error) failureNotice {
	// 缺少明确阶段证据时保守处理，不能把未知错误说成未发送。
	kind := requestSubmissionUnknown
	cause := err
	var classified *requestFailure
	if errors.As(err, &classified) {
		kind, cause = classified.kind, classified.cause
	}
	n, ok := failureNoticeForStage(string(kind))
	if !ok {
		n, _ = failureNoticeForStage(string(requestSubmissionUnknown))
	}
	var unknown *codex.DesktopOutcomeUnconfirmed
	switch {
	case errors.As(cause, &unknown):
		n.Detail = "桌面提交或执行回执未完整确认。"
	case errors.Is(cause, context.Canceled):
		n.Detail = "桥接本地等待已停止，不代表桌面任务已取消。"
	default:
		n.Detail = diagnostics.Error(cause)
	}
	return n
}

func (b *Bridge) recordRequestFailure(id, chatKey string, err error) (failureNotice, bool) {
	n := failureNoticeForError(err)
	recorded, e := b.store.MarkRequestFailure(id, chatKey, n.Status, n.Stage, n.Detail)
	if e != nil {
		b.log.Warn("请求异常状态保存失败", "event", id, "error", diagnostics.Error(e))
		return n, true
	}
	return n, recorded
}

func statusCardFailureText(err error) string {
	return "【仅状态卡更新异常】\n状态卡可能暂时缺失或停留在旧状态，这不代表问题发送或执行失败。\n原因：" + diagnostics.Error(err) + "\n此异常不会重新执行或取消任务。请以实际回复为准，可发送 /status 再查看；不要因此重发原问题。"
}
