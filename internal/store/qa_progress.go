package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"im-agent/internal/im"
)

// QAProgress 保存一轮执行的进度；LastEventAt 只由真实模型事件更新。
type QAProgress struct {
	ExecutionState string `json:"execution_state"`
	ExecutionError string `json:"execution_error"`
	EventID        string `json:"event_id"`
	QuestionID     string `json:"question_id"`
	Connection     string `json:"connection"`
	ChatID         string `json:"chat_id"`
	Stage          string `json:"stage"`
	Detail         string `json:"detail"`
	Kind           string `json:"kind"`
	Workflow       string `json:"workflow"`
	UpdatedAt      int64  `json:"updated_at"`
	LastEventAt    int64  `json:"last_event_at"`
	CreatedAt      int64  `json:"created_at"`
}

// QANotice 是独立于最终回答的通知出站记录。Version 用于拒绝过期发送结果。
type QANotice struct {
	Kind       string `json:"kind"`
	Stage      string `json:"stage"`
	ID         string `json:"id"`
	EventID    string `json:"event_id"`
	QuestionID string `json:"question_id"`
	Connection string `json:"connection"`
	ChatID     string `json:"chat_id"`
	Body       string `json:"body"`
	State      string `json:"state"`
	Version    int64  `json:"version"`
}

func (s *Store) migrateQAProgress() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS qa_workflows(
 question_id TEXT PRIMARY KEY REFERENCES qa_questions(id),kind TEXT NOT NULL DEFAULT 'question',
 phase TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS qa_progress(
 event_id TEXT PRIMARY KEY REFERENCES qa_turns(id),stage TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL,last_event_at INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,
 version INTEGER NOT NULL DEFAULT 1);
 CREATE TABLE IF NOT EXISTS qa_notices(
 id TEXT PRIMARY KEY,event_id TEXT REFERENCES qa_turns(id),question_id TEXT REFERENCES qa_questions(id),
 connection_id TEXT NOT NULL,chat_id TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',
 version INTEGER NOT NULL DEFAULT 1,platform_id TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS qa_notices_pending ON qa_notices(state,created_at,id);
 INSERT OR IGNORE INTO qa_workflows(question_id,kind,phase,updated_at)
 SELECT id,'question','',created_at FROM qa_questions;
 INSERT OR IGNORE INTO qa_progress(event_id,stage,detail,updated_at,last_event_at,created_at)
 SELECT t.id,CASE
 WHEN EXISTS(SELECT 1 FROM qa_deliveries d WHERE d.event_id=t.id AND d.state='unknown') THEN 'delivery_unknown'
 WHEN EXISTS(SELECT 1 FROM qa_deliveries d WHERE d.event_id=t.id AND d.state='failed') THEN 'delivery_failed'
 WHEN EXISTS(SELECT 1 FROM qa_deliveries d WHERE d.event_id=t.id)
  AND NOT EXISTS(SELECT 1 FROM qa_deliveries d WHERE d.event_id=t.id AND d.state<>'sent') THEN 'sent'
 WHEN t.state='completed' THEN 'sending'
 WHEN t.state IN ('starting','running') THEN 'processing'
 ELSE t.state END,t.error,t.created_at,0,t.created_at FROM qa_turns t;`)
	if err != nil {
		return err
	}
	rows, e := tx.Query("PRAGMA table_info(qa_notices)")
	if e != nil {
		return e
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if e = rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); e != nil {
			rows.Close()
			return e
		}
		columns[name] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for name, definition := range map[string]string{"kind": "TEXT NOT NULL DEFAULT 'stage'", "target_stage": "TEXT NOT NULL DEFAULT ''"} {
		if !columns[name] {
			if _, e = tx.Exec("ALTER TABLE qa_notices ADD COLUMN " + name + " " + definition); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}

const qaProgressColumns = "t.state,t.error,p.event_id,t.question_id,q.connection_id,q.chat_id,p.stage,p.detail,COALESCE(w.kind,'question'),COALESCE(w.phase,''),p.updated_at,p.last_event_at,p.created_at"
const qaProgressJoins = " FROM qa_progress p JOIN qa_turns t ON t.id=p.event_id JOIN qa_questions q ON q.id=t.question_id LEFT JOIN qa_workflows w ON w.question_id=t.question_id"

func scanQAProgress(row interface{ Scan(...any) error }) (QAProgress, error) {
	var p QAProgress
	err := row.Scan(&p.ExecutionState, &p.ExecutionError, &p.EventID, &p.QuestionID, &p.Connection, &p.ChatID, &p.Stage, &p.Detail, &p.Kind, &p.Workflow, &p.UpdatedAt, &p.LastEventAt, &p.CreatedAt)
	return p, err
}

func (s *Store) QAProgressForEvent(event string) (QAProgress, error) {
	return scanQAProgress(s.db.QueryRow("SELECT "+qaProgressColumns+qaProgressJoins+" WHERE p.event_id=?", event))
}

func (s *Store) QALatestProgress(qid string) (QAProgress, error) {
	return scanQAProgress(s.db.QueryRow("SELECT "+qaProgressColumns+qaProgressJoins+" WHERE t.question_id=? ORDER BY t.seq DESC LIMIT 1", qid))
}

var qaStageLabels = map[string]string{
	"received":         "已接收",
	"queued":           "已接收，正在排队",
	"classifying":      "正在识别问题类型",
	"processing":       "正在处理",
	"generating":       "正在生成回答",
	"using_tool":       "正在查询业务信息",
	"waiting_approval": "等待本机确认",
	"waiting_input":    "等待补充信息",
	"documenting":      "正在整理文档",
	"saving_document":  "正在保存文档",
	"sending":          "回答已生成，正在发送",
	"sent":             "本轮回复已发送",
	"rejected":         "本轮请求未执行",
	"failed":           "本轮处理失败",
	"unknown":          "执行结果未确认，请在本机核对；未自动重发",
	"delivery_failed":  "回复发送失败，请在本机处理",
	"delivery_unknown": "回复发送结果未确认，请先核对群消息；未自动重发",
}

func (s *Store) QASetProgress(event, stage, detail string, notify bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = qaSetProgressTx(tx, event, stage, detail, notify); err != nil {
		return err
	}
	return tx.Commit()
}

// qaSetProgressTx 将进度与阶段通知一起提交，不把调度、心跳当成模型事件。
func qaSetProgressTx(tx *sql.Tx, event, stage, detail string, notify bool) error {
	if _, ok := qaStageLabels[stage]; !ok {
		return errors.New("问答进度阶段无效")
	}
	var qid, connection, chat, input string
	var created int64
	if err := tx.QueryRow(`SELECT t.question_id,q.connection_id,q.chat_id,t.input,t.created_at
 FROM qa_turns t JOIN qa_questions q ON q.id=t.question_id WHERE t.id=?`, event).Scan(&qid, &connection, &chat, &input, &created); err != nil {
		return err
	}
	var previous string
	err := tx.QueryRow("SELECT stage FROM qa_progress WHERE event_id=?", event).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := time.Now().Unix()
	if _, err = tx.Exec(`INSERT INTO qa_progress(event_id,stage,detail,updated_at,last_event_at,created_at)
 VALUES(?,?,?,?,0,?) ON CONFLICT(event_id) DO UPDATE SET stage=excluded.stage,detail=excluded.detail,
 updated_at=excluded.updated_at,version=qa_progress.version+1
 WHERE qa_progress.stage<>excluded.stage OR qa_progress.detail<>excluded.detail`, event, stage, detail, now, created); err != nil {
		return err
	}
	if !notify || previous == stage {
		return nil
	}
	var version int64
	if err = tx.QueryRow("SELECT version FROM qa_progress WHERE event_id=?", event).Scan(&version); err != nil {
		return err
	}
	// 阶段回执仅包含用户问题摘要和固定说明，绝不拼接模型输出或工具详情。
	summary := []rune(strings.Join(strings.Fields(input), " "))
	if len(summary) > 48 {
		summary = append(summary[:48], '…')
	}
	if len(summary) == 0 {
		summary = []rune("未填写问题内容")
	}
	summary = []rune(strings.NewReplacer("@", "＠", "*", "＊", "_", "＿", "[", "［", "]", "］", "`", "｀", "<", "＜", ">", "＞").Replace(string(summary)))
	short := event
	if len(short) > 8 {
		short = short[:8]
	}
	kind, label := "stage", qaStageLabels[stage]
	if stage == "queued" {
		kind = "receipt"
		label = "已收到并记录这条问题"
	}
	body := fmt.Sprintf("问题「%s」 [%s]\n阶段记录（%s）：%s\n引用本回执发送 /status 或“进度”可查看对应轮次进度。", string(summary), short, time.Unix(now, 0).Format("2006-01-02 15:04:05 -07:00"), label)
	return qaQueueNoticeTx(tx, QANotice{ID: Hash("qa:stage:" + event + ":" + stage), EventID: event, QuestionID: qid, Connection: connection, ChatID: chat, Body: body, Version: version, Kind: kind, Stage: stage})
}

// QATouchProgress 只能在消费真实模型事件时调用，不能用于轮询或心跳。
func (s *Store) QATouchProgress(event string) error {
	now := time.Now().Unix()
	r, err := s.db.Exec("UPDATE qa_progress SET last_event_at=MAX(last_event_at,?),updated_at=MAX(updated_at,?) WHERE event_id=?", now, now, event)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) QASetWorkflow(qid, kind, phase string) error {
	if strings.TrimSpace(kind) == "" {
		return errors.New("问答类型不能为空")
	}
	r, err := s.db.Exec(`INSERT INTO qa_workflows(question_id,kind,phase,updated_at)
 SELECT id,?,?,? FROM qa_questions WHERE id=? ON CONFLICT(question_id) DO UPDATE
 SET kind=excluded.kind,phase=excluded.phase,updated_at=excluded.updated_at`, kind, phase, time.Now().Unix(), qid)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) QAGetWorkflow(qid string) (kind, phase string, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(w.kind,'question'),COALESCE(w.phase,'')
 FROM qa_questions q LEFT JOIN qa_workflows w ON w.question_id=q.id WHERE q.id=?`, qid).Scan(&kind, &phase)
	return
}

var (
	errQAReplyForeign   = errors.New("引用属于其他群或钉钉连接，未执行")
	errQAReplyAmbiguous = errors.New("引用对应多个不同问答或轮次，未执行")
)

type qaReplyQuerier interface {
	QueryRow(string, ...any) *sql.Row
}

// QAResolveReply 仅使用平台引用精确匹配；同一问题的不同轮次也是不同目标。
func (s *Store) QAResolveReply(address im.Address, refs []string) (questionID, eventID string, err error) {
	return qaResolveReply(s.db, address, refs)
}

func qaResolveReply(db qaReplyQuerier, address im.Address, refs []string) (string, string, error) {
	if address.Channel != "dingtalk" || address.Connection == "" || address.ChatID == "" {
		return "", "", errors.New("问答引用地址无效")
	}
	var qid, event string
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		var foundQID, foundEvent string
		err := db.QueryRow(`SELECT r.question_id,r.event_id FROM qa_reply_refs r
 JOIN qa_questions q ON q.id=r.question_id JOIN qa_turns t ON t.id=r.event_id AND t.question_id=q.id
 WHERE r.connection_id=? AND r.chat_id=? AND r.reference=? AND q.connection_id=r.connection_id AND q.chat_id=r.chat_id`, address.Connection, address.ChatID, ref).Scan(&foundQID, &foundEvent)
		if errors.Is(err, sql.ErrNoRows) {
			var foreign int
			if err = db.QueryRow("SELECT count(*) FROM qa_reply_refs WHERE reference=? AND (connection_id<>? OR chat_id<>?)", ref, address.Connection, address.ChatID).Scan(&foreign); err != nil {
				return "", "", err
			}
			if foreign > 0 {
				return "", "", errQAReplyForeign
			}
			continue
		}
		if err != nil {
			return "", "", err
		}
		if qid != "" && (qid != foundQID || event != foundEvent) {
			return "", "", errQAReplyAmbiguous
		}
		qid, event = foundQID, foundEvent
	}
	if qid == "" {
		return "", "", sql.ErrNoRows
	}
	return qid, event, nil
}

// QAQueueNotice 的幂等键仅取入站消息身份，重复状态查询回调不会重复通知。
// progress 为空时仅通知，不创建任何问答引用。
func (s *Store) QAQueueNotice(msg im.Message, progress *QAProgress, body string) error {
	if msg.Address.Channel != "dingtalk" || msg.Address.Connection == "" || msg.Address.ChatID == "" || msg.ID == "" || strings.TrimSpace(body) == "" {
		return errors.New("问答通知消息无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	n := QANotice{ID: Hash("qa:query:" + msg.Address.BaseKey() + "\x00" + msg.ID), Connection: msg.Address.Connection, ChatID: msg.Address.ChatID, Body: body, Version: 1}
	n.Kind = "query"
	var exists int
	if err = tx.QueryRow("SELECT count(*) FROM qa_notices WHERE id=?", n.ID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return tx.Commit()
	}
	if strings.HasPrefix(msg.ID, "wait:") {
		n.Kind = "wait"
		if progress != nil {
			n.Stage = progress.Stage
		}
	}

	if progress != nil {
		var p QAProgress
		p, err = scanQAProgress(tx.QueryRow("SELECT "+qaProgressColumns+qaProgressJoins+" WHERE p.event_id=?", progress.EventID))
		if err != nil {
			return err
		}
		if p.QuestionID != progress.QuestionID || p.Connection != msg.Address.Connection || p.ChatID != msg.Address.ChatID ||
			(progress.Connection != "" && progress.Connection != p.Connection) || (progress.ChatID != "" && progress.ChatID != p.ChatID) {
			return errors.New("问答通知引用与当前群或连接不匹配")
		}
		n.QuestionID, n.EventID = p.QuestionID, p.EventID
	}
	if err = qaQueueNoticeTx(tx, n); err != nil {
		return err
	}
	return tx.Commit()
}

func qaQueueNoticeTx(tx *sql.Tx, n QANotice) error {
	now := time.Now().Unix()
	_, err := tx.Exec(`INSERT INTO qa_notices(id,event_id,question_id,connection_id,chat_id,body,version,kind,target_stage,created_at,updated_at)
 VALUES(?,NULLIF(?,''),NULLIF(?,''),?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, n.ID, n.EventID, n.QuestionID, n.Connection, n.ChatID, n.Body, n.Version, n.Kind, n.Stage, now, now)
	if err != nil {
		return err
	}
	var event, qid, connection, chat string
	if err = tx.QueryRow("SELECT COALESCE(event_id,''),COALESCE(question_id,''),connection_id,chat_id FROM qa_notices WHERE id=?", n.ID).Scan(&event, &qid, &connection, &chat); err != nil {
		return err
	}
	if event != n.EventID || qid != n.QuestionID || connection != n.Connection || chat != n.ChatID {
		return errors.New("同一通知标识已关联不同目标")
	}
	return nil
}

func (s *Store) QAPendingNotices(limit int) ([]QANotice, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,COALESCE(event_id,''),COALESCE(question_id,''),connection_id,chat_id,body,state,version,kind,target_stage
 FROM qa_notices WHERE state='pending' ORDER BY created_at,rowid LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QANotice{}
	for rows.Next() {
		var n QANotice
		if err = rows.Scan(&n.ID, &n.EventID, &n.QuestionID, &n.Connection, &n.ChatID, &n.Body, &n.State, &n.Version, &n.Kind, &n.Stage); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) QAClaimNotice(id string) (bool, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	var kind, target, current, execution string
	e = tx.QueryRow(`SELECT n.kind,n.target_stage,COALESCE(p.stage,''),COALESCE(t.state,'')
 FROM qa_notices n LEFT JOIN qa_progress p ON p.event_id=n.event_id LEFT JOIN qa_turns t ON t.id=n.event_id
 WHERE n.id=? AND n.state='pending'`, id).Scan(&kind, &target, &current, &execution)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if (kind == "stage" || kind == "wait") && (target == "" || target != current || (execution != "starting" && execution != "running" && execution != "queued")) {
		if _, e = tx.Exec("UPDATE qa_notices SET state='cancelled',error='阶段已变化，跳过过期通知',updated_at=? WHERE id=? AND state='pending'", time.Now().Unix(), id); e != nil {
			return false, e
		}
		return false, tx.Commit()
	}
	result, e := tx.Exec("UPDATE qa_notices SET state='sending',updated_at=? WHERE id=? AND state='pending'", time.Now().Unix(), id)
	if e != nil {
		return false, e
	}
	count, e := result.RowsAffected()
	if e != nil {
		return false, e
	}
	if e = tx.Commit(); e != nil {
		return false, e
	}
	return count == 1, nil
}
func (s *Store) QANoticeResult(notice QANotice, state, platformID, detail string) error {
	if state != "sent" && state != "failed" && state != "unknown" {
		return errors.New("问答通知发送状态无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var qid, event, connection, chat string
	if err = tx.QueryRow(`SELECT COALESCE(question_id,''),COALESCE(event_id,''),connection_id,chat_id
 FROM qa_notices WHERE id=? AND version=? AND state='sending'`, notice.ID, notice.Version).Scan(&qid, &event, &connection, &chat); err != nil {
		return err
	}
	if qid != notice.QuestionID || event != notice.EventID || connection != notice.Connection || chat != notice.ChatID {
		return errors.New("问答通知发送目标已变化")
	}
	if _, err = tx.Exec("UPDATE qa_notices SET state=?,platform_id=?,error=?,updated_at=? WHERE id=? AND version=? AND state='sending'", state, platformID, detail, time.Now().Unix(), notice.ID, notice.Version); err != nil {
		return err
	}
	if state == "sent" && platformID != "" && event != "" && qid != "" {
		if err = qaPutReplyRefTx(tx, connection, chat, platformID, qid, event); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func qaPutReplyRefTx(tx *sql.Tx, connection, chat, ref, qid, event string) error {
	_, err := tx.Exec(`INSERT INTO qa_reply_refs(connection_id,chat_id,reference,question_id,event_id)
 VALUES(?,?,?,?,?) ON CONFLICT(connection_id,chat_id,reference) DO NOTHING`, connection, chat, ref, qid, event)
	if err != nil {
		return err
	}
	var existingQID, existingEvent string
	if err = tx.QueryRow("SELECT question_id,event_id FROM qa_reply_refs WHERE connection_id=? AND chat_id=? AND reference=?", connection, chat, ref).Scan(&existingQID, &existingEvent); err != nil {
		return err
	}
	if existingQID != qid || existingEvent != event {
		return errors.New("平台回执标识已关联其他问答或轮次")
	}
	return nil
}

// qaDeliveryProgressTx 统一汇总所有片段，避免较早片段成功掩盖后续发送错误。
func qaDeliveryProgressTx(tx *sql.Tx, event string) error {
	var total, sent, failed, unknown int
	if err := tx.QueryRow(`SELECT count(*),COALESCE(sum(state='sent'),0),COALESCE(sum(state='failed'),0),
 COALESCE(sum(state='unknown'),0) FROM qa_deliveries WHERE event_id=?`, event).Scan(&total, &sent, &failed, &unknown); err != nil {
		return err
	}
	stage, detail := "sending", ""
	switch {
	case unknown > 0:
		stage, detail = "delivery_unknown", "发送结果未确认；未自动重发，请先核对群消息"
	case failed > 0:
		stage, detail = "delivery_failed", "回复发送失败；请在本机处理"
	case total > 0 && sent == total:
		stage = "sent"
	}
	return qaSetProgressTx(tx, event, stage, detail, false)
}
