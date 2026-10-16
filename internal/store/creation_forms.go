package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"im-agent/internal/im"
	"time"
)

type CreationForm struct {
	ID        string     `json:"id"`
	Address   im.Address `json:"address"`
	UserID    string     `json:"user_id"`
	MessageID string     `json:"message_id"`
	Catalog   string     `json:"-"`
	Selection string     `json:"-"`
	State     string     `json:"state"`
	Stage     string     `json:"stage"`
	ThreadID  string     `json:"thread_id"`
	Error     string     `json:"error"`
	ExpiresAt int64      `json:"expires_at"`
}

func (s *Store) migrateCreationForms() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS creation_forms (
 id TEXT PRIMARY KEY,address_json TEXT NOT NULL,user_id TEXT NOT NULL,message_id TEXT NOT NULL DEFAULT '',
 catalog TEXT NOT NULL,selection TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,stage TEXT NOT NULL DEFAULT '',
 thread_id TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',expires_at INTEGER NOT NULL,updated_at INTEGER NOT NULL
 );`)
	if e != nil {
		return e
	}
	_, e = s.db.Exec("UPDATE creation_forms SET state='interrupted',error='上次创建中断，可能已有资源；未自动重建或重放首问' WHERE state='claimed'")
	return e
}
func (s *Store) PutCreationForm(f CreationForm) error {
	_, e := s.db.Exec("INSERT INTO creation_forms(id,address_json,user_id,catalog,state,expires_at,updated_at) VALUES(?,?,?,?,'pending',?,?)", f.ID, JSON(f.Address), f.UserID, f.Catalog, f.ExpiresAt, time.Now().Unix())
	return e
}
func (s *Store) CreationForm(id string) (CreationForm, error) {
	var f CreationForm
	var address string
	e := s.db.QueryRow("SELECT id,address_json,user_id,message_id,catalog,selection,state,stage,thread_id,error,expires_at FROM creation_forms WHERE id=?", id).Scan(&f.ID, &address, &f.UserID, &f.MessageID, &f.Catalog, &f.Selection, &f.State, &f.Stage, &f.ThreadID, &f.Error, &f.ExpiresAt)
	if e != nil {
		return f, e
	}
	if json.Unmarshal([]byte(address), &f.Address) != nil {
		return f, errors.New("创建表单地址无效")
	}
	return f, nil
}
func (s *Store) CreationMessage(id, message string) error {
	_, e := s.db.Exec("UPDATE creation_forms SET message_id=? WHERE id=? AND state='pending'", message, id)
	return e
}
func (s *Store) ClaimCreation(id, selection string) (bool, error) {
	r, e := s.db.Exec("UPDATE creation_forms SET state='claimed',stage='queued',selection=?,updated_at=? WHERE id=? AND state='pending' AND message_id<>'' AND (expires_at=0 OR expires_at>?)", selection, time.Now().Unix(), id, time.Now().Unix())
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) CancelCreation(id string) (bool, error) {
	r, e := s.db.Exec("UPDATE creation_forms SET state='cancelled',updated_at=? WHERE id=? AND state='pending'", time.Now().Unix(), id)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) CreationProgress(id, state, stage, thread, hint string) error {
	guard := ""
	if stage == "first_question" && state != "done" {
		// 回复恢复确认与首问失败回调可能并发，晚到的失败不能覆盖已确认结果。
		guard = " AND NOT EXISTS (SELECT 1 FROM request_results WHERE event_id=creation_forms.id || ':first' AND execution_state='completed' AND delivery_state='sent')"
	}
	_, e := s.db.Exec("UPDATE creation_forms SET state=?,stage=?,thread_id=CASE WHEN ?='' THEN thread_id ELSE ? END,error=?,updated_at=? WHERE id=?"+guard, state, stage, thread, thread, hint, time.Now().Unix(), id)
	return e
}
func (s *Store) CreationOwnedThread(thread string) (bool, error) {
	var n int
	e := s.db.QueryRow("SELECT count(*) FROM creation_forms WHERE thread_id=?", thread).Scan(&n)
	return n > 0, e
}
func creationFormsSnapshot(db *sql.DB) ([]CreationForm, error) {
	var n int
	if e := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='creation_forms'").Scan(&n); e != nil || n == 0 {
		return []CreationForm{}, e
	}
	rows, e := db.Query("SELECT id,state,stage,thread_id,error,expires_at FROM creation_forms ORDER BY updated_at DESC LIMIT 50")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []CreationForm{}
	for rows.Next() {
		var f CreationForm
		if e = rows.Scan(&f.ID, &f.State, &f.Stage, &f.ThreadID, &f.Error, &f.ExpiresAt); e != nil {
			return nil, e
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
