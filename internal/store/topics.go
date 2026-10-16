package store

import (
	"database/sql"
	"errors"
	"im-agent/internal/im"
)

// hasColumn 只读探测用于兼容尚未启动新版本的业务库，不触发迁移。
func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, required, pk int
		var name, kind string
		var value any
		if err = rows.Scan(&cid, &name, &kind, &required, &value, &pk); err != nil {
			return false, err
		}
		if name == column {
			found = true
		}
	}
	return found, rows.Err()
}

// migrateTopics 原样复制旧映射，不做去重；唯一冲突或外键异常使整个迁移回滚。
// SQLite 无法删除表内 UNIQUE 约束，故在单连接、进程锁保护下原子重建父表。
func (s *Store) migrateTopics() (err error) {
	has, err := hasColumn(s.db, "sessions", "topic_id")
	if err != nil {
		return err
	}
	if _, err = s.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer func() {
		_, restore := s.db.Exec("PRAGMA foreign_keys=ON")
		if err == nil {
			err = restore
		}
	}()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !has {
		for _, q := range []string{
			`CREATE TABLE sessions_topics_v1 (
    thread_id TEXT PRIMARY KEY,
    channel TEXT NOT NULL, connection_id TEXT NOT NULL, chat_id TEXT NOT NULL,
    topic_id TEXT NOT NULL DEFAULT '', root_message_id TEXT NOT NULL DEFAULT '',
    work_dir TEXT NOT NULL, owner_id TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, managed_chat INTEGER NOT NULL DEFAULT 0,
    UNIQUE (channel, connection_id, chat_id, topic_id)
   )`,
			`INSERT INTO sessions_topics_v1 (thread_id, channel, connection_id, chat_id, work_dir, owner_id, status, created_at, updated_at, managed_chat)
SELECT thread_id, channel, connection_id, chat_id, work_dir, owner_id, status, created_at, updated_at, managed_chat FROM sessions`,
			"DROP TABLE sessions", "ALTER TABLE sessions_topics_v1 RENAME TO sessions",
			"ALTER TABLE pending_actions ADD COLUMN topic_id TEXT NOT NULL DEFAULT ''",
			"ALTER TABLE pending_actions ADD COLUMN root_message_id TEXT NOT NULL DEFAULT ''",
			"ALTER TABLE outbox ADD COLUMN topic_id TEXT NOT NULL DEFAULT ''",
			"ALTER TABLE outbox ADD COLUMN root_message_id TEXT NOT NULL DEFAULT ''",
		} {
			if _, err = tx.Exec(q); err != nil {
				return err
			}
		}
	}
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	broken := rows.Next()
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return rowErr
	}
	if broken {
		return errors.New("话题迁移检测到外键异常，已回滚，未丢弃记录")
	}
	return tx.Commit()
}

func (s *Store) HasGroupBinding(a im.Address) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT count(*) FROM sessions WHERE channel = ? AND connection_id = ? AND chat_id = ? AND topic_id = ''", a.Channel, a.Connection, a.ChatID).Scan(&count)
	return count > 0, err
}
func (s *Store) HasTopics(a im.Address) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT count(*) FROM sessions WHERE channel = ? AND connection_id = ? AND chat_id = ? AND topic_id <> ''", a.Channel, a.Connection, a.ChatID).Scan(&count)
	return count > 0, err
}

// FinishTopicClosure 不声称群已解散。只有已归档 Codex 且入口更新成功才调用。
func (s *Store) FinishTopicClosure(thread string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE session_closures SET state = 'topic_archived', last_error = '', updated_at = unixepoch()
WHERE thread_id = ? AND codex_archived = 1
  AND EXISTS (SELECT 1 FROM sessions WHERE thread_id = ? AND topic_id <> '')`, thread, thread)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("话题归档确认状态无效")
	}
	if _, err = tx.Exec("UPDATE sessions SET status = 'archived', updated_at = unixepoch() WHERE thread_id = ?", thread); err != nil {
		return err
	}
	return tx.Commit()
}
