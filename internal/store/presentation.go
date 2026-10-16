package store

import (
	"database/sql"
	"errors"
	"time"
)

// Presentation 不向 UI 暴露历史正文；Payload 仅由发送工作者读取。
type Presentation struct {
	TopicID       string `json:"topic_id"`
	SessionStatus string `json:"session_status"`
	ThreadID      string `json:"thread_id"`
	Connection    string `json:"connection"`
	ChatID        string `json:"chat_id"`
	Name          string `json:"name"`
	NameApplied   bool   `json:"name_applied"`
	HistoryState  string `json:"history_state"`
	HistoryCount  int    `json:"history_count"`
	Payload       string `json:"-"`
	MessageID     string `json:"message_id"`
	NameError     string `json:"name_error"`
	HistoryError  string `json:"history_error"`
	Attempts      int    `json:"attempts"`
	NextAttempt   int64  `json:"next_attempt"`
	UpdatedAt     int64  `json:"updated_at"`
}

const presentationSchema = `CREATE TABLE IF NOT EXISTS session_presentation (
    thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id),
    target_name TEXT NOT NULL DEFAULT '',
    name_applied INTEGER NOT NULL DEFAULT 0,
    history_state TEXT NOT NULL DEFAULT 'pending',
    history_count INTEGER NOT NULL DEFAULT 0,
    history_payload TEXT NOT NULL DEFAULT '',
    history_message_id TEXT NOT NULL DEFAULT '',
    name_error TEXT NOT NULL DEFAULT '',
    history_error TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);`

func (s *Store) migratePresentation() error {
	if _, e := s.db.Exec(presentationSchema); e != nil {
		return e
	}
	// 发送中崩溃不能确定平台是否收到；禁止重启后自动重复发送。
	_, e := s.db.Exec("UPDATE session_presentation SET history_state = 'unknown', history_error = '上次发送中断，可能已送达；为避免重复，不自动重发', updated_at = ? WHERE history_state = 'sending'", time.Now().Unix())
	return e
}

// EnsurePresentations 只回填有创建证明的既有活跃群，手动绑定/旧无证明群不处理。
func (s *Store) EnsurePresentations() error {
	_, e := s.db.Exec(`INSERT INTO session_presentation (thread_id, updated_at)
SELECT thread_id, ? FROM sessions WHERE managed_chat = 1 AND status = 'active'
ON CONFLICT(thread_id) DO NOTHING`, time.Now().Unix())
	return e
}

const presentationBaseColumns = "p.thread_id, s.connection_id, s.chat_id, p.target_name, p.name_applied, p.history_state, p.history_count, p.history_payload, p.history_message_id, p.name_error, p.history_error, p.attempts, p.next_attempt, p.updated_at, s.status"

func scanPresentation(row interface{ Scan(...any) error }) (Presentation, error) {
	var p Presentation
	e := row.Scan(&p.ThreadID, &p.Connection, &p.ChatID, &p.Name, &p.NameApplied, &p.HistoryState, &p.HistoryCount, &p.Payload, &p.MessageID, &p.NameError, &p.HistoryError, &p.Attempts, &p.NextAttempt, &p.UpdatedAt, &p.SessionStatus, &p.TopicID)
	return p, e
}
func (s *Store) presentationColumns() (string, error) {
	has, err := hasColumn(s.db, "sessions", "topic_id")
	if err != nil {
		return "", err
	}
	if has {
		return presentationBaseColumns + ", s.topic_id", nil
	}
	return presentationBaseColumns + ", ''", nil
}
func (s *Store) Presentation(thread string) (Presentation, error) {
	presentationColumns, e := s.presentationColumns()
	if e != nil {
		return Presentation{}, e
	}
	return scanPresentation(s.db.QueryRow("SELECT "+presentationColumns+" FROM session_presentation p JOIN sessions s ON s.thread_id = p.thread_id WHERE p.thread_id = ?", thread))
}
func (s *Store) Presentations(pendingOnly bool) ([]Presentation, error) {
	presentationColumns, columnErr := s.presentationColumns()
	if columnErr != nil {
		return nil, columnErr
	}
	q := "SELECT " + presentationColumns + " FROM session_presentation p JOIN sessions s ON s.thread_id = p.thread_id "
	var rows *sql.Rows
	var e error
	if pendingOnly {
		q += "WHERE s.status = 'active' AND s.managed_chat = 1 AND p.next_attempt <= ? AND (p.name_applied = 0 OR p.history_state = 'pending') ORDER BY p.next_attempt, p.thread_id LIMIT 20"
		rows, e = s.db.Query(q, time.Now().Unix())
	} else {
		q += "ORDER BY p.updated_at DESC, p.thread_id LIMIT 100"
		rows, e = s.db.Query(q)
	}
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Presentation{}
	for rows.Next() {
		p, e := scanPresentation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) PreparePresentation(thread, name, body string, count int, sources ...PresentationSource) error {
	if count < -1 || count > 5 {
		return errors.New("历史条数超出范围")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if name != "" {
		if _, e = tx.Exec("UPDATE session_presentation SET target_name = ?, updated_at = ? WHERE thread_id = ? AND target_name = '' AND name_applied = 0", name, time.Now().Unix(), thread); e != nil {
			return e
		}
	}
	if body != "" {
		pruned, checkErr := presentationPruned(tx, thread, presentationSource(sources))
		if checkErr != nil {
			return checkErr
		}
		if pruned {
			return tx.Commit()
		}
		result, updateErr := tx.Exec("UPDATE session_presentation SET history_payload = ?, history_count = ?, history_state = 'pending', updated_at = ? WHERE thread_id = ? AND history_state IN ('pending', 'empty') AND history_payload = ''", body, count, time.Now().Unix(), thread)
		if updateErr != nil {
			return updateErr
		}
		n, rowErr := result.RowsAffected()
		if rowErr != nil {
			return rowErr
		}
		if n == 1 {
			if e = recordPresentationClock(tx, thread, body, 0, presentationSource(sources)); e != nil {
				return e
			}
		}
	} else if count == 0 {
		_, e = tx.Exec("UPDATE session_presentation SET history_state = 'empty', updated_at = ? WHERE thread_id = ? AND history_state = 'pending' AND history_payload = ''", time.Now().Unix(), thread)
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) PresentationRenamed(thread string) error {
	_, e := s.db.Exec("UPDATE session_presentation SET name_applied = 1, name_error = '', updated_at = ? WHERE thread_id = ?", time.Now().Unix(), thread)
	return e
}
func (s *Store) BeginHistorySend(thread string) (bool, error) {
	r, e := s.db.Exec(`UPDATE session_presentation SET history_state = 'sending', updated_at = ?
WHERE thread_id = ? AND history_state = 'pending' AND history_payload <> ''
  AND EXISTS (SELECT 1 FROM sessions WHERE thread_id = ? AND managed_chat = 1 AND status = 'active')`, time.Now().Unix(), thread, thread)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) HistorySent(thread, message string) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	result, e := tx.Exec("UPDATE session_presentation SET history_state = 'sent', history_message_id = ?, history_error = '', updated_at = ? WHERE thread_id = ? AND history_state = 'sending'", message, time.Now().Unix(), thread)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n == 1 {
		var body string
		if e = tx.QueryRow("SELECT history_payload FROM session_presentation WHERE thread_id=?", thread).Scan(&body); e != nil {
			return e
		}
		if e = recordPresentationClock(tx, thread, body, time.Now().Unix()); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// HistoryUpdated 保存已成功更新到同一张IM卡片的最新对话，正文及其保留时间同事务确认。
func (s *Store) HistoryUpdated(thread, body string, count int, sources ...PresentationSource) error {
	if count < 0 || count > 2 {
		return errors.New("最近一轮对话条数超出范围")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	pruned, checkErr := presentationPruned(tx, thread, presentationSource(sources))
	if checkErr != nil {
		return checkErr
	}
	if pruned {
		return nil
	}
	// 来源与展示内容均未变化时不刷新updated_at，避免每轮扫描重排列表或无谓写库。
	var unchanged int
	if e = tx.QueryRow(`SELECT count(*) FROM session_presentation p
         JOIN history_presentation_clock c ON c.thread_id = p.thread_id
WHERE p.thread_id = ? AND p.history_state = 'sent' AND p.history_payload = ?
  AND c.source_key = ? AND c.content_hash = ? AND c.confirmed_at > 0`, thread, body, presentationSource(sources).key(), Hash(body)).Scan(&unchanged); e != nil {
		return e
	}
	if unchanged > 0 {
		return nil
	}
	result, e := tx.Exec(`UPDATE session_presentation
SET history_payload = ?, history_count = ?, history_state = 'sent', history_error = '', next_attempt = 0, updated_at = ?
WHERE thread_id = ? AND history_state = 'sent' AND history_message_id <> ''`, body, count, time.Now().Unix(), thread)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n == 1 {
		if e = recordPresentationClock(tx, thread, body, time.Now().Unix(), presentationSource(sources)); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) PresentationFailure(thread, field, message string, uncertain bool) error {
	if field != "name" && field != "history" {
		return errors.New("未知展示状态字段")
	}
	p, e := s.Presentation(thread)
	if e != nil {
		return e
	}
	delay := 10
	for i := 0; i < p.Attempts && delay < 600; i++ {
		delay *= 2
	}
	if delay > 600 {
		delay = 600
	}
	assignment := field + "_error = ?"
	if field == "history" {
		if uncertain {
			assignment += ", history_state = 'unknown'"
		} else {
			assignment += ", history_state = CASE WHEN history_state = 'sending' THEN 'pending' ELSE history_state END"
		}
	}
	_, e = s.db.Exec("UPDATE session_presentation SET "+assignment+", attempts = attempts + 1, next_attempt = ?, updated_at = ? WHERE thread_id = ?", message, time.Now().Unix()+int64(delay), time.Now().Unix(), thread)
	return e
}
func (s *Store) RetryPresentation(thread string) error {
	r, e := s.db.Exec(`UPDATE session_presentation SET next_attempt = ?
WHERE thread_id = ? AND history_state NOT IN ('sending', 'unknown')
  AND (name_applied = 0 OR history_state = 'pending')
  AND EXISTS (SELECT 1 FROM sessions WHERE thread_id = ? AND status = 'active' AND managed_chat = 1)`, time.Now().Unix(), thread, thread)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return errors.New("没有可安全重试的补齐任务；发送结果不确定时不会自动重发")
	}
	return nil
}

// SetPresentationName 仅为活跃自建群更新目标名称；不改历史快照或消息投递状态。
// 同名不重置已确认标记，不因轮询重复改名。
func (s *Store) SetPresentationName(thread, name string) error {
	if name == "" || len([]rune(name)) > 55 {
		return errors.New("群名称长度无效")
	}
	_, err := s.db.Exec(`UPDATE session_presentation SET target_name=?,name_applied=0,name_error='',updated_at=?
 WHERE thread_id=? AND target_name<>? AND EXISTS (SELECT 1 FROM sessions s WHERE s.thread_id=session_presentation.thread_id AND s.status='active' AND s.managed_chat=1)`, name, time.Now().Unix(), thread, name)
	return err
}
