package store

import "strings"

// 判定顺序由看板 SQL、历史任务和状态卡共同使用。事件结束不等于回复送达。
type requestStateRule struct {
	state                                    string
	statuses, stages, executions, deliveries []string
}

var requestStateRules = []requestStateRule{
	{state: "attention", deliveries: []string{"failed", "unknown"}},
	{state: "completed", executions: []string{"completed"}, deliveries: []string{"sent"}},
	{state: "attention", executions: []string{"failed"}},
	{state: "closed", statuses: []string{"cancelled", "expired", "bundled", "rejected"}},
	{state: "active", executions: []string{"completed"}, deliveries: []string{"pending", "awaiting_replies"}},
	{state: "attention", statuses: []string{"failed", "uncertain", "interrupted", "queued_failed", "queue_failed"}},
	{state: "waiting", statuses: []string{"waiting_input"}},
	{state: "waiting", statuses: []string{"processing"}, stages: []string{"waiting_draft", "waiting_input", "waiting_approval", "waiting_desktop_approval"}},
	{state: "active", statuses: []string{"processing"}},
}

func requestValueMatches(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func RequestState(task TaskRow) string {
	execution, delivery := "", ""
	if task.Result != nil {
		execution, delivery = task.Result.Execution, task.Result.Delivery
	}
	for _, rule := range requestStateRules {
		if requestValueMatches(rule.statuses, task.Status) && requestValueMatches(rule.stages, task.Stage) &&
			requestValueMatches(rule.executions, execution) && requestValueMatches(rule.deliveries, delivery) {
			return rule.state
		}
	}
	return "attention"
}

// 表达式来自调用方固定列名，取值来自本文件固定规则，不拼接用户输入。
func requestStateSQL(status, stage, execution, delivery string) string {
	query := "CASE"
	for _, rule := range requestStateRules {
		var conditions []string
		for i, values := range [][]string{rule.statuses, rule.stages, rule.executions, rule.deliveries} {
			if len(values) == 0 {
				continue
			}
			conditions = append(conditions, "("+[]string{status, stage, execution, delivery}[i]+") IN ('"+strings.Join(values, "','")+"')")
		}
		query += " WHEN " + strings.Join(conditions, " AND ") + " THEN '" + rule.state + "'"
	}
	return query + " ELSE 'attention' END"
}

func RequestStateLabel(task TaskRow) string {
	if task.ID == "" {
		return "尚无请求记录"
	}
	if strings.HasPrefix(task.Stage, "steer_") {
		return RequestStageLabel(task.Stage)
	}
	if strings.HasPrefix(task.Stage, "command_") {
		return RequestStageLabel(task.Stage) // 管理命令没有模型执行与回复清单。
	}
	result := task.Result
	switch RequestState(task) {
	case "completed":
		return "已回复"
	case "closed":
		switch task.Status {
		case "bundled":
			return "附件已合并到任务"
		case "expired":
			return "已过期，未自动执行"
		case "cancelled":
			return "已取消，未自动重放"
		}
		return "请求已关闭，未自动重放"
	case "waiting":
		if task.Status == "waiting_input" || task.Stage == "waiting_input" {
			return "等待文字说明"
		}
		return RequestStageLabel(task.Stage)
	case "active":
		if result != nil && result.Execution == "completed" {
			if result.Delivery == "awaiting_replies" {
				return "模型已完成，回复待核对"
			}
			return "模型已完成，回复同步中"
		}
		return RequestStageLabel(task.Stage)
	default:
		if result != nil {
			switch result.Delivery {
			case "unknown":
				return "回复送达待核对"
			case "failed":
				return "回复发送失败，等待重试"
			}
			if result.Execution == "failed" {
				return "模型执行失败或已中断"
			}
		}
		if task.Status == "failed" || task.Status == "queued_failed" || task.Status == "queue_failed" {
			if task.Stage == "not_sent" {
				return "尚未发送"
			}
			return "请求处理失败"
		}
		return "结果待核对"
	}
}

func RequestStageLabel(stage string) string {
	labels := map[string]string{
		"steer_submitting": "正在补充到当前轮次", "steer_submitted": "补充已送入当前轮次，最终结果跟随原轮次", "steer_unknown": "补充回执待核对，未重新提交",
		"received": "已收到，等待处理", "queued": "本地排队", "starting": "准备处理", "resuming": "准备会话",
		"downloading_attachment": "处理附件", "submitting": "提交消息",
		"waiting_desktop_submit": "等待桌面提交", "checking_desktop": "检查桌面输入区",
		"preserving_draft": "备份桌面草稿", "restoring_draft": "恢复桌面草稿",
		"waiting_draft": "等待草稿处理", "waiting_desktop": "等待桌面就绪",
		"switching_desktop": "切换会话", "waiting_approval": "等待确认",
		"waiting_editor": "等待输入区", "waiting_dialog": "等待弹窗处理",
		"waiting_desktop_approval": "等待桌面确认", "waiting_model": "已提交，等待执行或回复",
		"delivering_reply": "回复发送中",
		"command_running":  "管理命令处理中", "command_completed": "管理命令完成", "command_failed": "管理命令失败",
	}
	if label := labels[stage]; label != "" {
		return label
	}
	return "状态待核对"
}

// 逐片投递证据优先于聚合送达字段，不因聚合记录迟到而误报成功。
func requestDeliveryState(base string, unknown, failed int) string {
	if unknown != 0 {
		return "unknown"
	}
	if failed != 0 {
		return "failed"
	}
	return base
}

const requestReplyDeliverySQL = `SELECT coalesce(sum(m.expected_parts),0),
 coalesce(sum((SELECT count(*) FROM live_mirror_parts p WHERE p.thread_id=m.thread_id AND p.item_id=m.item_id AND p.state='sent')),0),
 coalesce(max(EXISTS(SELECT 1 FROM live_mirror_parts p WHERE p.thread_id=m.thread_id AND p.item_id=m.item_id AND p.state='unknown')),0),
 coalesce(max(EXISTS(SELECT 1 FROM live_mirror_parts p WHERE p.thread_id=m.thread_id AND p.item_id=m.item_id AND p.state='pending' AND p.error<>'')),0)
FROM request_result_replies m WHERE m.event_id=?`
