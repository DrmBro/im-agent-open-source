package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"im-agent/internal/im"
)

type QAQuestion struct {
	ID         string `json:"id"`
	Connection string `json:"connection"`
	ChatID     string `json:"chat_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"created_at"`
}
type QATurn struct {
	ID         string      `json:"id"`
	QuestionID string      `json:"question_id"`
	Seq        int64       `json:"seq"`
	Connection string      `json:"connection"`
	ChatID     string      `json:"chat_id"`
	UserID     string      `json:"user_id"`
	SenderName string      `json:"sender_name"`
	Text       string      `json:"text"`
	State      string      `json:"state"`
	ThreadID   string      `json:"thread_id"`
	TurnID     string      `json:"turn_id"`
	Answer     string      `json:"answer"`
	Error      string      `json:"error"`
	CreatedAt  int64       `json:"created_at"`
	Progress   *QAProgress `json:"progress,omitempty"`
}
type QADelivery struct {
	ID         string `json:"id"`
	QuestionID string `json:"question_id"`
	EventID    string `json:"event_id"`
	Connection string `json:"connection"`
	ChatID     string `json:"chat_id"`
	Body       string `json:"-"`
	State      string `json:"state"`
	Error      string `json:"error"`
	Part       int    `json:"part"`
}
type QARequest struct {
	Question   string          `json:"question"`
	SenderName string          `json:"sender_name"`
	Connection string          `json:"connection"`
	ID         string          `json:"id"`
	QuestionID string          `json:"question_id"`
	EventID    string          `json:"event_id"`
	Method     string          `json:"method"`
	Detail     string          `json:"detail"`
	Payload    json.RawMessage `json:"payload"`
	State      string          `json:"state"`
	CreatedAt  int64           `json:"created_at"`
}
type QAView struct {
	Turns      []QATurn     `json:"turns"`
	Requests   []QARequest  `json:"requests"`
	Deliveries []QADelivery `json:"deliveries"`
	Artifacts  []QAArtifact `json:"artifacts"`
}

func (s *Store) migrateQA() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS qa_questions(
 id TEXT PRIMARY KEY,connection_id TEXT NOT NULL,chat_id TEXT NOT NULL,name TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'active',created_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS qa_turns(
 seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,question_id TEXT NOT NULL REFERENCES qa_questions(id),
 user_id TEXT NOT NULL,sender_name TEXT NOT NULL,input TEXT NOT NULL,state TEXT NOT NULL,
 thread_id TEXT NOT NULL DEFAULT '',turn_id TEXT NOT NULL DEFAULT '',answer TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS qa_turns_queue ON qa_turns(state,seq);
 CREATE INDEX IF NOT EXISTS qa_turns_question ON qa_turns(question_id,seq);
 CREATE TABLE IF NOT EXISTS qa_deliveries(
 id TEXT PRIMARY KEY,event_id TEXT NOT NULL REFERENCES qa_turns(id),question_id TEXT NOT NULL REFERENCES qa_questions(id),
 part INTEGER NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',platform_id TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',UNIQUE(event_id,part));
 CREATE TABLE IF NOT EXISTS qa_reply_refs(
 connection_id TEXT NOT NULL,chat_id TEXT NOT NULL,reference TEXT NOT NULL,
 question_id TEXT NOT NULL REFERENCES qa_questions(id),event_id TEXT NOT NULL REFERENCES qa_turns(id),
 PRIMARY KEY(connection_id,chat_id,reference));
 CREATE TABLE IF NOT EXISTS qa_requests(
 id TEXT PRIMARY KEY,question_id TEXT NOT NULL REFERENCES qa_questions(id),event_id TEXT NOT NULL REFERENCES qa_turns(id),
 method TEXT NOT NULL,detail TEXT NOT NULL,payload TEXT NOT NULL,state TEXT NOT NULL,created_at INTEGER NOT NULL);`)
	if e != nil {
		return e
	}
	if e = s.migrateQAProgress(); e != nil {
		return e
	}
	return s.migrateQAArtifacts()
}

// AcceptQA 在同一事务中保存入站、初始进度与接收通知，引用仅按平台标识精确解析。
func (s *Store) AcceptQA(m im.Message, maxInput, capacity int) error {
	if m.Address.Channel != "dingtalk" || m.Address.Connection == "" || m.Address.ChatID == "" || m.ID == "" || m.UserID == "" {
		return errors.New("问答消息身份无效")
	}
	id := Hash(m.Address.BaseKey() + "\x00" + m.ID)
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var exists int
	if e = tx.QueryRow("SELECT count(*) FROM qa_turns WHERE id=?", id).Scan(&exists); e != nil {
		return e
	}
	if exists > 0 {
		return nil
	}
	var queued int
	if e = tx.QueryRow("SELECT count(*) FROM qa_turns WHERE state IN ('queued','starting','running')").Scan(&queued); e != nil {
		return e
	}
	if queued >= capacity {
		return errors.New("问答等待队列已满")
	}
	qid, notice := "", ""
	if m.HasReply {
		var err error
		qid, _, err = qaResolveReply(tx, m.Address, m.ReplyToIDs)
		if errors.Is(err, sql.ErrNoRows) {
			notice = "无法定位这条引用所属的问答，未执行。请引用本机器人的回答或状态回执并 @追问。"
		} else if err != nil {
			// 引用歧义和跨群引用必须拒绝，数据库故障则回滚入站。
			if !errors.Is(err, errQAReplyForeign) && !errors.Is(err, errQAReplyAmbiguous) {
				return err
			}
			notice = err.Error()
		}
	}
	if strings.TrimSpace(m.Text) == "" {
		notice = "请在 @机器人后填写问题内容。"
	}
	if len([]rune(m.Text)) > maxInput {
		notice = "问题超过当前输入长度限制，未执行。请缩短后重新提问。"
	}
	if qid != "" {
		var status string
		if e = tx.QueryRow("SELECT status FROM qa_questions WHERE id=?", qid).Scan(&status); e != nil {
			return e
		}
		if status != "active" {
			notice = "这条问答的上轮执行结果未确认，已停止续接，避免重复操作。请先在本机核对，或直接 @开始新的问答。"
		}
	}
	if qid == "" {
		qid = ID()
		name := []rune(strings.TrimSpace(m.Text))
		if len(name) > 60 {
			name = name[:60]
		}
		qstate := "active"
		if notice != "" {
			qstate = "notice"
		}
		if _, e = tx.Exec("INSERT INTO qa_questions(id,connection_id,chat_id,name,status,created_at) VALUES(?,?,?,?,?,?)", qid, m.Address.Connection, m.Address.ChatID, string(name), qstate, time.Now().Unix()); e != nil {
			return e
		}
	}
	state := "queued"
	if notice != "" {
		state = "rejected"
	}
	// 拒绝的输入也限制长度，避免超长回调膨胀数据库。
	input := []rune(m.Text)
	if len(input) > maxInput {
		input = input[:maxInput]
	}
	if _, e = tx.Exec("INSERT INTO qa_turns(id,question_id,user_id,sender_name,input,state,error,created_at) VALUES(?,?,?,?,?,?,?,?)", id, qid, m.UserID, m.SenderName, string(input), state, notice, time.Now().Unix()); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT OR IGNORE INTO qa_workflows(question_id,kind,phase,updated_at) VALUES(?,'question','',?)", qid, time.Now().Unix()); e != nil {
		return e
	}
	if e = qaSetProgressTx(tx, id, state, notice, notice == ""); e != nil {
		return e
	}
	if notice != "" {
		if _, e = tx.Exec("INSERT INTO qa_deliveries(id,event_id,question_id,part,body) VALUES(?,?,?,?,?)", Hash(id+":0"), id, qid, 0, notice); e != nil {
			return e
		}
	}
	return tx.Commit()
}

const qaTurnColumns = "t.id,t.question_id,t.seq,q.connection_id,q.chat_id,t.user_id,t.sender_name,t.input,t.state,t.thread_id,t.turn_id,t.answer,t.error,t.created_at"

func scanQATurn(row interface{ Scan(...any) error }) (QATurn, error) {
	var t QATurn
	e := row.Scan(&t.ID, &t.QuestionID, &t.Seq, &t.Connection, &t.ChatID, &t.UserID, &t.SenderName, &t.Text, &t.State, &t.ThreadID, &t.TurnID, &t.Answer, &t.Error, &t.CreatedAt)
	return t, e
}
func (s *Store) QAQueued(limit int) ([]QATurn, error) {
	rows, e := s.db.Query("SELECT "+qaTurnColumns+` FROM qa_turns t JOIN qa_questions q ON q.id=t.question_id WHERE t.state='queued'
 AND NOT EXISTS(SELECT 1 FROM qa_turns prior WHERE prior.question_id=t.question_id AND prior.seq<t.seq AND prior.state IN ('queued','starting','running'))
 ORDER BY t.seq LIMIT ?`, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []QATurn{}
	for rows.Next() {
		t, e := scanQATurn(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) QAClaim(id string) (bool, error) {
	r, e := s.db.Exec(`UPDATE qa_turns SET state='starting' WHERE id=? AND state='queued'
 AND NOT EXISTS(SELECT 1 FROM qa_turns prior WHERE prior.question_id=qa_turns.question_id AND prior.seq<qa_turns.seq AND prior.state IN ('queued','starting','running'))`, id)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}

// QASubmitted 保存当前真实模型轮次，支持范围分类后继续提交业务回答。
func (s *Store) QASubmitted(id, thread, turn string) error {
	r, e := s.db.Exec("UPDATE qa_turns SET state='running',thread_id=?,turn_id=? WHERE id=? AND state IN ('starting','running')", thread, turn, id)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("问答提交状态已变化")
	}
	return nil
}
func (s *Store) QAHistory(qid string, before int64) ([]QATurn, error) {
	rows, e := s.db.Query("SELECT "+qaTurnColumns+" FROM qa_turns t JOIN qa_questions q ON q.id=t.question_id WHERE t.question_id=? AND t.seq<? AND t.state='completed' ORDER BY t.seq DESC LIMIT 31", qid, before)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []QATurn{}
	for rows.Next() {
		t, e := scanQATurn(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
func (s *Store) QAFinish(id, state, answer, detail string, parts []string) error {
	if state != "completed" && state != "failed" && state != "unknown" && state != "rejected" {
		return errors.New("问答结束状态无效")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var qid, current string
	if e = tx.QueryRow("SELECT question_id,state FROM qa_turns WHERE id=?", id).Scan(&qid, &current); e != nil {
		return e
	}
	if current != "starting" && current != "running" && !(current == "queued" && state == "rejected") {
		return errors.New("问答已结束，不能重复保存回复")
	}
	if _, e = tx.Exec("UPDATE qa_turns SET state=?,answer=?,error=? WHERE id=?", state, answer, detail, id); e != nil {
		return e
	}
	if state == "unknown" {
		if _, e = tx.Exec("UPDATE qa_questions SET status='blocked' WHERE id=?", qid); e != nil {
			return e
		}
	}
	for i, part := range parts {
		if _, e = tx.Exec("INSERT INTO qa_deliveries(id,event_id,question_id,part,body) VALUES(?,?,?,?,?)", Hash(id+":"+strconv.Itoa(i)), id, qid, i, part); e != nil {
			return e
		}
	}
	stage := state
	if state == "completed" {
		stage = "sending"
	}
	if e = qaSetProgressTx(tx, id, stage, detail, false); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) QAQuestionActive(id string) (bool, error) {
	var state string
	e := s.db.QueryRow("SELECT status FROM qa_questions WHERE id=?", id).Scan(&state)
	return state == "active", e
}

// RecoverQA 保留不确定结果，不重新提交模型任务，也不重发未知通知。
func (s *Store) RecoverQA() error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE qa_notices SET state='unknown',error='后台停止时通知发送结果未确认；未自动重发',updated_at=? WHERE state='sending'", time.Now().Unix()); e != nil {
		return e
	}
	rows, e := tx.Query("SELECT id FROM qa_turns WHERE state IN ('starting','running')")
	if e != nil {
		return e
	}
	var events []string
	for rows.Next() {
		var event string
		if e = rows.Scan(&event); e != nil {
			rows.Close()
			return e
		}
		events = append(events, event)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, event := range events {
		if e = qaSetProgressTx(tx, event, "unknown", "后台停止时执行结果未确认；未自动重发，请核对本机操作结果", true); e != nil {
			return e
		}
	}
	for _, query := range []string{
		"UPDATE qa_questions SET status='blocked' WHERE id IN (SELECT question_id FROM qa_turns WHERE state IN ('starting','running'))",
		"UPDATE qa_turns SET state='unknown',error='后台停止时执行结果未确认；未自动重发，请核对本机操作结果' WHERE state IN ('starting','running')",
		"UPDATE qa_deliveries SET state='unknown',error='后台停止时发送结果未确认；请先核对群消息' WHERE state='sending'",
		"UPDATE qa_requests SET state='expired' WHERE state='pending'",
	} {
		if _, e = tx.Exec(query); e != nil {
			return e
		}
	}
	rows, e = tx.Query("SELECT DISTINCT event_id FROM qa_deliveries WHERE state='unknown'")
	if e != nil {
		return e
	}
	events = nil
	for rows.Next() {
		var event string
		if e = rows.Scan(&event); e != nil {
			rows.Close()
			return e
		}
		events = append(events, event)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, event := range events {
		if e = qaDeliveryProgressTx(tx, event); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) QAPendingDeliveries(limit int) ([]QADelivery, error) {
	rows, e := s.db.Query(`SELECT d.id,d.question_id,d.event_id,q.connection_id,q.chat_id,d.body,d.state,d.error,d.part
 FROM qa_deliveries d JOIN qa_questions q ON q.id=d.question_id WHERE d.state='pending'
 AND NOT EXISTS(SELECT 1 FROM qa_deliveries prior WHERE prior.event_id=d.event_id AND prior.part<d.part AND prior.state<>'sent')
 ORDER BY (SELECT seq FROM qa_turns WHERE id=d.event_id),d.part LIMIT ?`, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []QADelivery{}
	for rows.Next() {
		var d QADelivery
		if e = rows.Scan(&d.ID, &d.QuestionID, &d.EventID, &d.Connection, &d.ChatID, &d.Body, &d.State, &d.Error, &d.Part); e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) QAClaimDelivery(id string) (bool, error) {
	r, e := s.db.Exec("UPDATE qa_deliveries SET state='sending' WHERE id=? AND state='pending'", id)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) QADeliveryResult(d QADelivery, state, platform, detail string) error {
	if state != "sent" && state != "failed" && state != "unknown" {
		return errors.New("问答投递状态无效")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var stored QADelivery
	if e = tx.QueryRow(`SELECT d.event_id,d.question_id,q.connection_id,q.chat_id FROM qa_deliveries d
 JOIN qa_questions q ON q.id=d.question_id WHERE d.id=? AND d.state='sending'`, d.ID).Scan(&stored.EventID, &stored.QuestionID, &stored.Connection, &stored.ChatID); e != nil {
		return e
	}
	if d.EventID != stored.EventID || d.QuestionID != stored.QuestionID || d.Connection != stored.Connection || d.ChatID != stored.ChatID {
		return errors.New("问答回复发送目标不匹配")
	}
	r, e := tx.Exec("UPDATE qa_deliveries SET state=?,platform_id=?,error=? WHERE id=? AND state='sending'", state, platform, detail, d.ID)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("投递状态已变化")
	}
	if state == "sent" && platform != "" {
		if e = qaPutReplyRefTx(tx, d.Connection, d.ChatID, platform, d.QuestionID, d.EventID); e != nil {
			return e
		}
	}
	if e = qaDeliveryProgressTx(tx, d.EventID); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) QAResolveDelivery(id, action string, confirmed bool) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var state, event string
	if e = tx.QueryRow("SELECT state,event_id FROM qa_deliveries WHERE id=?", id).Scan(&state, &event); e != nil {
		return e
	}
	next := "pending"
	switch action {
	case "retry":
		if state != "failed" {
			return errors.New("只允许重试明确失败的发送")
		}
	case "confirmed_missing":
		if state != "unknown" || !confirmed {
			return errors.New("请先核对群内未收到；重发仍可能重复")
		}
	case "confirmed_sent":
		if state != "unknown" || !confirmed {
			return errors.New("请先核对群内确实已收到")
		}
		next = "sent"
	default:
		return errors.New("不支持的问答投递操作")
	}
	if _, e = tx.Exec("UPDATE qa_deliveries SET state=?,error='' WHERE id=? AND state=?", next, id, state); e != nil {
		return e
	}
	if e = qaDeliveryProgressTx(tx, event); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) QAPutRequest(r QARequest) error {
	_, e := s.db.Exec("INSERT INTO qa_requests(id,question_id,event_id,method,detail,payload,state,created_at) VALUES(?,?,?,?,?,?,'pending',?)", r.ID, r.QuestionID, r.EventID, r.Method, r.Detail, string(r.Payload), time.Now().Unix())
	return e
}
func (s *Store) QARequestState(id, state string) error {
	_, e := s.db.Exec("UPDATE qa_requests SET state=? WHERE id=? AND state='pending'", state, id)
	return e
}

func QAStateSnapshot(ctx context.Context, dir string) (QAView, error) {
	out := QAView{Turns: []QATurn{}, Requests: []QARequest{}, Deliveries: []QADelivery{}, Artifacts: []QAArtifact{}}
	db, e := qaOpenSnapshot(dir)
	if os.IsNotExist(e) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	defer db.Close()
	var exists int
	if e = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='qa_turns'").Scan(&exists); e != nil {
		return out, e
	}
	if exists == 0 {
		return out, nil
	}
	rows, e := db.QueryContext(ctx, "SELECT "+qaTurnColumns+" FROM qa_turns t JOIN qa_questions q ON q.id=t.question_id ORDER BY t.seq DESC LIMIT 50")
	if e != nil {
		return out, e
	}
	for rows.Next() {
		t, err := scanQATurn(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		t.Answer = ""
		if preview := []rune(t.Text); len(preview) > 512 {
			t.Text = string(preview[:512]) + "…"
		}
		out.Turns = append(out.Turns, t)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	hasProgress, e := qaHasTable(ctx, db, "qa_progress")
	if e != nil {
		return out, e
	}
	if hasProgress {
		for i := range out.Turns {
			p, err := scanQAProgress(db.QueryRowContext(ctx, "SELECT "+qaProgressColumns+qaProgressJoins+" WHERE p.event_id=?", out.Turns[i].ID))
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return out, err
			}
			out.Turns[i].Progress = &p
		}
	}
	rows, e = db.QueryContext(ctx, `SELECT r.id,r.question_id,r.event_id,r.method,r.detail,r.payload,r.state,r.created_at,
 q.connection_id,t.sender_name,substr(t.input,1,512)
 FROM qa_requests r JOIN qa_turns t ON t.id=r.event_id JOIN qa_questions q ON q.id=r.question_id
 WHERE r.state='pending' ORDER BY r.created_at LIMIT 64`)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var r QARequest
		var raw string
		if e = rows.Scan(&r.ID, &r.QuestionID, &r.EventID, &r.Method, &r.Detail, &raw, &r.State, &r.CreatedAt, &r.Connection, &r.SenderName, &r.Question); e != nil {
			rows.Close()
			return out, e
		}
		r.Payload = json.RawMessage(raw)
		out.Requests = append(out.Requests, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	rows, e = db.QueryContext(ctx, "SELECT d.id,d.question_id,d.event_id,q.connection_id,q.chat_id,d.state,d.error,d.part FROM qa_deliveries d JOIN qa_questions q ON q.id=d.question_id WHERE d.state IN ('failed','unknown') ORDER BY d.rowid DESC LIMIT 50")
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var d QADelivery
		if e = rows.Scan(&d.ID, &d.QuestionID, &d.EventID, &d.Connection, &d.ChatID, &d.State, &d.Error, &d.Part); e != nil {
			rows.Close()
			return out, e
		}
		out.Deliveries = append(out.Deliveries, d)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	hasArtifacts, e := qaHasTable(ctx, db, "qa_artifacts")
	if e != nil || !hasArtifacts {
		return out, e
	}
	rows, e = db.QueryContext(ctx, "SELECT "+qaArtifactColumns+" FROM qa_artifacts ORDER BY created_at DESC,rowid DESC LIMIT 50")
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanQAArtifact(rows)
		if err != nil {
			return out, err
		}
		out.Artifacts = append(out.Artifacts, a)
	}
	return out, rows.Err()
}

// QAReconcileConnections 保留孤立记录；未发送通知标记失败，发送中的通知标记未知。
func (s *Store) QAReconcileConnections(connections []string) error {
	args := make([]any, len(connections))
	for i, id := range connections {
		args[i] = id
	}
	predicate := "1=1"
	if len(connections) > 0 {
		predicate = "connection_id NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(connections)), ",") + ")"
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// 先取完整目标并关闭 rows，再在同一连接更新进度。
	query := "SELECT id,'failed' FROM qa_turns WHERE state='queued' AND question_id IN (SELECT id FROM qa_questions WHERE " + predicate + ")" +
		" UNION ALL SELECT DISTINCT event_id,'delivery_failed' FROM qa_deliveries WHERE state='pending' AND question_id IN (SELECT id FROM qa_questions WHERE " + predicate + ")"
	queryArgs := append(append([]any{}, args...), args...)
	rows, e := tx.Query(query, queryArgs...)
	if e != nil {
		return e
	}
	var targets []struct{ event, stage string }
	for rows.Next() {
		var target struct{ event, stage string }
		if e = rows.Scan(&target.event, &target.stage); e != nil {
			rows.Close()
			return e
		}
		targets = append(targets, target)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE qa_deliveries SET state='failed',error='对应钉钉连接未配置；记录已保留，恢复连接后可手动处理' WHERE state='pending' AND question_id IN (SELECT id FROM qa_questions WHERE "+predicate+")", args...); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE qa_turns SET state='failed',error='对应钉钉连接已移除，未提交的问答已停止，不自动执行' WHERE state='queued' AND question_id IN (SELECT id FROM qa_questions WHERE "+predicate+")", args...); e != nil {
		return e
	}
	noticeArgs := append([]any{time.Now().Unix()}, args...)
	if _, e = tx.Exec(`UPDATE qa_notices SET
 state=CASE WHEN state='sending' THEN 'unknown' ELSE 'failed' END,
 error=CASE WHEN state='sending' THEN '对应钉钉连接已移除，通知发送结果未知；未自动重发' ELSE '对应钉钉连接已移除，通知未发送' END,
 updated_at=? WHERE state IN ('pending','sending') AND `+predicate, noticeArgs...); e != nil {
		return e
	}
	for _, target := range targets {
		if target.stage == "delivery_failed" {
			// 同一回答已有未知片段时，仍保留发送未知，不能被连接移除覆盖。
			e = qaDeliveryProgressTx(tx, target.event)
		} else {
			e = qaSetProgressTx(tx, target.event, target.stage, "对应钉钉连接已移除；记录已保留，未自动执行或重发", false)
		}
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) QADeliveryConnection(id string) (string, error) {
	var connection string
	e := s.db.QueryRow("SELECT q.connection_id FROM qa_deliveries d JOIN qa_questions q ON q.id=d.question_id WHERE d.id=?", id).Scan(&connection)
	return connection, e
}
