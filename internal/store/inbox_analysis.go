package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type InboxAnalysis struct {
	Attention        bool     `json:"attention"`
	NewSession       bool     `json:"new_session"`
	Reason           string   `json:"reason"`
	CompletedRequest string   `json:"completed_request,omitempty"`
	Evidence         []string `json:"evidence,omitempty"`
	Note             string   `json:"note,omitempty"`
	Failed           bool     `json:"failed,omitempty"`
}

func (s *Store) migrateInboxAnalysis() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbox_analysis(event_id TEXT PRIMARY KEY REFERENCES personal_inbox(id) ON DELETE CASCADE,result TEXT NOT NULL, attempts INTEGER NOT NULL, retry_at INTEGER NOT NULL)`)
	return err
}

func (s *Store) NextInboxAnalysis(ctx context.Context, since int64) (id string, received int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT p.id,p.received_at FROM personal_inbox p LEFT JOIN inbox_analysis a ON a.event_id=p.id
 WHERE p.state='new' AND p.received_at>=? AND (a.event_id IS NULL OR (a.attempts<3 AND a.retry_at>0 AND a.retry_at<=?))
 ORDER BY p.received_at,p.id LIMIT 1`, since, time.Now().Unix()).Scan(&id, &received)
	return
}

func (s *Store) SaveInboxAnalysis(ctx context.Context, id string, a InboxAnalysis) error {
	result, err := json.Marshal(a)
	if err != nil {
		return err
	}
	var retry int64
	if a.Failed {
		retry = time.Now().Add(time.Minute).Unix()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO inbox_analysis(event_id,result,attempts,retry_at)
 SELECT id,?,1,? FROM personal_inbox WHERE id=? AND state='new'
 ON CONFLICT(event_id) DO UPDATE SET result=excluded.result, attempts=inbox_analysis.attempts+1, retry_at=excluded.retry_at`, string(result), retry, id)
	return err
}

func (s *Store) InboxSupplement(id string) (string, error) {
	var raw string
	err := s.db.QueryRow("SELECT result FROM inbox_analysis WHERE event_id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var a InboxAnalysis
	if err = json.Unmarshal([]byte(raw), &a); err != nil {
		return "", err
	}
	return a.CompletedRequest, nil
}
