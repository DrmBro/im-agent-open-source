package store

import (
	"database/sql"
	"errors"
	"time"
)

// 与原关闭意图独立，防止“写入后丢回执/进程重启”触发第二次归档。
type ClosureArchiveAttempt struct {
	ThreadID, AddressKey, Route, RequestID, State, Detail string
	UpdatedAt                                             int64
}

func (s *Store) migrateClosureArchive() error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.Exec(`CREATE TABLE IF NOT EXISTS closure_archive_attempts (
 thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id),address_key TEXT NOT NULL,
 route TEXT NOT NULL,request_id TEXT NOT NULL,state TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL
 )`)
	if e != nil {
		return e
	}
	_, e = tx.Exec("UPDATE closure_archive_attempts SET state='unknown',detail='上次归档发送过程中断，结果未确认；仅核对状态，不重发',updated_at=? WHERE state='sending'", time.Now().Unix())
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) ClosureArchiveAttempt(thread string) (ClosureArchiveAttempt, error) {
	var a ClosureArchiveAttempt
	e := s.db.QueryRow("SELECT thread_id,address_key,route,request_id,state,detail,updated_at FROM closure_archive_attempts WHERE thread_id=?", thread).Scan(&a.ThreadID, &a.AddressKey, &a.Route, &a.RequestID, &a.State, &a.Detail, &a.UpdatedAt)
	return a, e
}

// closedArchiveBinding必须在同一事务内验证真实持久化关闭意图和地址。
func closedArchiveBinding(tx *sql.Tx, session Session) error {
	var n int
	e := tx.QueryRow(`SELECT count(*) FROM sessions s
         JOIN session_closures c ON c.thread_id = s.thread_id
WHERE s.thread_id = ? AND s.channel = ? AND s.connection_id = ? AND s.chat_id = ?
  AND s.topic_id = '' AND s.root_message_id = '' AND s.managed_chat = 1 AND s.status = 'closing'
  AND c.state = 'pending' AND c.chat_closed = 1 AND c.codex_archived = 0`, session.ThreadID, session.Address.Channel, session.Address.Connection, session.Address.ChatID).Scan(&n)
	if e != nil {
		return e
	}
	if n != 1 || session.Address.IsTopic() || session.Address.RootMessageID != "" {
		return errors.New("不是已确认解散且待归档的独立自建群，未发送归档")
	}
	return nil
}
func (s *Store) ReserveClosureArchive(session Session, route string) (ClosureArchiveAttempt, bool, error) {
	a := ClosureArchiveAttempt{ThreadID: session.ThreadID, AddressKey: Hash(session.Address.Key()), Route: route, RequestID: ID(), State: "sending", UpdatedAt: time.Now().Unix()}
	if route != "desktop_compat" && route != "independent" {
		return a, false, errors.New("未知归档路由")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return a, false, e
	}
	defer tx.Rollback()
	if e = closedArchiveBinding(tx, session); e != nil {
		return a, false, e
	}
	r, e := tx.Exec(`INSERT INTO closure_archive_attempts(thread_id,address_key,route,request_id,state,updated_at)
VALUES (?,?,?,?,'sending',?)
ON CONFLICT(thread_id) DO UPDATE SET route=excluded.route,request_id=excluded.request_id,
    state='sending',detail='',updated_at=excluded.updated_at
WHERE closure_archive_attempts.state = 'rejected' AND closure_archive_attempts.address_key = excluded.address_key`, a.ThreadID, a.AddressKey, a.Route, a.RequestID, a.UpdatedAt)
	if e != nil {
		return a, false, e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return a, false, e
	}
	if n != 1 {
		return a, false, nil
	}
	return a, true, tx.Commit()
}
func (s *Store) CompleteClosureArchive(a ClosureArchiveAttempt, state, detail string) error {
	if state != "confirmed" && state != "rejected" && state != "unknown" {
		return errors.New("无效归档发送结果")
	}
	if len([]rune(detail)) > 600 {
		detail = string([]rune(detail)[:600])
	}
	r, e := s.db.Exec(`UPDATE closure_archive_attempts SET state=?,detail=?,updated_at=?
WHERE thread_id=? AND address_key=? AND request_id=? AND route=? AND state='sending'`, state, detail, time.Now().Unix(), a.ThreadID, a.AddressKey, a.RequestID, a.Route)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("归档尝试记录已变化，不覆盖旧结果")
	}
	return nil
}
