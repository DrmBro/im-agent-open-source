package store

import (
	"context"
	"errors"
	"time"
)

type MessageForwardRecord struct {
	ID      string `json:"id"`
	Thread  string `json:"thread"`
	Message string `json:"message"`
	Target  string `json:"target"`
	State   string `json:"state"`
	Sent    int    `json:"sent"`
	Total   int    `json:"total"`
	Updated int64  `json:"updated"`
}

func (s *Store) ensureMessageForwards(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS message_forwards(id TEXT PRIMARY KEY,thread TEXT NOT NULL,message TEXT NOT NULL,target TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL,sent INTEGER NOT NULL DEFAULT 0,total INTEGER NOT NULL,updated INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS message_forward_parts(id TEXT NOT NULL,part INTEGER NOT NULL,platform TEXT NOT NULL,PRIMARY KEY(id,part));`)
	return err
}
func (s *Store) ReserveMessageForward(ctx context.Context, r MessageForwardRecord, body string) (bool, error) {
	if err := s.ensureMessageForwards(ctx); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO message_forwards(id,thread,message,target,body,state,total,updated) VALUES(?,?,?,?,?,'sending',?,?) ON CONFLICT(id) DO NOTHING`, r.ID, r.Thread, r.Message, r.Target, body, r.Total, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (s *Store) ForwardPartSent(ctx context.Context, id string, part int, platform string) error {
	if platform == "" {
		return errors.New("投递缺少平台回执")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_forward_parts(id,part,platform) VALUES(?,?,?)`, id, part, platform); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE message_forwards SET sent=sent+1,updated=? WHERE id=? AND state='sending'`, time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) FinishMessageForward(ctx context.Context, id, state string) error {
	if state != "sent" && state != "unknown" && state != "failed" {
		return errors.New("投递状态无效")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE message_forwards SET state=?,updated=? WHERE id=? AND state='sending'`, state, time.Now().Unix(), id)
	return err
}
func (s *Store) MessageForwards(ctx context.Context) ([]MessageForwardRecord, error) {
	if err := s.ensureMessageForwards(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,thread,message,target,state,sent,total,updated FROM message_forwards ORDER BY updated DESC,id LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageForwardRecord{}
	for rows.Next() {
		var r MessageForwardRecord
		if err = rows.Scan(&r.ID, &r.Thread, &r.Message, &r.Target, &r.State, &r.Sent, &r.Total, &r.Updated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) migrateMessageForwards() error {
	ctx := context.Background()
	if err := s.ensureMessageForwards(ctx); err != nil {
		return err
	}
	_, err := s.db.Exec("UPDATE message_forwards SET state='unknown' WHERE state='sending'")
	return err
}

func (s *Store) MessageForwardRecord(ctx context.Context, id string) (MessageForwardRecord, error) {
	var r MessageForwardRecord
	err := s.db.QueryRowContext(ctx, "SELECT id,thread,message,target,state,sent,total,updated FROM message_forwards WHERE id=?", id).Scan(&r.ID, &r.Thread, &r.Message, &r.Target, &r.State, &r.Sent, &r.Total, &r.Updated)
	return r, err
}
