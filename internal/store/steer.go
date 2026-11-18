package store

import (
	"errors"
	"time"
)

func (s *Store) migrateSteer() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS steer_inputs(event_id TEXT PRIMARY KEY REFERENCES events(id),thread_id TEXT NOT NULL,turn_id TEXT NOT NULL,state TEXT NOT NULL,updated_at INTEGER NOT NULL);
 UPDATE events SET status='uncertain',stage='steer_unknown',error_summary='补充提交回执未确认，未自动重发' WHERE id IN (SELECT event_id FROM steer_inputs WHERE state='submitting');
 UPDATE steer_inputs SET state='unknown' WHERE state='submitting';`)
	return err
}

// 只有仍在队列中的消息可以被领取，阻止旧的普通队列任务再次执行。
func (s *Store) ReserveSteer(session Session, event, turn string) (bool, error) {
	if turn == "" {
		return false, errors.New("补充消息缺少轮次")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE events SET stage='steer_submitting',updated_at=? WHERE id=? AND chat_key=? AND status='processing' AND stage='queued'
 AND EXISTS(SELECT 1 FROM sessions WHERE thread_id=? AND status='active')
 AND NOT EXISTS(SELECT 1 FROM request_results WHERE event_id=?)
 AND NOT EXISTS(SELECT 1 FROM steer_inputs WHERE event_id=?)`, time.Now().Unix(), event, Hash(session.Address.Key()), session.ThreadID, event, event)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if _, err = tx.Exec(`INSERT INTO steer_inputs(event_id,thread_id,turn_id,state,updated_at) VALUES(?,?,?,'submitting',?)`, event, session.ThreadID, turn, time.Now().Unix()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
func (s *Store) FinishSteer(event, state, detail string) error {
	status, stage := "uncertain", "steer_unknown"
	if state == "submitted" {
		status, stage = "bundled", "steer_submitted"
	} else if state == "failed" {
		status, stage = "failed", "not_sent"
	} else if state != "unknown" {
		return errors.New("补充状态无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE steer_inputs SET state=?,updated_at=? WHERE event_id=? AND state='submitting'`, state, time.Now().Unix(), event); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE events SET status=?,stage=?,error_summary=?,updated_at=? WHERE id=? AND stage='steer_submitting'`, status, stage, detail, time.Now().Unix(), event); err != nil {
		return err
	}
	return tx.Commit()
}
