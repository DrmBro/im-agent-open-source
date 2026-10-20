package bridge

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"im-agent/internal/im"
	"im-agent/internal/store"
	"im-agent/internal/telemetry"
)

// 当前会话视图补充桌面轮次；单条 IM 请求卡继续使用原请求的精确查询。
func (b *Bridge) readCurrentSessionStatus(ctx context.Context, thread string, overview bool) (store.SessionStatus, error) {
	view, err := b.store.ReadSessionStatus(thread)
	if err != nil {
		return view, err
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	snapshot, user, readErr := b.codex.LatestSessionTelemetry(readCtx, thread)
	cancel()
	if readErr != nil {
		// 查询失败不能把历史 IM 的“已回复”当成当前会话状态。
		snapshot = telemetry.Snapshot{Issue: "read_failed"}
	}
	// 新 IM 请求尚在提交前时优先显示排队状态，不退回其前一轮。
	if !overview && readErr == nil && snapshot.StartedAt > 0 && view.Task.Status == "processing" && view.Task.ReceivedAt*1000 > snapshot.StartedAt {
		return view, nil
	}
	if readErr == nil && snapshot.TurnID != "" {
		request, matchErr := b.store.ObservedSessionRequest(view.Session, user, snapshot.TurnID)
		if matchErr == nil && !overview {
			return b.store.ReadSessionRequestStatus(thread, request)
		}
		if matchErr != nil && !errors.Is(matchErr, sql.ErrNoRows) {
			snapshot.Issue = "identity_conflict"
		}
	}
	// 尚未提交的 IM 请求比日志中的轮次更新时，仍展示该请求的排队/提交状态。
	if !overview && snapshot.Issue == "" && view.Task.ID != "" && view.Task.ReceivedAt*1000 >= snapshot.StartedAt {
		return view, nil
	}
	view.Task = store.TaskRow{ThreadID: thread, Name: view.Name}
	view.Result, view.Telemetry, view.Desktop = nil, nil, &snapshot
	return view, nil
}

// 桌面轮次没有 IM 请求的送达回执，完成与消息送达分别表述。
func desktopSessionCard(view store.SessionStatus, now time.Time) im.Card {
	p := view.Desktop
	state := "当前状态待确认"
	if p.Issue == "" || p.Issue == "invalid_usage" {
		switch p.Outcome {
		case "completed":
			state = "模型已完成"
		case "failed":
			state = "本轮响应失败"
		case "interrupted":
			state = "本轮已中断"
		default:
			if p.StartedAt > 0 && p.LastActivityAt > 0 && now.UnixMilli()-p.LastActivityAt <= 60_000 {
				state = "日志显示轮次处理中"
			} else if p.StartedAt > 0 {
				state = "尚无轮次结束记录，当前状态待确认"
			}
		}
	}
	text := "当前状态：" + state + "\n来源：当前会话轮次（日志观察）"
	if p.StartedAt > 0 {
		text += "\n本轮开始：" + telemetryTime(p.StartedAt)
	}
	if p.LastOutputAt > 0 {
		text += "\n本轮最近可确认输出：" + telemetryTime(p.LastOutputAt)
	}
	if p.LastActivityAt > 0 {
		text += "\n最近活动：" + telemetryTime(p.LastActivityAt)
	}
	if p.EndedAt > 0 {
		text += "\n轮次结束：" + telemetryTime(p.EndedAt)
	}
	if p.Model != "" {
		text += "\n本轮模型：" + p.Model
	}
	if p.Issue != "" && p.Issue != "invalid_usage" {
		text += "\n说明：最新轮次日志暂不可完整核对，未沿用历史任务结果。"
	}
	if p.Outcome == "completed" {
		text += "\n回复消息的送达情况请以群内实际消息为准。"
	}
	return im.Card{Title: "会话状态", Text: text}
}
