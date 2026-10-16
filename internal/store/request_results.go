package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RequestResult与events的执行显示、session_status_cards的展示回执分别保存。
type RequestResult struct {
	EventID          string         `json:"event_id"`
	TurnRef          string         `json:"turn_ref"`
	Execution        string         `json:"execution"`
	Delivery         string         `json:"delivery"`
	ExpectedReplies  int            `json:"expected_replies"`
	ExpectedParts    int            `json:"expected_parts"`
	ConfirmedParts   int            `json:"confirmed_parts"`
	CompletedAt      int64          `json:"completed_at"`
	DeliveredAt      int64          `json:"delivered_at"`
	Recovered        bool           `json:"recovered"`
	ObservationError string         `json:"observation_error,omitempty"`
	Audit            []RequestAudit `json:"audit,omitempty"`
}
type RequestAudit struct {
	Reason         string `json:"reason"`
	PreviousStatus string `json:"previous_status"`
	PreviousStage  string `json:"previous_stage"`
	Detail         string `json:"detail"`
	At             int64  `json:"at"`
}
type RequestProbe struct {
	EventID, ThreadID, AddressKey, TurnRef, Path, UserItemID, State string
	Offset                                                          int64
}

func (s *Store) migrateRequestResults() error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.Exec(`CREATE TABLE IF NOT EXISTS request_results (
 event_id TEXT PRIMARY KEY REFERENCES events(id),thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
 address_key TEXT NOT NULL,turn_ref TEXT NOT NULL,execution_state TEXT NOT NULL DEFAULT 'submitted',
 sealed INTEGER NOT NULL DEFAULT 0,expected_replies INTEGER NOT NULL DEFAULT 0,
 delivery_state TEXT NOT NULL DEFAULT 'waiting_execution',completed_at INTEGER NOT NULL DEFAULT 0,
 delivered_at INTEGER NOT NULL DEFAULT 0,recovered INTEGER NOT NULL DEFAULT 0,
 observer_path TEXT NOT NULL DEFAULT '',observer_offset INTEGER NOT NULL DEFAULT 0,
 observer_state TEXT NOT NULL DEFAULT '',next_probe INTEGER NOT NULL DEFAULT 0,
 observation_error TEXT NOT NULL DEFAULT '',reply_checked_at INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL
 );
 CREATE TABLE IF NOT EXISTS request_result_replies (
 event_id TEXT NOT NULL REFERENCES request_results(event_id),thread_id TEXT NOT NULL,item_id TEXT NOT NULL,
 expected_parts INTEGER NOT NULL CHECK(expected_parts > 0),PRIMARY KEY(event_id,item_id),UNIQUE(thread_id,item_id),
 FOREIGN KEY(thread_id,item_id) REFERENCES live_mirror_entries(thread_id,item_id)
 );
 CREATE TABLE IF NOT EXISTS request_result_audit (
 seq INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL REFERENCES events(id),
 reason TEXT NOT NULL,previous_status TEXT NOT NULL,previous_stage TEXT NOT NULL,detail TEXT NOT NULL,created_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS request_result_audit_event ON request_result_audit(event_id,seq);
 CREATE INDEX IF NOT EXISTS request_results_delivery ON request_results(thread_id,execution_state,delivery_state);`)
	if e != nil {
		return e
	}
	return tx.Commit()
}

// 仅接受当前请求与目标地址已有的确切绑定，不从相似正文或最近时间推导。
func ensureRequestResult(tx *sql.Tx, session Session, event, turn string) error {
	if event == "" || turn == "" {
		return errors.New("请求或提交回执缺少标识")
	}
	var status, key string
	if e := tx.QueryRow("SELECT e.chat_key,s.status FROM events e JOIN sessions s ON s.thread_id=? WHERE e.id=?", session.ThreadID, event).Scan(&key, &status); e != nil {
		return e
	}
	if key != Hash(session.Address.Key()) || status != "active" {
		return errors.New("请求与活跃会话绑定不一致")
	}
	// 同时校验数据库当前地址，调用者持有的会话快照不能作为变更后的授权。
	var channel, connection, chat, topic string
	if e := tx.QueryRow("SELECT channel,connection_id,chat_id,topic_id FROM sessions WHERE thread_id=?", session.ThreadID).Scan(&channel, &connection, &chat, &topic); e != nil {
		return e
	}
	if channel != session.Address.Channel || connection != session.Address.Connection || chat != session.Address.ChatID || topic != session.Address.TopicID {
		return errors.New("请求目标地址已变化")
	}
	_, e := tx.Exec(`INSERT INTO request_results(event_id,thread_id,address_key,turn_ref,created_at,updated_at)
VALUES (?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, event, session.ThreadID, key, turn, time.Now().Unix(), time.Now().Unix())
	if e != nil {
		return e
	}
	var savedThread, savedKey, savedTurn string
	if e = tx.QueryRow("SELECT thread_id,address_key,turn_ref FROM request_results WHERE event_id=?", event).Scan(&savedThread, &savedKey, &savedTurn); e != nil {
		return e
	}
	if savedThread != session.ThreadID || savedKey != key || savedTurn != turn {
		return errors.New("请求的提交回执或会话已存在冲突，未覆盖")
	}
	return nil
}

// 提交前将请求来源和观察位置同事务落盘；即便提交后进程退出，
// 仍可按原始用户 item_id 核对轮次，而不是按文本重发输入。
func (s *Store) RegisterSubmissionIntent(session Session, event, path string, offset int64, original, rendered string) error {
	if path == "" || offset < 0 || original == "" || rendered == "" {
		return errors.New("请求来源证据不足，未提交")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	intent := "intent:" + event
	if err = ensureRequestResult(tx, session, event, intent); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO live_mirror_origins
(event_id,thread_id,path,min_offset,original_hash,rendered_hash)
VALUES (?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, event, session.ThreadID, path, offset, original, rendered); err != nil {
		return err
	}
	var savedThread, savedPath, savedOriginal, savedRendered string
	var savedOffset int64
	if err = tx.QueryRow(`SELECT thread_id,path,min_offset,original_hash,rendered_hash
FROM live_mirror_origins WHERE event_id=?`, event).Scan(&savedThread, &savedPath, &savedOffset, &savedOriginal, &savedRendered); err != nil {
		return err
	}
	if savedThread != session.ThreadID || savedPath != path || savedOffset != offset || savedOriginal != original || savedRendered != rendered {
		return errors.New("请求来源已有冲突记录，未提交")
	}
	result, err := tx.Exec(`UPDATE request_results SET execution_state='pending_submission',
observer_path=?,observer_offset=?,updated_at=?
WHERE event_id=? AND thread_id=? AND turn_ref=? AND sealed=0`, path, offset, time.Now().Unix(), event, session.ThreadID, intent)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return errors.New("提交前持久化结果未确认，未继续发送")
	}
	return tx.Commit()
}

func (s *Store) RecordRequestSubmission(session Session, event, turn string) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`UPDATE request_results SET turn_ref=?,execution_state='submitted',updated_at=?
WHERE event_id=? AND thread_id=? AND address_key=? AND turn_ref=?
  AND execution_state='pending_submission' AND sealed=0`, turn, time.Now().Unix(), event,
		session.ThreadID, Hash(session.Address.Key()), "intent:"+event); e != nil {
		return e
	}
	if e = ensureRequestResult(tx, session, event, turn); e != nil {
		return e
	}
	_, e = tx.Exec(`UPDATE request_results
SET observer_path = coalesce((SELECT path FROM live_mirror_origins WHERE event_id = ? AND thread_id = ?), ''),
    observer_offset = coalesce((SELECT min_offset FROM live_mirror_origins WHERE event_id = ? AND thread_id = ?), 0)
WHERE event_id = ? AND observer_path = '' AND sealed = 0`, event, session.ThreadID, event, session.ThreadID, event)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func auditRequest(tx *sql.Tx, event, reason, status, stage, detail string) error {
	_, e := tx.Exec("INSERT INTO request_result_audit(event_id,reason,previous_status,previous_stage,detail,created_at) VALUES(?,?,?,?,?,?)", event, reason, status, stage, detail, time.Now().Unix())
	return e
}

// 回复入账与封存完整清单同一事务；在投递网络请求前持久化。
func (s *Store) SealRequestReplies(session Session, event, turn string, entries []MirrorEntry) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = ensureRequestResult(tx, session, event, turn); e != nil {
		return e
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Role != "assistant" || entry.ItemID == "" {
			return errors.New("回复清单缺少助手消息身份")
		}
		ids = append(ids, entry.ItemID)
		if e = prepareMirrorEntry(tx, session.ThreadID, "", entry); e != nil {
			return e
		}
	}
	if e = sealRequestReplyIDs(tx, event, session.ThreadID, ids, "执行完成回执"); e != nil {
		return e
	}
	// 与封存同事务标记投递阶段，避免恢复先完成后被晚到的阶段更新覆盖。
	if _, e = tx.Exec("UPDATE events SET stage='delivering_reply',updated_at=? WHERE id=? AND status='processing'", time.Now().Unix(), event); e != nil {
		return e
	}
	return tx.Commit()
}
func sealRequestReplyIDs(tx *sql.Tx, event, thread string, ids []string, evidence string) error {
	if len(ids) == 0 {
		return errors.New("回复集合不完整，未确认完成")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			return errors.New("回复清单标识缺失或重复，未合并记录")
		}
		seen[id] = true
	}
	var sealed, count int
	if e := tx.QueryRow("SELECT sealed,expected_replies FROM request_results WHERE event_id=?", event).Scan(&sealed, &count); e != nil {
		return e
	}
	if sealed != 0 && count != len(ids) {
		return errors.New("完成回执的回复集合与已封存清单不一致")
	}
	for _, id := range ids {
		var role string
		var suppressed, parts int
		if e := tx.QueryRow("SELECT role,suppressed FROM live_mirror_entries WHERE thread_id=? AND item_id=?", thread, id).Scan(&role, &suppressed); e != nil {
			return e
		}
		if role != "assistant" || suppressed != 0 {
			return errors.New("回复条目不是可投递的助手消息")
		}
		if e := tx.QueryRow("SELECT count(*) FROM live_mirror_parts WHERE thread_id=? AND item_id=?", thread, id).Scan(&parts); e != nil {
			return e
		}
		if parts == 0 {
			return errors.New("回复尚未完整进入投递账本")
		}
		if sealed != 0 {
			var expected int
			if e := tx.QueryRow("SELECT expected_parts FROM request_result_replies WHERE event_id=? AND thread_id=? AND item_id=?", event, thread, id).Scan(&expected); e != nil {
				return e
			}
			if expected != parts {
				return errors.New("已封存回复分片数发生变化")
			}
		} else {
			// UNIQUE(thread_id,item_id)拒绝把同一回复归到另一请求，不忽略冲突。
			if _, e := tx.Exec("INSERT INTO request_result_replies(event_id,thread_id,item_id,expected_parts) VALUES(?,?,?,?)", event, thread, id, parts); e != nil {
				return e
			}
		}
	}
	if sealed != 0 {
		return nil
	}
	var status, stage, detail string
	if e := tx.QueryRow("SELECT status,stage,error_summary FROM events WHERE id=?", event).Scan(&status, &stage, &detail); e != nil {
		return e
	}
	if e := auditRequest(tx, event, "execution_completed", status, stage, evidence+"；原记录："+detail); e != nil {
		return e
	}
	_, e := tx.Exec("UPDATE request_results SET execution_state='completed',sealed=1,expected_replies=?,delivery_state='pending',completed_at=CASE WHEN completed_at > 0 THEN completed_at ELSE ? END,observation_error='',updated_at=? WHERE event_id=? AND sealed=0", len(ids), time.Now().Unix(), time.Now().Unix(), event)
	return e
}

// MarkRequestFailure防止恢复已完成后，晚到的旧失败回调把任务降级。
// 图文关联状态与事件同事务写入，避免恢复完成后再覆盖图片组状态。
func (s *Store) MarkRequestFailure(event, chatKey, status, stage, detail string) (bool, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	var beforeStatus, beforeStage, beforeDetail string
	if e = tx.QueryRow("SELECT status,stage,error_summary FROM events WHERE id=? AND chat_key=?", event, Hash(chatKey)).Scan(&beforeStatus, &beforeStage, &beforeDetail); e != nil {
		return false, e
	}
	var delivered int
	if e = tx.QueryRow("SELECT count(*) FROM request_results WHERE event_id=? AND delivery_state='sent' AND execution_state='completed'", event).Scan(&delivered); e != nil {
		return false, e
	}
	if delivered != 0 {
		return false, nil
	}
	if stage != "reply_delivery_unconfirmed" {
		var completed int
		if e = tx.QueryRow("SELECT count(*) FROM request_results WHERE event_id=? AND execution_state='completed'", event).Scan(&completed); e != nil {
			return false, e
		}
		if completed != 0 {
			return false, nil
		}
	}
	if e = auditRequest(tx, event, "request_unfinished", beforeStatus, beforeStage, "原记录："+beforeDetail+"；本次："+detail); e != nil {
		return false, e
	}
	if stage == "execution_failed" {
		if _, e = tx.Exec("UPDATE request_results SET execution_state='failed',delivery_state='not_applicable',updated_at=? WHERE event_id=? AND sealed=0", time.Now().Unix(), event); e != nil {
			return false, e
		}
	}
	if _, e = tx.Exec("UPDATE events SET status=?,stage=?,error_summary=?,updated_at=? WHERE id=?", status, stage, detail, time.Now().Unix(), event); e != nil {
		return false, e
	}
	if _, e = tx.Exec("UPDATE image_drafts SET state=? WHERE trigger_id=? AND state IN ('submitted','interrupted','uncertain')", status, event); e != nil {
		return false, e
	}
	return true, tx.Commit()
}

// 只检查该请求封存的消息与分片，不使用群级积压数量作为完成依据。
func (s *Store) ReconcileRequestReplies(session Session) ([]string, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rows, e := tx.Query(`SELECT r.event_id, r.expected_replies, e.status, e.stage, e.error_summary
FROM request_results r
         JOIN events e ON e.id = r.event_id
         JOIN sessions s ON s.thread_id = r.thread_id
WHERE r.thread_id = ? AND r.address_key = ? AND e.chat_key = r.address_key
  AND s.status = 'active' AND s.channel = ? AND s.connection_id = ? AND s.chat_id = ? AND s.topic_id = ?
  AND r.execution_state = 'completed' AND r.sealed = 1 AND r.delivery_state = 'pending'
  AND e.stage IN ('delivering_reply','reply_delivery_unconfirmed','waiting_model','execution_unconfirmed','completed')
ORDER BY r.reply_checked_at, r.created_at, r.event_id LIMIT 64`, session.ThreadID, Hash(session.Address.Key()), session.Address.Channel, session.Address.Connection, session.Address.ChatID, session.Address.TopicID)
	if e != nil {
		return nil, e
	}
	type candidate struct {
		id, status, stage, detail string
		expected                  int
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if e = rows.Scan(&c.id, &c.expected, &c.status, &c.stage, &c.detail); e != nil {
			rows.Close()
			return nil, e
		}
		candidates = append(candidates, c)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	var completed []string
	for _, c := range candidates {
		if _, e = tx.Exec("UPDATE request_results SET reply_checked_at=? WHERE event_id=?", time.Now().Unix(), c.id); e != nil {
			return nil, e
		}
		var count, bad int
		e = tx.QueryRow(`SELECT count(*), coalesce(sum(CASE
           WHEN (SELECT count(*) FROM live_mirror_parts p WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id) <> m.expected_parts
             OR EXISTS (SELECT 1 FROM live_mirror_parts p WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id AND (p.state <> 'sent' OR p.part_index < 0 OR p.part_index >= m.expected_parts))
           THEN 1 ELSE 0 END), 0)
FROM request_result_replies m WHERE m.event_id = ? AND m.thread_id = ?`, c.id, session.ThreadID).Scan(&count, &bad)
		if e != nil {
			return nil, e
		}
		if count == 0 || count != c.expected || bad != 0 {
			continue
		}
		recovered := c.status == "uncertain" || c.status == "interrupted" || c.stage == "reply_delivery_unconfirmed"
		reason := "reply_delivered"
		if recovered {
			reason = "reply_delivery_recovered"
		}
		if e = auditRequest(tx, c.id, reason, c.status, c.stage, fmt.Sprintf("该请求%d条回复的全部分片已确认送达；原记录：%s", count, c.detail)); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("UPDATE request_results SET delivery_state='sent',delivered_at=?,recovered=?,updated_at=? WHERE event_id=? AND delivery_state='pending'", time.Now().Unix(), recovered, time.Now().Unix(), c.id); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("UPDATE events SET status='done',stage='completed',error_summary='',updated_at=? WHERE id=?", time.Now().Unix(), c.id); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("UPDATE image_drafts SET state='done' WHERE trigger_id=? AND state IN ('submitted','uncertain','interrupted','failed')", c.id); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("UPDATE creation_forms SET state='done',stage='completed',error='',updated_at=? WHERE id || ':first' = ? AND thread_id=? AND stage IN ('first_question','submitting_first_question')", time.Now().Unix(), c.id, session.ThreadID); e != nil {
			return nil, e
		}
		completed = append(completed, c.id)
	}
	return completed, tx.Commit()
}

func (s *Store) PendingRequestProbes() ([]RequestProbe, error) {
	rows, e := s.db.Query(`SELECT r.event_id,r.thread_id,r.address_key,r.turn_ref,r.observer_path,r.observer_offset,r.observer_state,o.user_item_id
FROM request_results r
         JOIN events e ON e.id = r.event_id
         JOIN sessions s ON s.thread_id = r.thread_id
         JOIN live_mirror_origins o ON o.event_id = r.event_id AND o.thread_id = r.thread_id AND o.path = r.observer_path
WHERE r.execution_state IN ('pending_submission','submitted','completed') AND r.sealed = 0 AND r.observer_path <> ''
  AND o.state = 'matched' AND o.user_item_id <> '' AND s.status = 'active'
  AND (SELECT count(*) FROM live_mirror_origins x WHERE x.thread_id = o.thread_id AND x.user_item_id = o.user_item_id AND x.state = 'matched') = 1
  AND e.status IN ('uncertain','interrupted') AND e.chat_key = r.address_key
  AND e.stage IN ('waiting_model','submission_unconfirmed','execution_unconfirmed','outcome_unconfirmed','reply_delivery_unconfirmed')
  AND r.next_probe <= ?
ORDER BY r.next_probe,r.created_at,r.event_id LIMIT 16`, time.Now().Unix())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []RequestProbe
	for rows.Next() {
		var p RequestProbe
		if e = rows.Scan(&p.EventID, &p.ThreadID, &p.AddressKey, &p.TurnRef, &p.Path, &p.Offset, &p.State, &p.UserItemID); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) SaveRequestProbe(p RequestProbe, next int64, state, hint string) (bool, error) {
	if next < p.Offset {
		return false, errors.New("恢复观察游标不能回退")
	}
	delay := int64(5)
	if hint != "" {
		delay = 60
	}
	result, e := s.db.Exec(`UPDATE request_results
SET observer_offset = ?, observer_state = ?, observation_error = ?, next_probe = ?, updated_at = ?
WHERE event_id = ? AND thread_id = ? AND turn_ref = ? AND address_key = ?
  AND observer_offset = ? AND observer_state = ? AND observer_path = ?
  AND execution_state IN ('pending_submission','submitted','completed') AND sealed = 0`, next, state, hint, time.Now().Unix()+delay, time.Now().Unix(), p.EventID, p.ThreadID, p.TurnRef, p.AddressKey, p.Offset, p.State, p.Path)
	if e != nil {
		return false, e
	}
	n, e := result.RowsAffected()
	return n == 1, e
}
func (s *Store) SealObservedRequest(session Session, p RequestProbe, ids []string, actualTurn string, entries ...MirrorEntry) error {
	if actualTurn == "" {
		return errors.New("恢复观察缺少实际轮次标识")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = ensureRequestResult(tx, session, p.EventID, p.TurnRef); e != nil {
		return e
	}
	var user, status, stage string
	if e = tx.QueryRow("SELECT user_item_id FROM live_mirror_origins WHERE event_id=? AND thread_id=? AND state='matched'", p.EventID, p.ThreadID).Scan(&user); e != nil {
		return e
	}
	if user != p.UserItemID {
		return errors.New("恢复观察的用户消息关联已变化")
	}
	if e = tx.QueryRow("SELECT status,stage FROM events WHERE id=?", p.EventID).Scan(&status, &stage); e != nil {
		return e
	}
	if status != "uncertain" && status != "interrupted" {
		return errors.New("请求状态已变化，不使用旧观察结果")
	}
	if stage != "waiting_model" && stage != "submission_unconfirmed" && stage != "execution_unconfirmed" && stage != "outcome_unconfirmed" && stage != "reply_delivery_unconfirmed" {
		return errors.New("请求阶段不允许恢复确认")
	}
	if len(entries) > 0 {
		var evidence struct {
			Outcome     string   `json:"outcome"`
			MatchedTurn string   `json:"matched_turn"`
			UserItemID  string   `json:"user_item_id"`
			ReplyIssue  string   `json:"reply_issue"`
			ReplyIDs    []string `json:"reply_ids"`
		}
		if json.Unmarshal([]byte(p.State), &evidence) != nil || evidence.Outcome != "completed" ||
			evidence.MatchedTurn != actualTurn || evidence.UserItemID != p.UserItemID || evidence.ReplyIssue != "" ||
			len(entries) != len(ids) || len(evidence.ReplyIDs) != len(ids) {
			return errors.New("重读回复缺少完整观察证据")
		}
		var current int
		if e = tx.QueryRow(`SELECT count(*) FROM request_results
WHERE event_id = ? AND thread_id = ? AND address_key = ? AND turn_ref = ?
  AND observer_path = ? AND observer_offset = ? AND observer_state = ? AND sealed = 0`,
			p.EventID, p.ThreadID, p.AddressKey, p.TurnRef, p.Path, p.Offset, p.State).Scan(&current); e != nil {
			return e
		}
		if current != 1 {
			return errors.New("重读回复的观察版本已过期")
		}
		for index, entry := range entries {
			if entry.ItemID != ids[index] || entry.ItemID != evidence.ReplyIDs[index] || entry.Role != "assistant" {
				return errors.New("重读回复的消息清单不一致")
			}
			if e = prepareMirrorEntry(tx, session.ThreadID, "", entry); e != nil {
				return e
			}
		}
	}
	if e = sealRequestReplyIDs(tx, p.EventID, p.ThreadID, ids, "只读日志确认轮次 "+actualTurn); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE events SET status='uncertain',stage='reply_delivery_unconfirmed',error_summary='只读观察已确认执行完成，正在核对对应回复的送达记录',updated_at=? WHERE id=?", time.Now().Unix(), p.EventID); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) FailObservedRequest(session Session, p RequestProbe, actualTurn string) error {
	if actualTurn == "" {
		return errors.New("恢复观察缺少实际轮次标识")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = ensureRequestResult(tx, session, p.EventID, p.TurnRef); e != nil {
		return e
	}
	var sealed int
	var execution string
	if e = tx.QueryRow("SELECT sealed,execution_state FROM request_results WHERE event_id=?", p.EventID).Scan(&sealed, &execution); e != nil {
		return e
	}
	if sealed != 0 || execution == "completed" {
		return errors.New("已有完成证据，不覆盖为中断")
	}
	var user string
	if e = tx.QueryRow("SELECT user_item_id FROM live_mirror_origins WHERE event_id=? AND thread_id=? AND state='matched'", p.EventID, p.ThreadID).Scan(&user); e != nil {
		return e
	}
	if user != p.UserItemID {
		return errors.New("恢复观察的用户消息关联已变化")
	}
	var status, stage, detail string
	if e = tx.QueryRow("SELECT status,stage,error_summary FROM events WHERE id=?", p.EventID).Scan(&status, &stage, &detail); e != nil {
		return e
	}
	if status != "uncertain" && status != "interrupted" {
		return errors.New("请求状态已变化")
	}
	if stage != "waiting_model" && stage != "submission_unconfirmed" && stage != "execution_unconfirmed" && stage != "outcome_unconfirmed" {
		return errors.New("没有可以改为执行中断的请求阶段")
	}
	if e = auditRequest(tx, p.EventID, "execution_aborted", status, stage, "只读日志确认轮次 "+actualTurn+" 已中断；原记录："+detail); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE request_results SET execution_state='failed',delivery_state='not_applicable',observation_error='',updated_at=? WHERE event_id=?", time.Now().Unix(), p.EventID); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE events SET status='failed',stage='execution_failed',error_summary='只读日志已确认该轮次中断；可能已有部分结果，不会自动重跑',updated_at=? WHERE id=?", time.Now().Unix(), p.EventID); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE image_drafts SET state='failed' WHERE trigger_id=? AND state IN ('submitted','interrupted','uncertain')", p.EventID); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE creation_forms SET state='failed',error='只读观察确认执行中断，未重新运行',updated_at=? WHERE id || ':first'=? AND thread_id=? AND stage IN ('first_question','submitting_first_question')", time.Now().Unix(), p.EventID, session.ThreadID); e != nil {
		return e
	}
	return tx.Commit()
}

func requestResultSnapshot(db *sql.DB, event string) (*RequestResult, error) {
	var r RequestResult
	e := db.QueryRow(`SELECT event_id,turn_ref,execution_state,delivery_state,expected_replies,completed_at,delivered_at,recovered,observation_error
FROM request_results WHERE event_id = ?`, event).Scan(&r.EventID, &r.TurnRef, &r.Execution, &r.Delivery, &r.ExpectedReplies, &r.CompletedAt, &r.DeliveredAt, &r.Recovered, &r.ObservationError)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	e = db.QueryRow(`SELECT coalesce(sum(m.expected_parts),0),
       coalesce(sum((SELECT count(*) FROM live_mirror_parts p WHERE p.thread_id = m.thread_id AND p.item_id = m.item_id AND p.state = 'sent')),0)
FROM request_result_replies m WHERE m.event_id = ?`, event).Scan(&r.ExpectedParts, &r.ConfirmedParts)
	if e != nil {
		return nil, e
	}
	rows, e := db.Query("SELECT reason,previous_status,previous_stage,detail,created_at FROM request_result_audit WHERE event_id=? ORDER BY seq DESC LIMIT 5", event)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var a RequestAudit
		if e = rows.Scan(&a.Reason, &a.PreviousStatus, &a.PreviousStage, &a.Detail, &a.At); e != nil {
			return nil, e
		}
		r.Audit = append(r.Audit, a)
	}
	return &r, rows.Err()
}
func (s *Store) RequestResult(event string) (*RequestResult, error) {
	return requestResultSnapshot(s.db, event)
}
