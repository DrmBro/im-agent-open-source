package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"im-agent/internal/telemetry"
)

type SessionStatus struct {
	Session   Session
	Name      string
	Task      TaskRow
	Queued    int
	Result    *RequestResult
	Telemetry *telemetry.Report
}
type SessionStatusCard struct {
	ThreadID, RequestID, AddressKey, MessageID, State, Digest, EventID, Error string
	Attempts                                                                  int
	UpdatedAt, NextAttempt                                                    int64
}

// 事务迁移只涉及展示表，不改写会话或模型事件。旧卡保留原消息ID。
func (s *Store) migrateSessionStatus() error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.Exec(`CREATE TABLE IF NOT EXISTS session_status_cards (
 thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id),address_key TEXT NOT NULL,
 message_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,digest TEXT NOT NULL DEFAULT '',event_id TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL
 )`)
	if e != nil {
		return e
	}
	rows, e := tx.Query("PRAGMA table_info(session_status_cards)")
	if e != nil {
		return e
	}
	migrated := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if e = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); e != nil {
			rows.Close()
			return e
		}
		if name == "request_id" {
			migrated = true
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if !migrated {
		for _, q := range []string{
			`CREATE TABLE session_status_cards_by_request (
    thread_id TEXT NOT NULL REFERENCES sessions(thread_id),request_id TEXT NOT NULL,
    address_key TEXT NOT NULL,message_id TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,
    digest TEXT NOT NULL DEFAULT '',event_id TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL,
    observed_version TEXT NOT NULL DEFAULT '',PRIMARY KEY(thread_id,request_id))`,
			`INSERT INTO session_status_cards_by_request
    (thread_id,request_id,address_key,message_id,state,digest,event_id,error,attempts,next_attempt,updated_at)
    SELECT thread_id,event_id,address_key,message_id,state,digest,event_id,error,attempts,next_attempt,updated_at
    FROM session_status_cards`,
			`DROP TABLE session_status_cards`,
			`ALTER TABLE session_status_cards_by_request RENAME TO session_status_cards`,
		} {
			if _, e = tx.Exec(q); e != nil {
				return e
			}
		}
	}
	// 首次升级只自动展示升级后的新请求，旧卡仍跟随它原先关联的请求。
	for _, key := range []string{"session-status-enabled-at", "request-status-enabled-at"} {
		if _, e = tx.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING", key, strconv.FormatInt(time.Now().Unix(), 10)); e != nil {
			return e
		}
	}
	_, e = tx.Exec("UPDATE session_status_cards SET state='unknown',error='上次状态卡发送中断，送达结果待核对；不会重复发卡或重放任务',updated_at=? WHERE state='creating'", time.Now().Unix())
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) StatusCardsEnabledAt() (int64, error) {
	raw, e := s.GetMeta("request-status-enabled-at")
	if e != nil {
		return 0, e
	}
	return strconv.ParseInt(raw, 10, 64)
}

// events.id是主键，chat_key是既有绑定地址散列。只读取模型请求的已知阶段，命令不覆盖任务。
const statusRequestStages = "('queued','starting','resuming','downloading_attachment','submitting','waiting_desktop_submit','checking_desktop','preserving_draft','restoring_draft','waiting_draft','waiting_desktop','switching_desktop','waiting_model','completed','outcome_unconfirmed','queue_full','waiting_approval','waiting_editor','waiting_dialog','waiting_desktop_approval','approval_unconfirmed','delivering_reply','binding_failed','not_sent','submission_unconfirmed','execution_unconfirmed','execution_failed','reply_delivery_unconfirmed')"

func (s *Store) ReadSessionStatus(thread string) (SessionStatus, error) {
	var out SessionStatus
	session, e := s.ByThread(thread)
	if e != nil {
		return out, e
	}
	out.Session = session
	p, e := s.Presentation(thread)
	if e == nil {
		out.Name = p.Name
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	key := Hash(session.Address.Key())
	if e = s.db.QueryRow("SELECT count(*) FROM events WHERE chat_key=? AND status='processing' AND stage='queued'", key).Scan(&out.Queued); e != nil {
		return out, e
	}
	row := s.db.QueryRow(`SELECT id,status,stage,error_summary,created_at,updated_at FROM events
 WHERE chat_key=? AND stage IN `+statusRequestStages+`
 ORDER BY CASE WHEN status='processing' AND stage<>'queued' THEN 0 WHEN status='processing' THEN 1 ELSE 2 END,
 created_at DESC,rowid DESC LIMIT 1`, key)
	e = row.Scan(&out.Task.ID, &out.Task.Status, &out.Task.Stage, &out.Task.Error, &out.Task.ReceivedAt, &out.Task.UpdatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	out.Task.ThreadID = thread
	out.Task.Name = out.Name
	out.Result, e = s.RequestResult(out.Task.ID)
	out.Telemetry, _ = s.RequestTelemetry(out.Task.ID, thread, key)
	return out, e
}

// 请求版本来自实际读到的事件快照，不在发送成功后重新读取，避免吞掉并发状态变化。
func (v SessionStatus) RequestVersion() string {
	version := fmt.Sprintf("%s/%s/%d/%s", v.Task.Status, v.Task.Stage, v.Task.UpdatedAt, v.Task.Error)
	if v.Result == nil {
		return version + "/none"
	}
	return version + fmt.Sprintf("/%s/%s/%d/%d", v.Result.Execution, v.Result.Delivery, v.Result.CompletedAt, v.Result.DeliveredAt)
}

func (s *Store) ReadSessionRequestStatus(thread, request string) (SessionStatus, error) {
	view, e := s.ReadSessionStatus(thread)
	if e != nil {
		return view, e
	}
	view.Task = TaskRow{ThreadID: thread, Name: view.Name}
	view.Telemetry = nil
	e = s.db.QueryRow(`SELECT id, status, stage, error_summary, created_at, updated_at
FROM events
WHERE id = ? AND chat_key = ?
  AND stage IN `+statusRequestStages, request, Hash(view.Session.Address.Key())).Scan(
		&view.Task.ID, &view.Task.Status, &view.Task.Stage, &view.Task.Error, &view.Task.ReceivedAt, &view.Task.UpdatedAt)
	if e == nil {
		view.Result, e = s.RequestResult(view.Task.ID)
		view.Telemetry, _ = s.RequestTelemetry(view.Task.ID, thread, Hash(view.Session.Address.Key()))
	}
	return view, e
}

// 与RequestVersion一致，结果记录变化也刷新卡片，不依赖秒级事件时间变化。
const statusRequestVersionSQL = "e.status || '/' || e.stage || '/' || e.updated_at || '/' || e.error_summary || CASE WHEN r.event_id IS NULL THEN '/none' ELSE '/' || r.execution_state || '/' || r.delivery_state || '/' || r.completed_at || '/' || r.delivered_at END"

// 每条events.id是独立请求。包含两次扫描之间已完成的短请求，不只挑当前最新请求。
func (s *Store) PendingSessionStatusRequests(session Session) ([]string, error) {
	enabled, e := s.StatusCardsEnabledAt()
	if e != nil {
		return nil, e
	}
	rows, e := s.db.Query(`SELECT e.id
FROM events e
         LEFT JOIN session_status_cards c ON c.thread_id = ? AND c.request_id = e.id
         LEFT JOIN request_results r ON r.event_id = e.id
WHERE e.chat_key = ?
  AND e.stage IN `+statusRequestStages+`
  AND (c.request_id IS NOT NULL OR e.created_at >= ?)
  AND (c.request_id IS NULL
       OR (c.state NOT IN ('creating', 'unknown') AND c.next_attempt <= ?
           AND (c.error <> '' OR e.status = 'processing' OR r.delivery_state = 'pending'
                OR c.observed_version <> (`+statusRequestVersionSQL+`))))
ORDER BY CASE
             WHEN c.request_id IS NULL
                  OR c.observed_version <> (`+statusRequestVersionSQL+`) THEN 0
             ELSE 1 END,
         e.created_at, e.rowid
LIMIT 32`, session.ThreadID, Hash(session.Address.Key()), enabled, time.Now().Unix())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// 无request参数时仅供旧调用方查看最近更新的卡；展示链路始终显式传请求ID。
func (s *Store) StatusCard(thread string, request ...string) (SessionStatusCard, error) {
	var c SessionStatusCard
	q := "SELECT thread_id,request_id,address_key,message_id,state,digest,event_id,error,attempts,updated_at,next_attempt FROM session_status_cards WHERE thread_id=?"
	args := []any{thread}
	if len(request) > 0 {
		q += " AND request_id=?"
		args = append(args, request[0])
	}
	q += " ORDER BY updated_at DESC,rowid DESC LIMIT 1"
	e := s.db.QueryRow(q, args...).Scan(&c.ThreadID, &c.RequestID, &c.AddressKey, &c.MessageID, &c.State, &c.Digest, &c.EventID, &c.Error, &c.Attempts, &c.UpdatedAt, &c.NextAttempt)
	return c, e
}
func statusRequestID(request []string) string {
	if len(request) > 0 {
		return request[0]
	}
	return ""
}

// 原子预留每请求的展示消息，不创建/领取任何模型事件。
func (s *Store) ReserveStatusCard(session Session, request ...string) (bool, error) {
	r, e := s.db.Exec(`INSERT INTO session_status_cards(thread_id,request_id,address_key,event_id,state,updated_at)
 SELECT ?,?,?,?,'creating',? WHERE EXISTS (SELECT 1 FROM sessions WHERE thread_id=? AND status='active' AND managed_chat=1)
 ON CONFLICT(thread_id,request_id) DO UPDATE SET state='creating',updated_at=excluded.updated_at
 WHERE session_status_cards.state='failed' AND session_status_cards.message_id='' AND session_status_cards.next_attempt<=?
 AND session_status_cards.address_key=excluded.address_key`, session.ThreadID, statusRequestID(request), Hash(session.Address.Key()), statusRequestID(request), time.Now().Unix(), session.ThreadID, time.Now().Unix())
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) StatusCardSent(thread, message, digest, event string, request ...string) error {
	if message == "" {
		return errors.New("状态卡缺少消息ID")
	}
	r, e := s.db.Exec("UPDATE session_status_cards SET message_id=?,state='sent',digest=?,event_id=?,error='',attempts=0,next_attempt=0,updated_at=? WHERE thread_id=? AND request_id=? AND state='creating'", message, digest, event, time.Now().Unix(), thread, statusRequestID(request))
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return errors.New("状态卡创建状态已变化")
	}
	return nil
}
func (s *Store) StatusCardUpdated(thread, message, digest, event string, request ...string) error {
	r, e := s.db.Exec("UPDATE session_status_cards SET state='sent',digest=?,event_id=?,error='',attempts=0,next_attempt=0,updated_at=? WHERE thread_id=? AND request_id=? AND message_id=? AND message_id<>''", digest, event, time.Now().Unix(), thread, statusRequestID(request), message)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return errors.New("状态卡消息ID已变化")
	}
	return nil
}
func (s *Store) StatusCardObserved(thread, request, version string) error {
	r, e := s.db.Exec("UPDATE session_status_cards SET observed_version=? WHERE thread_id=? AND request_id=? AND state='sent'", version, thread, request)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("状态卡展示记录已变化")
	}
	return nil
}
func (s *Store) StatusCardFailed(thread, hint string, unknown bool, request ...string) error {
	card, e := s.StatusCard(thread, request...)
	if e != nil {
		return e
	}
	state := "failed"
	if card.MessageID != "" {
		state = "sent"
	} else if unknown {
		state = "unknown"
	}
	delay := int64(15) << min(card.Attempts, 5)
	_, e = s.db.Exec("UPDATE session_status_cards SET state=?,error=?,attempts=attempts+1,next_attempt=?,updated_at=? WHERE thread_id=? AND request_id=?", state, hint, time.Now().Unix()+delay, time.Now().Unix(), thread, card.RequestID)
	return e
}

// 每个会话+请求仅自动尝试一次卡片异常提醒，不把提醒失败写入模型任务状态。
// 提醒送达不确定也不重发；显式/status的反馈不受此记录限制。
func (s *Store) ReserveStatusCardFailureNotice(thread, request string) (bool, error) {
	r, e := s.db.Exec(`INSERT INTO meta(key, value)
SELECT ?, ?
WHERE EXISTS (SELECT 1 FROM sessions WHERE thread_id = ? AND status = 'active' AND managed_chat = 1)
ON CONFLICT(key) DO NOTHING`, "status-card-failure-notice:"+Hash(thread+"\x00"+request), strconv.FormatInt(time.Now().Unix(), 10), thread)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
