package store

import (
	"errors"
	"time"
)

type DesktopApprovalCard struct {
	ID, ThreadID, AddressKey, Payload, Fingerprint, State, Decision, Detail, MessageID, DeliveryState, Digest string
	ExpiresAt, NextAttempt                                                                                    int64
}

func (s *Store) migrateDesktopApprovals() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS desktop_approval_cards(
 id TEXT PRIMARY KEY,thread_id TEXT NOT NULL REFERENCES sessions(thread_id),address_key TEXT NOT NULL,payload TEXT NOT NULL,fingerprint TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'pending',decision TEXT NOT NULL DEFAULT '',detail TEXT NOT NULL DEFAULT '',message_id TEXT NOT NULL DEFAULT '',
 delivery_state TEXT NOT NULL DEFAULT 'pending',digest TEXT NOT NULL DEFAULT '',expires_at INTEGER NOT NULL,next_attempt INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
 UPDATE desktop_approval_cards SET delivery_state='unknown',detail='上次卡片发送未确认，不自动重发' WHERE delivery_state='sending';
 UPDATE desktop_approval_cards SET state='uncertain',detail='上次审批响应未确认，不自动重发选择' WHERE state='responding';`)
	return err
}
func (s *Store) RegisterDesktopApproval(c DesktopApprovalCard) error {
	_, err := s.db.Exec(`INSERT INTO desktop_approval_cards(id,thread_id,address_key,payload,fingerprint,expires_at,created_at,updated_at)
 SELECT ?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM sessions WHERE thread_id=? AND status='active')
 ON CONFLICT(id) DO UPDATE SET expires_at=0,payload=excluded.payload,
 state=CASE WHEN desktop_approval_cards.state='expired' THEN 'pending' ELSE desktop_approval_cards.state END,
 detail=CASE WHEN desktop_approval_cards.state='expired' THEN '' ELSE desktop_approval_cards.detail END,
 digest=CASE WHEN desktop_approval_cards.state='expired' THEN '' ELSE desktop_approval_cards.digest END
 WHERE desktop_approval_cards.state IN ('pending','expired') AND desktop_approval_cards.decision=''
 AND desktop_approval_cards.fingerprint=excluded.fingerprint AND desktop_approval_cards.thread_id=excluded.thread_id AND desktop_approval_cards.address_key=excluded.address_key`, c.ID, c.ThreadID, c.AddressKey, c.Payload, c.Fingerprint, c.ExpiresAt, time.Now().Unix(), time.Now().Unix(), c.ThreadID)
	return err
}

const approvalColumns = "id,thread_id,address_key,payload,fingerprint,state,decision,detail,message_id,delivery_state,digest,expires_at,next_attempt"

func (s *Store) DesktopApproval(id string) (DesktopApprovalCard, error) {
	var c DesktopApprovalCard
	err := s.db.QueryRow("SELECT "+approvalColumns+" FROM desktop_approval_cards WHERE id=?", id).Scan(&c.ID, &c.ThreadID, &c.AddressKey, &c.Payload, &c.Fingerprint, &c.State, &c.Decision, &c.Detail, &c.MessageID, &c.DeliveryState, &c.Digest, &c.ExpiresAt, &c.NextAttempt)
	return c, err
}
func (s *Store) DesktopApprovalCards() ([]DesktopApprovalCard, error) {
	rows, err := s.db.Query("SELECT " + approvalColumns + " FROM desktop_approval_cards WHERE state IN ('pending','responding','uncertain') OR (message_id<>'' AND digest='') ORDER BY created_at,id LIMIT 128")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DesktopApprovalCard{}
	for rows.Next() {
		var c DesktopApprovalCard
		if err = rows.Scan(&c.ID, &c.ThreadID, &c.AddressKey, &c.Payload, &c.Fingerprint, &c.State, &c.Decision, &c.Detail, &c.MessageID, &c.DeliveryState, &c.Digest, &c.ExpiresAt, &c.NextAttempt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) SetDesktopApprovalState(id, state, detail string) error {
	_, err := s.db.Exec(`UPDATE desktop_approval_cards SET state=?,detail=?,digest='',updated_at=? WHERE id=? AND state IN ('pending','responding','uncertain') AND (state<>? OR detail<>?)`, state, detail, time.Now().Unix(), id, state, detail)
	return err
}
func (s *Store) ClaimDesktopApproval(id, decision, receipt string) (bool, error) {
	if decision != "accept" && decision != "decline" {
		return false, errors.New("审批选择无效")
	}
	r, err := s.db.Exec(`UPDATE desktop_approval_cards SET state='responding',decision=?,detail='正在向桌面发送本次选择',digest='',updated_at=? WHERE id=? AND state='pending' AND (expires_at=0 OR expires_at>?) AND delivery_state='sent' AND message_id=?`, decision, time.Now().Unix(), id, time.Now().Unix(), receipt)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *Store) ClaimDesktopApprovalCard(id string) (bool, error) {
	r, err := s.db.Exec(`UPDATE desktop_approval_cards SET delivery_state='sending',updated_at=? WHERE id=? AND state='pending' AND (expires_at=0 OR expires_at>?) AND delivery_state IN ('pending','failed') AND next_attempt<=? AND message_id=''`, time.Now().Unix(), id, time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *Store) DesktopApprovalCardSent(id, message, digest string) error {
	if message == "" {
		return errors.New("审批卡缺少平台回执")
	}
	_, err := s.db.Exec(`UPDATE desktop_approval_cards SET message_id=?,delivery_state='sent',digest=?,next_attempt=0,updated_at=? WHERE id=? AND delivery_state='sending'`, message, digest, time.Now().Unix(), id)
	return err
}
func (s *Store) DesktopApprovalCardUpdated(c DesktopApprovalCard, digest string) error {
	// 只确认实际展示的状态版本，不能吞掉正在发生的用户选择。
	_, err := s.db.Exec(`UPDATE desktop_approval_cards SET digest=?,next_attempt=0,updated_at=? WHERE id=? AND message_id=? AND state=? AND decision=? AND detail=?`, digest, time.Now().Unix(), c.ID, c.MessageID, c.State, c.Decision, c.Detail)
	return err
}
func (s *Store) DesktopApprovalCardFailed(id string, unknown bool) error {
	state := "failed"
	if unknown {
		state = "unknown"
	}
	_, err := s.db.Exec(`UPDATE desktop_approval_cards SET delivery_state=CASE WHEN message_id='' THEN ? ELSE 'sent' END,next_attempt=?,updated_at=? WHERE id=?`, state, time.Now().Add(20*time.Second).Unix(), time.Now().Unix(), id)
	return err
}
