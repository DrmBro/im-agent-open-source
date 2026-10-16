package store

import "database/sql"

// 报告动作以已确认请求+会话+地址为键，不把“同正文”当成同一任务。
func TaskReportTransferID(thread, event, addressKey string) string {
	return "task-report:" + Hash(thread+"\x00"+event+"\x00"+addressKey)
}

// 只读取独立附件账本，不据此改写原请求或原回复的完成状态。
func (s *Store) TaskReportTransfer(thread, event, addressKey string) (Transfer, error) {
	var t Transfer
	err := s.db.QueryRow(`SELECT id, thread_id, direction, kind, name, state, error, platform_id, updated_at
FROM attachment_transfers
WHERE id = ? AND thread_id = ? AND direction = 'out'`, TaskReportTransferID(thread, event, addressKey), thread).
		Scan(&t.ID, &t.ThreadID, &t.Direction, &t.Kind, &t.Name, &t.State, &t.Error, &t.PlatformID, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return Transfer{}, nil
	}
	return t, err
}

// MarkStatusCardRefreshPending 只为有明确平台回执的原卡登记更新重试。
// 不改变state、message_id、attempts，绝不能把creating/unknown改成可重新发卡。
func (s *Store) MarkStatusCardRefreshPending(thread, request string) error {
	_, err := s.db.Exec(`UPDATE session_status_cards
SET error = '任务交付状态待补刷，展示队列暂不可用', next_attempt = 0
WHERE thread_id = ? AND request_id = ?
  AND state = 'sent' AND message_id <> ''`, thread, request)
	return err
}
