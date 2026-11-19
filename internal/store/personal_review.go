package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"im-agent/internal/im"
)

type PersonalReview struct {
	Target       string `json:"target"`
	PeerName     string `json:"peer_name"`
	EventID      string `json:"event_id"`
	ThreadID     string `json:"thread_id"`
	Peer         string `json:"peer"`
	Input        string `json:"input"`
	Assessment   string `json:"assessment"`
	Reason       string `json:"reason"`
	Draft        string `json:"draft"`
	State        string `json:"state"`
	Version      int64  `json:"version"`
	DeliveryItem string `json:"delivery_item"`
	ApprovedText string `json:"approved_text"`
	UpdatedAt    int64  `json:"updated_at"`
	AutoEligible bool   `json:"auto_eligible"`
	ApprovalKind string `json:"approval_kind"`
	CanSend      bool   `json:"can_send"`
}
type PersonalReviewAction struct {
	EventID string `json:"event_id"`
	Version int64  `json:"version"`
	Action  string `json:"action"`
	Text    string `json:"text"`
}

const reviewFields = "event_id,thread_id,peer,input,assessment,reason,draft,state,version,delivery_item,approved_text,updated_at,auto_eligible,approval_kind,EXISTS(SELECT 1 FROM request_results r WHERE r.event_id=personal_reviews.event_id AND r.execution_state='completed' AND r.sealed=1),coalesce((SELECT chat_id FROM sessions WHERE thread_id=personal_reviews.thread_id),''),coalesce((SELECT json_extract(payload,'$.UserName') FROM inbound_messages WHERE event_id=personal_reviews.event_id),'')"

func scanReview(row interface{ Scan(...any) error }) (PersonalReview, error) {
	var r PersonalReview
	err := row.Scan(&r.EventID, &r.ThreadID, &r.Peer, &r.Input, &r.Assessment, &r.Reason, &r.Draft, &r.State, &r.Version, &r.DeliveryItem, &r.ApprovedText, &r.UpdatedAt, &r.AutoEligible, &r.ApprovalKind, &r.CanSend, &r.Target, &r.PeerName)
	return r, err
}
func (s *Store) migratePersonalReviews() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS personal_reviews (
event_id TEXT PRIMARY KEY REFERENCES events(id), thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
peer TEXT NOT NULL,input TEXT NOT NULL,assessment TEXT NOT NULL,reason TEXT NOT NULL,draft TEXT NOT NULL,
state TEXT NOT NULL DEFAULT 'pending',version INTEGER NOT NULL DEFAULT 1,
delivery_item TEXT NOT NULL DEFAULT '',approved_text TEXT NOT NULL DEFAULT '',
notified INTEGER NOT NULL DEFAULT 0,updated_at INTEGER NOT NULL,created_at INTEGER NOT NULL,
auto_eligible INTEGER NOT NULL DEFAULT 0,approval_kind TEXT NOT NULL DEFAULT 'manual');
CREATE INDEX IF NOT EXISTS personal_reviews_thread ON personal_reviews(thread_id,state);
CREATE TABLE IF NOT EXISTS personal_review_audit (
id INTEGER PRIMARY KEY AUTOINCREMENT,event_id TEXT NOT NULL,version INTEGER NOT NULL,action TEXT NOT NULL,
text_hash TEXT NOT NULL,created_at INTEGER NOT NULL);`)
	return err
}

// 从已封存的模型输出补建待审记录，覆盖完成后进程退出、尚未来得及生成草稿的情况。
type ReviewCandidate struct {
	Body    string
	Natural bool
	Request string
}

func (s *Store) PersonalReviewCandidates(thread string) (map[string]ReviewCandidate, error) {
	rows, err := s.db.Query(`SELECT r.event_id,p.body,coalesce(json_array_length(i.payload,'$.inbox_sources'),0)>0,coalesce(json_extract(i.payload,'$.Text'),'') FROM request_results r
LEFT JOIN inbound_messages i ON i.event_id=r.event_id
JOIN request_result_replies m ON m.event_id=r.event_id
JOIN live_mirror_parts p ON p.thread_id=m.thread_id AND p.item_id=m.item_id
JOIN live_mirror_entries e ON e.thread_id=p.thread_id AND e.item_id=p.item_id
WHERE r.thread_id=? AND r.execution_state='completed' AND r.sealed=1
AND r.delivery_state<>'sent'
AND NOT EXISTS(SELECT 1 FROM personal_reviews v WHERE v.event_id=r.event_id)
AND NOT EXISTS(SELECT 1 FROM request_result_replies z JOIN live_mirror_parts q ON q.thread_id=z.thread_id AND q.item_id=z.item_id WHERE z.event_id=r.event_id AND q.state<>'pending')
ORDER BY r.created_at,e.seq,p.part_index`, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ReviewCandidate{}
	for rows.Next() {
		var event, body string
		var natural bool
		var request string
		if err = rows.Scan(&event, &body, &natural, &request); err != nil {
			return nil, err
		}
		candidate := out[event]
		candidate.Natural = natural
		candidate.Request = request
		if candidate.Body == "\x00" {
			continue
		}
		if len(candidate.Body)+len(body) > 64000 {
			candidate.Body = "\x00"
		} else {
			candidate.Body += body
		}
		out[event] = candidate
	}
	return out, rows.Err()
}
func (s *Store) PutPersonalReview(thread, event, assessment, reason, draft string, autoAfter int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	var received int64
	if err = tx.QueryRow(`SELECT i.payload,i.created_at FROM inbound_messages i JOIN request_results r ON r.event_id=i.event_id
JOIN sessions s ON s.thread_id=r.thread_id WHERE i.event_id=? AND r.thread_id=? AND r.execution_state='completed'
AND r.sealed=1 AND s.channel='dingtalk_personal'`, event, thread).Scan(&raw, &received); err != nil {
		return err
	}
	var msg im.Message
	if json.Unmarshal([]byte(raw), &msg) != nil {
		return errors.New("个人收件原文不可读取")
	}
	_, err = tx.Exec(`INSERT INTO personal_reviews(event_id,thread_id,peer,input,assessment,reason,draft,updated_at,created_at,auto_eligible)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, event, thread, msg.UserID, msg.Text, assessment, reason, draft, time.Now().Unix(), time.Now().Unix(), autoAfter > 0 && received*1000 >= autoAfter)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) PersonalReview(event string) (PersonalReview, error) {
	return scanReview(s.db.QueryRow("SELECT "+reviewFields+" FROM personal_reviews WHERE event_id=?", event))
}

func (s *Store) PutPersonalFailure(session Session, msg im.Message, reason string) error {
	_, err := s.db.Exec(`INSERT INTO personal_reviews(event_id,thread_id,peer,input,assessment,reason,draft,updated_at,created_at)
VALUES(?,?,?,?,'manual',?,'',?,?) ON CONFLICT(event_id) DO NOTHING`, msg.ID, session.ThreadID, msg.UserID, msg.Text, reason, time.Now().Unix(), time.Now().Unix())
	return err
}
func (s *Store) PendingPersonalReviews(thread string) ([]PersonalReview, error) {
	rows, err := s.db.Query("SELECT "+reviewFields+" FROM personal_reviews WHERE thread_id=? AND state='pending' ORDER BY created_at,event_id LIMIT 100", thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PersonalReview{}
	for rows.Next() {
		r, e := scanReview(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// 批准与生成实际待发片段同事务。原始模型输出仍保留，实际投递另用版本化消息标识。
func (s *Store) DecidePersonalReview(session Session, r PersonalReviewAction, parts []string, automatic bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := scanReview(tx.QueryRow("SELECT "+reviewFields+" FROM personal_reviews WHERE event_id=?", r.EventID))
	if err != nil {
		return err
	}
	if old.Version != r.Version || old.State != "pending" || old.ThreadID != session.ThreadID {
		return errors.New("审核记录已变化或已处理，请刷新后再操作")
	}
	var currentState, address string
	if err = tx.QueryRow(`SELECT s.status,e.chat_key FROM sessions s JOIN events e ON e.id=? WHERE s.thread_id=?`, r.EventID, old.ThreadID).Scan(&currentState, &address); err != nil {
		return err
	}
	localClose := r.Action == "manual" || r.Action == "ignored"
	if (!localClose && currentState != "active") || address != Hash(session.Address.Key()) || session.Address.Channel != "dingtalk_personal" {
		return errors.New("会话已停用或地址已改变")
	}
	state := r.Action
	item := ""
	text := strings.TrimSpace(r.Text)
	switch r.Action {
	case "save":
		if len(text) > 24000 {
			return errors.New("草稿过长")
		}
		state = "pending"
	case "send":
		if !old.CanSend {
			return errors.New("本次分析未完成，不能发送自动生成的草稿；请自行回复")
		}
		if text == "" || len(text) > 24000 || len(parts) == 0 {
			return errors.New("回复不能为空或过长")
		}
		var attempted int
		if err = tx.QueryRow(`SELECT count(*) FROM request_result_replies m JOIN live_mirror_parts p ON p.thread_id=m.thread_id AND p.item_id=m.item_id WHERE m.event_id=? AND p.state<>'pending'`, r.EventID).Scan(&attempted); err != nil {
			return err
		}
		if attempted > 0 {
			return errors.New("原消息已有发送或不确定回执，不能重新批准发送")
		}
		if automatic && (!old.AutoEligible || old.Assessment != "ready" || text != old.Draft) {
			return errors.New("自动回复条件未满足")
		}
		var newer int
		newerQuery := `SELECT count(*) FROM inbound_messages n JOIN inbound_messages o ON o.event_id=? WHERE n.address_key=o.address_key AND n.rowid>o.rowid`
		wire, wireErr := session.Address.DeliveryAddress()
		if wireErr != nil {
			return wireErr
		}
		if strings.Contains(wire.ChatID, ":group:") {
			newerQuery += ` AND json_extract(n.payload,'$.FollowupTarget')=coalesce(nullif(json_extract(o.payload,'$.FollowupTarget'),''),o.event_id)`
		}
		if err = tx.QueryRow(newerQuery, r.EventID).Scan(&newer); err != nil {
			return err
		}
		if newer > 0 {
			return errors.New("该任务已有明确关联的补充消息，请先处理最新任务；旧草稿不自动发送")
		}
		item = "personal-review:" + r.EventID + fmt.Sprintf(":v%d:", r.Version) + Hash(text)[:16]
		if _, err = tx.Exec(`UPDATE live_mirror_entries SET suppressed=1 WHERE thread_id=? AND item_id IN (SELECT item_id FROM request_result_replies WHERE event_id=?)`, old.ThreadID, r.EventID); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM request_result_replies WHERE event_id=?", r.EventID); err != nil {
			return err
		}
		entry := MirrorEntry{ItemID: item, Role: "assistant", Fingerprint: Hash(text), Parts: parts}
		if err = prepareMirrorEntry(tx, old.ThreadID, "", entry); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO request_result_replies(event_id,thread_id,item_id,expected_parts) VALUES(?,?,?,?)`, r.EventID, old.ThreadID, item, len(parts)); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE request_results SET expected_replies=1,delivery_state='pending',updated_at=? WHERE event_id=?`, time.Now().Unix(), r.EventID); err != nil {
			return err
		}
		state = "approved"
	case "manual", "ignored":
		if _, err = tx.Exec("UPDATE request_results SET delivery_state='not_applicable',updated_at=? WHERE event_id=?", time.Now().Unix(), r.EventID); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE live_mirror_entries SET suppressed=1 WHERE thread_id=? AND item_id IN (SELECT item_id FROM request_result_replies WHERE event_id=?)`, old.ThreadID, r.EventID); err != nil {
			return err
		}
	default:
		return errors.New("不支持的个人审核操作")
	}
	approved := ""
	if r.Action == "send" {
		approved = text
	}
	if r.Action == "save" {
		old.Draft = text
	}
	kind := "manual"
	if automatic {
		kind = "auto"
	}
	if _, err = tx.Exec(`UPDATE personal_reviews SET state=?,version=version+1,draft=?,delivery_item=?,approved_text=?,updated_at=?,approval_kind=? WHERE event_id=? AND version=?`, state, old.Draft, item, approved, time.Now().Unix(), kind, r.EventID, r.Version); err != nil {
		return err
	}
	actor := r.Action
	if automatic {
		actor = "auto_send"
	}
	if _, err = tx.Exec(`INSERT INTO personal_review_audit(event_id,version,action,text_hash,created_at) VALUES(?,?,?,?,?)`, r.EventID, r.Version, actor, Hash(text), time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) PersonalPartApproved(thread, item string, autoPeers []string) (bool, error) {
	r, err := scanReview(s.db.QueryRow("SELECT "+reviewFields+" FROM personal_reviews WHERE thread_id=? AND delivery_item=? AND state IN ('approved','sending')", thread, item))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.ApprovalKind == "auto" {
		for _, peer := range autoPeers {
			if peer == r.Peer {
				return true, nil
			}
		}
		return false, nil
	}
	return true, nil
}
func (s *Store) RefreshPersonalDelivery(thread string) error {
	_, err := s.db.Exec(`UPDATE personal_reviews SET state=CASE
WHEN EXISTS(SELECT 1 FROM live_mirror_parts p WHERE p.thread_id=personal_reviews.thread_id AND p.item_id=delivery_item AND p.state='unknown') THEN 'unknown'
WHEN NOT EXISTS(SELECT 1 FROM live_mirror_parts p WHERE p.thread_id=personal_reviews.thread_id AND p.item_id=delivery_item AND p.state<>'sent') THEN 'sent'
ELSE 'sending' END, updated_at=? WHERE thread_id=? AND state IN ('approved','sending') AND delivery_item<>''`, time.Now().Unix(), thread)
	return err
}

// 关闭自动规则后，未开始投递的自动批准回到待审核；已有部分回执则要求核对。
func (s *Store) HoldDisabledAutomatic(thread string, peers []string) error {
	rows, err := s.db.Query("SELECT "+reviewFields+" FROM personal_reviews WHERE thread_id=? AND state IN ('approved','sending') AND approval_kind='auto'", thread)
	if err != nil {
		return err
	}
	var candidates []PersonalReview
	for rows.Next() {
		r, e := scanReview(rows)
		if e != nil {
			rows.Close()
			return e
		}
		allowed := false
		for _, p := range peers {
			if p == r.Peer {
				allowed = true
			}
		}
		if !allowed {
			candidates = append(candidates, r)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range candidates {
		tx, e := s.db.Begin()
		if e != nil {
			return e
		}
		var started int
		e = tx.QueryRow("SELECT count(*) FROM live_mirror_parts WHERE thread_id=? AND item_id=? AND state<>'pending'", thread, r.DeliveryItem).Scan(&started)
		if e != nil {
			tx.Rollback()
			return e
		}
		state, reason := "pending", "自动回复规则已关闭，等待本人审核。"
		if started > 0 {
			state = "unknown"
			reason = "自动回复规则已关闭；已有投递回执，请先核对已发送部分。"
		}
		_, e = tx.Exec("UPDATE personal_reviews SET state=?,auto_eligible=0,approval_kind='manual',version=version+1,notified=0,reason=?,updated_at=? WHERE event_id=? AND version=?", state, reason, time.Now().Unix(), r.EventID, r.Version)
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) ClaimPersonalNotice(event string) (bool, error) {
	r, err := s.db.Exec("UPDATE personal_reviews SET notified=1 WHERE event_id=? AND notified=0", event)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func PersonalReviewSnapshot(ctx context.Context, dir, event string) (*PersonalReview, error) {
	var result *PersonalReview
	err := taskBoardRead(ctx, dir, func(ctx context.Context, tx *sql.Tx, tables map[string]bool) error {
		if !tables["personal_reviews"] {
			return nil
		}
		r, err := scanReview(tx.QueryRowContext(ctx, "SELECT "+reviewFields+" FROM personal_reviews WHERE event_id=?", event))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result = &r
		return nil
	})
	return result, err
}
