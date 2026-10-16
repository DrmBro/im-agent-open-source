package store

import (
	"context"
	"database/sql"
	"time"
)

// WaitingSummary 只统计本地持久化处理中记录，不表示桌面或模型服务端实时队列。
// 与最多150条的任务展示列表分开读取，不因展示截断而少算；失败/未知/中断不计入。
type WaitingSummary struct {
	Queued        int   `json:"queued"`
	DesktopSubmit int   `json:"desktop_submit"`
	Draft         int   `json:"draft"`
	DesktopReady  int   `json:"desktop_ready"`
	Model         int   `json:"model"`
	CapturedAt    int64 `json:"captured_at"`
}

func waitingSummarySnapshot(db *sql.DB) (*WaitingSummary, error) {
	var out WaitingSummary
	// events.id 已有主键，一条记录就是一个请求；这里仅按阶段做正常聚合，不去重。
	e := db.QueryRow(`SELECT count(CASE WHEN stage = 'queued' THEN 1 END),
       count(CASE WHEN stage = 'waiting_desktop_submit' THEN 1 END),
       count(CASE WHEN stage = 'waiting_draft' THEN 1 END),
       count(CASE WHEN stage IN ('checking_desktop', 'waiting_desktop', 'switching_desktop', 'preserving_draft', 'restoring_draft') THEN 1 END),
       count(CASE WHEN stage = 'waiting_model' THEN 1 END)
FROM events
WHERE status = 'processing'`).Scan(&out.Queued, &out.DesktopSubmit, &out.Draft, &out.DesktopReady, &out.Model)
	if e != nil {
		return nil, e
	}
	out.CapturedAt = time.Now().Unix()
	return &out, nil
}

// RequestWaitingHint 由客户端和飞书状态卡共用。结束或结果未知的任务不能显示为正在等待。
func RequestWaitingHint(task TaskRow) string {
	if task.Status != "processing" {
		return ""
	}
	switch task.Stage {
	case "preserving_draft":
		return "正在备份并暂存同会话纯文本草稿；仅保存到本机加密快照，未确认备份前不会移走。"
	case "restoring_draft":
		return "正在核对并恢复桌面草稿；恢复结果与飞书提交结果分开确认，未恢复时可在本机草稿恢复中查看。"
	case "queued":
		return "尚未开始处理，等待同会话前序任务结束或空闲工作者；不是模型服务端排队，请勿重复发送。"
	case "waiting_desktop_submit":
		return "本条尚未发送，等待本程序现有的桌面提交锁；不代表前一条模型任务必须执行完，也不保证轮候名次。"
	case "checking_desktop":
		return "已取得桌面提交入口，正在确认目标会话和输入区；取得入口不等于已发送。"
	case "waiting_draft":
		return "本条尚未发送，等待手动处理桌面草稿；不会清空或覆盖，沿用原有等待期限，请勿重复发送。"
	case "waiting_desktop":
		return "本条尚未发送，等待桌面输入框恢复可编辑；不会中断已有任务或重置等待期限。"
	case "switching_desktop":
		return "正在定位目标会话，未确认正确会话和输入区前不会发送；不会抢回手动切换的窗口。"
	case "waiting_model":
		return "已提交，等待对应执行轮次和回复；尚不能据此判断模型已经开始执行，请勿重复发送。"
	default:
		return ""
	}
}

// EventWaitingStage 只更新同一请求仍在提交前观察阶段的处理中记录。
// 上层停止并收拢观察者后才记录提交结果；这里额外防止覆盖后台已确认的终态。
func (s *Store) EventWaitingStage(ctx context.Context, id, chatKey, stage string) (bool, error) {
	switch stage {
	case "waiting_desktop_submit", "checking_desktop", "waiting_draft", "waiting_desktop", "switching_desktop", "submitting", "preserving_draft", "restoring_draft":
	default:
		return false, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE events
SET stage = ?, error_summary = '', updated_at = ?
WHERE id = ?
  AND chat_key = ?
  AND status = 'processing'
  AND stage IN ('submitting', 'waiting_desktop_submit', 'checking_desktop', 'waiting_draft', 'waiting_desktop', 'switching_desktop', 'preserving_draft', 'restoring_draft')`,
		stage, time.Now().Unix(), id, Hash(chatKey))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
