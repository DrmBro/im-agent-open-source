package store

import (
	"database/sql"
	"errors"
	"time"
)

// Closure 持久化单个映射的双向关闭进度。两个确认标志只会从 false 变 true。
type Closure struct {
	ThreadID      string `json:"thread_id"`
	Channel       string `json:"channel"`
	Connection    string `json:"connection"`
	ChatID        string `json:"chat_id"`
	TopicID       string `json:"topic_id"`
	Source        string `json:"source"`
	State         string `json:"state"`
	CodexArchived bool   `json:"codex_archived"`
	ChatClosed    bool   `json:"chat_closed"`
	Managed       bool   `json:"managed"`
	Attempts      int    `json:"attempts"`
	NextAttempt   int64  `json:"next_attempt"`
	LastError     string `json:"last_error"`
	UpdatedAt     int64  `json:"updated_at"`
}

const closureSchema = `CREATE TABLE IF NOT EXISTS session_closures (
    thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id),
    source TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    codex_archived INTEGER NOT NULL DEFAULT 0,
    chat_closed INTEGER NOT NULL DEFAULT 0,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS closures_pending ON session_closures(state, next_attempt);`

func (s *Store) migrateClosure() error {
	rows, e := s.db.Query("PRAGMA table_info(sessions)")
	if e != nil {
		return e
	}
	has := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if e = rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); e != nil {
			rows.Close()
			return e
		}
		if name == "managed_chat" {
			has = true
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	// 旧群没有创建证明，不能从前缀或群主猜测归属，默认受保护。
	if !has {
		if _, e = s.db.Exec("ALTER TABLE sessions ADD COLUMN managed_chat INTEGER NOT NULL DEFAULT 0"); e != nil {
			return e
		}
	}
	_, e = s.db.Exec(closureSchema)
	return e
}
func (s *Store) RequestClosure(thread, source string, archived, closed bool) error {
	if source != "command" && source != "codex" && source != "im" {
		return errors.New("未知关闭来源")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var exists int
	if e = tx.QueryRow("SELECT 1 FROM sessions WHERE thread_id = ?", thread).Scan(&exists); e != nil {
		return e
	}
	now := time.Now().Unix()
	_, e = tx.Exec(`INSERT INTO session_closures (thread_id, source, codex_archived, chat_closed, next_attempt, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(thread_id) DO UPDATE SET
    state = CASE WHEN session_closures.state IN ('done', 'topic_archived') THEN session_closures.state
                 WHEN session_closures.state = 'protected' AND excluded.chat_closed = 0 THEN 'protected'
                 ELSE 'pending' END,
    next_attempt = CASE WHEN (excluded.chat_closed = 1 AND session_closures.chat_closed = 0)
                            OR (excluded.codex_archived = 1 AND session_closures.codex_archived = 0)
                       THEN excluded.next_attempt ELSE session_closures.next_attempt END,
    codex_archived = CASE WHEN excluded.codex_archived = 1 THEN 1 ELSE session_closures.codex_archived END,
    chat_closed = CASE WHEN excluded.chat_closed = 1 THEN 1 ELSE session_closures.chat_closed END,
    updated_at = excluded.updated_at`, thread, source, archived, closed, now, now, now)
	if e != nil {
		return e
	}
	_, e = tx.Exec(`UPDATE sessions SET status = CASE WHEN (SELECT state FROM session_closures WHERE thread_id = ?) IN ('done', 'protected', 'topic_archived')
                                              THEN 'archived' ELSE 'closing' END, updated_at = ? WHERE thread_id = ?`, thread, now, thread)
	if e != nil {
		return e
	}
	_, e = tx.Exec("UPDATE pending_actions SET status = 'cancelled', resolved_at = ? WHERE thread_id = ? AND status = 'pending'", now, thread)
	if e != nil {
		return e
	}
	return tx.Commit()
}

const closureBaseColumns = "c.thread_id, s.channel, s.connection_id, s.chat_id, c.source, c.state, c.codex_archived, c.chat_closed, s.managed_chat, c.attempts, c.next_attempt, c.last_error, c.updated_at"

func scanClosure(row interface{ Scan(...any) error }) (Closure, error) {
	var c Closure
	e := row.Scan(&c.ThreadID, &c.Channel, &c.Connection, &c.ChatID, &c.Source, &c.State, &c.CodexArchived, &c.ChatClosed, &c.Managed, &c.Attempts, &c.NextAttempt, &c.LastError, &c.UpdatedAt, &c.TopicID)
	return c, e
}
func (s *Store) closureColumns() (string, error) {
	has, err := hasColumn(s.db, "sessions", "topic_id")
	if err != nil {
		return "", err
	}
	if has {
		return closureBaseColumns + ", s.topic_id", nil
	}
	return closureBaseColumns + ", ''", nil
}
func (s *Store) Closure(thread string) (Closure, error) {
	closureColumns, e := s.closureColumns()
	if e != nil {
		return Closure{}, e
	}
	return scanClosure(s.db.QueryRow("SELECT "+closureColumns+" FROM session_closures c JOIN sessions s ON s.thread_id = c.thread_id WHERE c.thread_id = ?", thread))
}
func (s *Store) ClosureRows(pendingOnly bool) ([]Closure, error) {
	closureColumns, columnErr := s.closureColumns()
	if columnErr != nil {
		return nil, columnErr
	}
	q := "SELECT " + closureColumns + " FROM session_closures c JOIN sessions s ON s.thread_id = c.thread_id "
	if pendingOnly {
		q += "WHERE c.state = 'pending' AND c.next_attempt <= ? ORDER BY c.next_attempt, c.thread_id LIMIT 100"
	} else {
		q += "ORDER BY c.updated_at DESC, c.thread_id LIMIT 100"
	}
	var rows *sql.Rows
	var e error
	if pendingOnly {
		rows, e = s.db.Query(q, time.Now().Unix())
	} else {
		rows, e = s.db.Query(q)
	}
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Closure{}
	for rows.Next() {
		c, e := scanClosure(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) ConfirmClosure(thread string, codexSide bool) error {
	field := "chat_closed"
	if codexSide {
		field = "codex_archived"
	}
	_, e := s.db.Exec("UPDATE session_closures SET "+field+" = 1, updated_at = ? WHERE thread_id = ?", time.Now().Unix(), thread)
	return e
}
func (s *Store) FinishClosure(thread string, protect bool) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	status, reason := "done", ""
	condition := "codex_archived = 1 AND chat_closed = 1"
	if protect {
		status = "protected"
		reason = "Codex 已归档；该群没有客户端创建证明，已保留原群，不会自动解散"
		condition = "codex_archived = 1 AND chat_closed = 0 AND (SELECT managed_chat FROM sessions WHERE thread_id = session_closures.thread_id) = 0"
	}
	result, e := tx.Exec("UPDATE session_closures SET state = ?, last_error = ?, updated_at = ? WHERE thread_id = ? AND "+condition, status, reason, time.Now().Unix(), thread)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return errors.New("尚未满足两端关闭确认条件")
	}
	_, e = tx.Exec("UPDATE sessions SET status = 'archived', updated_at = ? WHERE thread_id = ?", time.Now().Unix(), thread)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) ClosureFailed(thread, reason string) error {
	c, e := s.Closure(thread)
	if e != nil {
		return e
	}
	delay := 10
	for i := 0; i < c.Attempts && delay < 600; i++ {
		delay *= 2
	}
	if delay > 600 {
		delay = 600
	}
	if len(reason) > 600 {
		reason = reason[:600]
	}
	_, e = s.db.Exec("UPDATE session_closures SET attempts = attempts + 1, last_error = ?, next_attempt = ?, updated_at = ? WHERE thread_id = ? AND state = 'pending'", reason, time.Now().Unix()+int64(delay), time.Now().Unix(), thread)
	return e
}
func (s *Store) RetryClosure(thread string) error {
	r, e := s.db.Exec("UPDATE session_closures SET next_attempt = ?, last_error = '' WHERE thread_id = ? AND state = 'pending'", time.Now().Unix(), thread)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return errors.New("该会话没有待重试的关闭任务")
	}
	return nil
}

// LifecycleSessions 同时检查活跃映射和受保护群，补偿离线期间的真正解散。
func (s *Store) LifecycleSessions() ([]Session, error) {
	rows, e := s.db.Query(`SELECT s.thread_id, s.channel, s.connection_id, s.chat_id, s.work_dir, s.owner_id, s.status, s.created_at, s.managed_chat, s.topic_id, s.root_message_id
FROM sessions s
     LEFT JOIN session_closures c ON c.thread_id = s.thread_id
WHERE s.status = 'active'
   OR c.state = 'protected'
ORDER BY s.created_at, s.thread_id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var x Session
		if e = rows.Scan(&x.ThreadID, &x.Address.Channel, &x.Address.Connection, &x.Address.ChatID, &x.WorkDir, &x.Owner, &x.Status, &x.CreatedAt, &x.Managed, &x.Address.TopicID, &x.Address.RootMessageID); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
