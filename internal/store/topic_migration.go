package store

import (
	"encoding/json"
	"errors"
	"im-agent/internal/im"
)

// RetiredGroup 查旧群墓碑；迁移后即使配置回退，也禁止旧群重新绑定或执行命令。
func (s *Store) RetiredGroup(a im.Address) (bool, error) {
	_, retired, err := s.RetiredTarget(a)
	return retired, err
}

// MoveToTopic 仅供停止桥接、持有存储独占锁的显式迁移使用。
// 不改 Codex ID/cwd/owner；旧展示状态完整留档，新目标单独进行一次历史补齐。
func (s *Store) MoveToTopic(old Session, target im.Address, name string) error {
	if old.Address.IsTopic() || !target.IsTopic() || target.RootMessageID == "" || target.ChatID == "" || target.Channel != old.Address.Channel || target.Connection != old.Address.Connection || target.ChatID == old.Address.ChatID {
		return errors.New("迁移目标必须是同连接的另一个群内的完整话题地址")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var blockers int
	e = tx.QueryRow(`SELECT (SELECT count(*) FROM events WHERE status = 'processing')
                   + (SELECT count(*) FROM pending_actions WHERE status = 'pending')
                   + (SELECT count(*) FROM session_closures WHERE thread_id = ?)
                   + (SELECT count(*) FROM outbox WHERE sent = 0 AND channel = ? AND connection_id = ? AND chat_id = ?)
                   + (SELECT count(*) FROM sessions WHERE channel = ? AND connection_id = ? AND chat_id = ? AND topic_id = '')`, old.ThreadID, old.Address.Channel, old.Address.Connection, old.Address.ChatID, target.Channel, target.Connection, target.ChatID).Scan(&blockers)
	if e != nil {
		return e
	}
	if blockers != 0 {
		return errors.New("存在执行/审批/关闭/待发送记录或目标群级绑定，不能迁移")
	}
	// 原样保留所有展示字段，避免旧历史回执丢失或不确定记录被当作未发送。
	rows, e := tx.Query("SELECT * FROM session_presentation WHERE thread_id = ?", old.ThreadID)
	if e != nil {
		return e
	}
	columns, e := rows.Columns()
	if e != nil {
		rows.Close()
		return e
	}
	saved := map[string]any{}
	if rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if e = rows.Scan(dest...); e != nil {
			rows.Close()
			return e
		}
		for i, key := range columns {
			saved[key] = values[i]
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	snapshot, _ := json.Marshal(map[string]any{"session": old, "presentation": saved, "target": target})
	if _, e = tx.Exec("INSERT INTO meta (key,value) VALUES (?,?)", "topic-migration-snapshot:"+old.ThreadID, string(snapshot)); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO meta (key,value) VALUES (?,?)", "retired-group:"+old.Address.BaseKey(), JSON(target)); e != nil {
		return e
	}
	r, e := tx.Exec(`UPDATE sessions SET chat_id = ?, topic_id = ?, root_message_id = ?, managed_chat = 1, updated_at = unixepoch()
WHERE thread_id = ? AND channel = ? AND connection_id = ? AND chat_id = ? AND topic_id = '' AND status = 'active'`, target.ChatID, target.TopicID, target.RootMessageID, old.ThreadID, old.Address.Channel, old.Address.Connection, old.Address.ChatID)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("原会话已变化，迁移回滚")
	}
	_, e = tx.Exec(`INSERT INTO session_presentation (thread_id,target_name,name_applied,history_state,updated_at)
VALUES (?,?,1,'pending',unixepoch())
ON CONFLICT(thread_id) DO UPDATE SET target_name = excluded.target_name, name_applied = 1,
    history_state = 'pending', history_count = 0, history_payload = '', history_message_id = '',
    name_error = '', history_error = '', attempts = 0, next_attempt = 0, updated_at = excluded.updated_at`, old.ThreadID, name)
	if e != nil {
		return e
	}
	return tx.Commit()
}
