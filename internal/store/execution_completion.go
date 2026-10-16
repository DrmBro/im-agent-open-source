package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const awaitingReplyIdentity = "已确认目标轮次执行完成；回复清单尚未完整关联，继续核对，不重新执行"

// RecordExecutionCompletion 接受已有订阅中明确匹配的完成通知，不代表回复已送达。
func (s *Store) RecordExecutionCompletion(session Session, event, turn string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureRequestResult(tx, session, event, turn); err != nil {
		return err
	}
	if err = recordExecutionCompletion(tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmObservedExecution 只接受当前已持久化的确切观察证据，不使用正文或最近时间猜测。
func (s *Store) ConfirmObservedExecution(session Session, p RequestProbe, actualTurn string) error {
	var evidence struct {
		UserItemID  string `json:"user_item_id"`
		MatchedTurn string `json:"matched_turn"`
		Outcome     string `json:"outcome"`
	}
	if actualTurn == "" || p.UserItemID == "" || json.Unmarshal([]byte(p.State), &evidence) != nil ||
		evidence.Outcome != "completed" || evidence.MatchedTurn != actualTurn || evidence.UserItemID != p.UserItemID ||
		p.ThreadID != session.ThreadID || p.AddressKey != Hash(session.Address.Key()) {
		return errors.New("执行完成观察缺少确切关联")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = ensureRequestResult(tx, session, p.EventID, p.TurnRef); err != nil {
		return err
	}
	var valid int
	err = tx.QueryRow(`SELECT count(*) FROM request_results r
         JOIN events e ON e.id = r.event_id
         JOIN live_mirror_origins o ON o.event_id = r.event_id AND o.thread_id = r.thread_id
WHERE r.event_id = ? AND r.thread_id = ? AND r.address_key = ? AND e.chat_key = r.address_key
  AND r.turn_ref = ? AND r.observer_path = ? AND r.observer_offset = ? AND r.observer_state = ?
  AND r.execution_state IN ('submitted','completed') AND r.sealed = 0
  AND o.path = r.observer_path AND o.state = 'matched' AND o.user_item_id = ?
  AND (SELECT count(*) FROM live_mirror_origins x
       WHERE x.thread_id = o.thread_id AND x.user_item_id = o.user_item_id AND x.state = 'matched') = 1
  AND e.status IN ('uncertain','interrupted')
  AND e.stage IN ('waiting_model','execution_unconfirmed','outcome_unconfirmed','reply_delivery_unconfirmed')`,
		p.EventID, p.ThreadID, p.AddressKey, p.TurnRef, p.Path, p.Offset, p.State, p.UserItemID).Scan(&valid)
	if err != nil {
		return err
	}
	if valid != 1 {
		return errors.New("执行完成观察已过期或来源冲突，未修改状态")
	}
	if err = recordExecutionCompletion(tx, p.EventID); err != nil {
		return err
	}
	return tx.Commit()
}

func recordExecutionCompletion(tx *sql.Tx, event string) error {
	var execution, status, stage string
	var sealed int
	if err := tx.QueryRow(`SELECT r.execution_state,e.status,e.stage,r.sealed
FROM request_results r JOIN events e ON e.id = r.event_id
WHERE r.event_id = ?`, event).Scan(&execution, &status, &stage, &sealed); err != nil {
		return err
	}
	if execution == "completed" {
		return nil // 重复核对不刷新完成时间，也不产生重复审计或卡片更新。
	}
	if sealed != 0 || execution != "submitted" || status != "processing" && status != "uncertain" && status != "interrupted" {
		return errors.New("请求状态与执行完成证据冲突")
	}
	if err := auditRequest(tx, event, "execution_confirmed", status, stage, "已确认目标轮次完成；回复清单与投递回执独立核对"); err != nil {
		return err
	}
	now := time.Now().Unix()
	result, err := tx.Exec(`UPDATE request_results
SET execution_state = 'completed', delivery_state = 'awaiting_replies', completed_at = ?,
    observation_error = ?, updated_at = ?
WHERE event_id = ? AND execution_state = 'submitted' AND sealed = 0`, now, awaitingReplyIdentity, now, event)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("执行完成记录未写入，未更新事件状态")
	}
	_, err = tx.Exec(`UPDATE events
SET stage = 'reply_delivery_unconfirmed', error_summary = ?, updated_at = ?
WHERE id = ?`, awaitingReplyIdentity, now, event)
	return err
}
