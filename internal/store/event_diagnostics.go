package store

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// 仅保存脱敏诊断，不保存用户输入、凭证或模型原始错误体。
type EventDiagnostic struct {
	Status     string `json:"status"`
	Stage      string `json:"stage"`
	Summary    string `json:"summary"`
	ReceivedAt int64  `json:"received_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

func (s *Store) migrateEventDiagnostics() error {
	rows, e := s.db.Query("PRAGMA table_info(events)")
	if e != nil {
		return e
	}
	found := map[string]bool{}
	for rows.Next() {
		var id, notnull, pk int
		var name, kind string
		var value any
		if e = rows.Scan(&id, &name, &kind, &notnull, &value, &pk); e != nil {
			rows.Close()
			return e
		}
		found[name] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, change := range []struct{ name, definition string }{{"chat_key", "TEXT NOT NULL DEFAULT ''"}, {"stage", "TEXT NOT NULL DEFAULT ''"}, {"error_summary", "TEXT NOT NULL DEFAULT ''"}, {"updated_at", "INTEGER NOT NULL DEFAULT 0"}} {
		if !found[change.name] {
			if _, e = s.db.Exec("ALTER TABLE events ADD COLUMN " + change.name + " " + change.definition); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *Store) EventStage(id, chatKey, stage, summary string) error {
	_, e := s.db.Exec("UPDATE events SET chat_key = ?, stage = ?, error_summary = ?, updated_at = ? WHERE id = ?", Hash(chatKey), stage, summary, time.Now().Unix(), id)
	return e
}
func EventDiagnosticsSnapshot(dir string) ([]EventDiagnostic, error) {
	p := filepath.Join(dir, "sessions.db")
	st, e := os.Lstat(p)
	if os.IsNotExist(e) {
		return []EventDiagnostic{}, nil
	}
	if e != nil || !st.Mode().IsRegular() {
		return nil, errors.New("业务数据库不可读取")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(p), RawQuery: "mode=ro"}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec("PRAGMA busy_timeout=1000"); e != nil {
		return nil, e
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM pragma_table_info('events') WHERE name = 'error_summary'").Scan(&n); e != nil {
		return nil, e
	}
	if n == 0 {
		return []EventDiagnostic{}, nil
	}
	rows, e := db.Query("SELECT status, stage, error_summary, created_at, updated_at FROM events ORDER BY created_at DESC, id LIMIT 20")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []EventDiagnostic{}
	for rows.Next() {
		var event EventDiagnostic
		if e = rows.Scan(&event.Status, &event.Stage, &event.Summary, &event.ReceivedAt, &event.UpdatedAt); e != nil {
			return nil, e
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) EventFailure(id, summary string) error {
	_, e := s.db.Exec("UPDATE events SET status = 'failed', error_summary = ?, updated_at = ? WHERE id = ?", summary, time.Now().Unix(), id)
	return e
}
