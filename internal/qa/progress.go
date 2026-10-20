package qa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"im-agent/internal/im"
	"im-agent/internal/store"
)

func isProgressQuery(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "/status", "进度", "查看进度", "进度查询":
		return true
	}
	return false
}
func (r *Runner) replyProgress(msg im.Message) error {
	if !msg.HasReply || len(msg.ReplyToIDs) == 0 {
		return r.db.QAQueueNotice(msg, nil, "请引用要查询的机器人回执或回答，再 @机器人发送 /status 或“进度”。状态查询不会重新执行问题。")
	}
	_, event, e := r.db.QAResolveReply(msg.Address, msg.ReplyToIDs)
	if e != nil {
		return r.db.QAQueueNotice(msg, nil, "暂时无法从这条引用定位问答。请引用本机器人的接收回执或回答后再查询。")
	}
	progress, e := r.db.QAProgressForEvent(event)
	if e != nil {
		return e
	}
	return r.db.QAQueueNotice(msg, &progress, formatProgress(progress))
}
func formatProgress(p store.QAProgress) string {
	stages := map[string]string{"received": "已接收", "queued": "排队中", "classifying": "正在检查业务范围", "processing": "模型处理中", "generating": "正在整理回答", "using_tool": "处理业务步骤中", "waiting_approval": "等待本机确认", "waiting_input": "等待补充信息", "documenting": "文档整理中", "saving_document": "正在保存文档", "sending": "回答已生成，正在发送", "sent": "本轮回复已发送（平台已接收）", "rejected": "本轮未执行", "failed": "本轮处理失败", "unknown": "执行结果未确认", "delivery_failed": "回复发送失败", "delivery_unknown": "回复发送结果未确认"}
	kinds := map[string]string{"question": "业务问答", "requirement": "业务需求", "bug": "业务 Bug"}
	phases := map[string]string{"": "尚未分类", "answering": "正在回答", "clarifying": "等待提问者补充，事项尚未完成", "document_ready": "文档已生成", "answered": "业务问题已回答", "out_of_scope": "超出业务范围"}
	stage := stages[p.Stage]
	if stage == "" {
		stage = "状态待核对"
	}
	kind := kinds[p.Kind]
	if kind == "" {
		kind = "待识别"
	}
	phase := phases[p.Workflow]
	if phase == "" {
		phase = "处理中"
	}
	now := time.Now().Unix()
	elapsed := now - p.CreatedAt
	if elapsed < 0 {
		elapsed = 0
	}
	results := map[string]string{"queued": "未提交", "starting": "准备中", "running": "执行中", "completed": "本轮已完成", "failed": "本轮失败", "unknown": "结果未确认", "rejected": "未执行"}
	result := results[p.ExecutionState]
	if result == "" {
		result = "待核对"
	}
	text := fmt.Sprintf("引用轮次的进度（查询时快照）\n采集时刻："+time.Now().Format("2006-01-02 15:04:05 -07:00")+"\n处理阶段：%s\n执行结果：%s\n事项类型：%s\n事项阶段：%s\n记录至今：%d 秒", stage, result, kind, phase, elapsed)
	if p.Detail != "" {
		text += "\n说明：" + p.Detail
	}
	if p.ExecutionError != "" && p.ExecutionError != p.Detail {
		text += "\n处理原因：" + p.ExecutionError
	}
	if p.LastEventAt > 0 {
		delta := now - p.LastEventAt
		if delta < 0 {
			delta = 0
		}
		text += fmt.Sprintf("\n最后一次有效模型事件距今：%d 秒", delta)
	} else {
		text += "\n尚未收到有效模型事件。"
	}
	return text
}
func (r *Runner) notifyLoop(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		notices, e := r.db.QAPendingNotices(8)
		if e != nil {
			r.fail(e)
			return
		}
		for _, notice := range notices {
			if ctx.Err() != nil {
				return
			}
			adapter := r.adapters[notice.Connection]
			if adapter == nil {
				continue
			}
			claimed, e := r.db.QAClaimNotice(notice.ID)
			if e != nil {
				r.fail(e)
				return
			}
			if !claimed {
				continue
			}
			op, stop := context.WithTimeout(ctx, 25*time.Second)
			id, sendErr := adapter.SendText(op, im.Address{Channel: "dingtalk", Connection: notice.Connection, ChatID: notice.ChatID}, notice.Body, notice.ID)
			stop()
			state, detail := "sent", ""
			if sendErr != nil {
				state = "failed"
				detail = "进度通知发送失败；最终回答仍独立投递"
				var uncertain *im.DeliveryUncertain
				if errors.As(sendErr, &uncertain) {
					state = "unknown"
					detail = "进度通知结果未确认，未自动重发；可引用既有回答查询状态"
				}
			}
			if e = r.db.QANoticeResult(notice, state, id, detail); e != nil {
				r.fail(e)
				return
			}
		}
	}
}
