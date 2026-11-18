package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"im-agent/internal/im"
	"time"
)

type GroupFollowTask struct {
	ID           string `json:"id"`
	ThreadID     string `json:"thread_id"`
	Connection   string `json:"connection"`
	Chat         string `json:"chat"`
	Conversation string `json:"conversation"`
	Until        int64  `json:"until"`
	Summary      string `json:"summary"`
	WireKey      string `json:"-"`
}
type GroupFollowInput struct {
	ID      string     `json:"id"`
	State   string     `json:"state"`
	Target  string     `json:"target"`
	Reason  string     `json:"reason"`
	Version int64      `json:"version"`
	Message im.Message `json:"message"`
	WireKey string     `json:"-"`
	Expires int64      `json:"expires"`
}
type GroupFollowAction struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Action  string `json:"action"`
	Target  string `json:"target"`
}

func (s *Store) migrateGroupFollowup() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS group_follow_tasks (
root_event TEXT PRIMARY KEY REFERENCES events(id),thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
connection_id TEXT NOT NULL,wire_chat TEXT NOT NULL,wire_key TEXT NOT NULL,conversation_id TEXT NOT NULL,
state TEXT NOT NULL DEFAULT 'active',expires INTEGER NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS group_follow_tasks_wire ON group_follow_tasks(wire_key,state,expires);
CREATE TABLE IF NOT EXISTS group_follow_refs (
wire_key TEXT NOT NULL,platform_id TEXT NOT NULL,root_event TEXT NOT NULL REFERENCES group_follow_tasks(root_event),
kind TEXT NOT NULL,created_at INTEGER NOT NULL,PRIMARY KEY(wire_key,platform_id));
CREATE TABLE IF NOT EXISTS group_follow_inbox (
id TEXT PRIMARY KEY,wire_key TEXT NOT NULL,payload TEXT NOT NULL,state TEXT NOT NULL,
target TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',version INTEGER NOT NULL DEFAULT 1,
expires INTEGER NOT NULL,notified INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS group_follow_inbox_state ON group_follow_inbox(state,expires);
CREATE TABLE IF NOT EXISTS group_follow_audit (
seq INTEGER PRIMARY KEY AUTOINCREMENT,input_id TEXT NOT NULL,action TEXT NOT NULL,target TEXT NOT NULL,created_at INTEGER NOT NULL);`)
	return err
}

func (s *Store) OpenGroupFollowup(session Session, msg im.Message, minutes int, explicit ...bool) error {
	if msg.IsDirect || msg.Followup || session.Address.Channel != "dingtalk_personal" {
		return nil
	}
	wire, err := session.Address.DeliveryAddress()
	if err != nil {
		return err
	}
	if minutes == 0 {
		minutes = 30
	}
	if minutes < 5 || minutes > 120 {
		return errors.New("群跟进期限无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var created int64
	if err = tx.QueryRow("SELECT created_at FROM events WHERE id=?", msg.ID).Scan(&created); err != nil {
		return err
	}
	manual := len(explicit) > 0 && explicit[0]
	if manual {
		created = time.Now().Unix()
	}
	until := created + int64(minutes)*60
	if until <= time.Now().Unix() {
		return nil
	}
	_, err = tx.Exec(`INSERT INTO group_follow_tasks(root_event,thread_id,connection_id,wire_chat,wire_key,conversation_id,expires,created_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(root_event) DO NOTHING`, msg.ID, session.ThreadID, wire.Connection, wire.ChatID, Hash(wire.Key()), msg.ConversationID, until, created)
	if err != nil {
		return err
	}
	if manual {
		if _, err = tx.Exec("UPDATE group_follow_tasks SET state='active',expires=? WHERE root_event=?", until, msg.ID); err != nil {
			return err
		}
	}
	if msg.PlatformID != "" {
		_, err = tx.Exec(`INSERT INTO group_follow_refs(wire_key,platform_id,root_event,kind,created_at)
VALUES(?,?,?,'mention',?) ON CONFLICT(wire_key,platform_id) DO NOTHING`, Hash(wire.Key()), msg.PlatformID, msg.ID, time.Now().Unix())
		if err != nil {
			return err
		}
	}
	// 恢复旧任务跟进时复用已有的实际发送回执，不要求再次发送或改群。
	_, err = tx.Exec(`INSERT INTO group_follow_refs(wire_key,platform_id,root_event,kind,created_at)
SELECT DISTINCT ?,p.platform_id,?,'reply',? FROM request_result_replies r
JOIN live_mirror_parts p ON p.thread_id=r.thread_id AND p.item_id=r.item_id
WHERE r.event_id=? AND p.state='sent' AND p.platform_id<>''
ON CONFLICT(wire_key,platform_id) DO NOTHING`, Hash(wire.Key()), msg.ID, time.Now().Unix(), msg.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GroupFollowSource(event string) (Session, im.Message, error) {
	var msg im.Message
	for hop := 0; hop < 2; hop++ {
		var raw, thread string
		err := s.db.QueryRow("SELECT payload,thread_id FROM inbound_messages WHERE event_id=?", event).Scan(&raw, &thread)
		if err != nil {
			return Session{}, msg, err
		}
		if json.Unmarshal([]byte(raw), &msg) != nil || msg.ID != event {
			return Session{}, msg, errors.New("原群消息不可读取")
		}
		if msg.FollowupTarget != "" {
			event = msg.FollowupTarget
			continue
		}
		if msg.Followup {
			return Session{}, msg, errors.New("群补充缺少原始任务关联")
		}
		session, err := s.ByThread(thread)
		return session, msg, err
	}
	return Session{}, msg, errors.New("群任务关联存在循环或层级异常")
}

const followTaskSelect = `SELECT g.root_event,g.thread_id,g.connection_id,g.wire_chat,g.conversation_id,g.expires,g.wire_key,
coalesce(substr(json_extract(i.payload,'$.Text'),1,800),'') FROM group_follow_tasks g
JOIN sessions s ON s.thread_id=g.thread_id LEFT JOIN inbound_messages i ON i.event_id=g.root_event
LEFT JOIN personal_reviews p ON p.event_id=g.root_event
WHERE g.state='active' AND s.status='active' AND coalesce(p.state,'')<>'ignored'`

func scanGroupTask(row interface{ Scan(...any) error }) (GroupFollowTask, error) {
	var t GroupFollowTask
	e := row.Scan(&t.ID, &t.ThreadID, &t.Connection, &t.Chat, &t.Conversation, &t.Until, &t.WireKey, &t.Summary)
	return t, e
}
func (s *Store) ActiveGroupFollowups(wireKey string) ([]GroupFollowTask, error) {
	q := followTaskSelect + " AND g.expires>?"
	args := []any{time.Now().Unix()}
	if wireKey != "" {
		q += " AND g.wire_key=?"
		args = append(args, wireKey)
	}
	rows, err := s.db.Query(q+" ORDER BY g.created_at DESC LIMIT 100", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupFollowTask{}
	for rows.Next() {
		t, e := scanGroupTask(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) GroupFollowTask(id string) (GroupFollowTask, error) {
	return scanGroupTask(s.db.QueryRow(followTaskSelect+" AND g.root_event=?", id))
}

// 平台消息 ID 只在同一实际群地址内匹配，禁止跨群引用串到其它任务。
func (s *Store) StageGroupFollowup(msg im.Message) error {
	wire, err := msg.Address.DeliveryAddress()
	if err != nil {
		return err
	}
	key := Hash(wire.Key())
	tasks, err := s.ActiveGroupFollowups(key)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	expires := tasks[0].Until
	for _, t := range tasks {
		if t.Until > expires {
			expires = t.Until
		}
	}
	state, target := "queued", ""
	if msg.QuotedID != "" && (msg.QuotedConversationID == "" || msg.QuotedConversationID == msg.ConversationID) {
		err = s.db.QueryRow(`SELECT r.root_event FROM group_follow_refs r JOIN group_follow_tasks g ON g.root_event=r.root_event
WHERE r.wire_key=? AND r.platform_id=? AND g.state='active' AND g.expires>?`, key, msg.QuotedID, time.Now().Unix()).Scan(&target)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			state = "matched"
		}
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO group_follow_inbox(id,wire_key,payload,state,target,expires,created_at,updated_at)
SELECT ?,?,?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM events WHERE id=?)
ON CONFLICT(id) DO NOTHING`, msg.ID, key, string(raw), state, target, expires, time.Now().Unix(), time.Now().Unix(), msg.ID)
	return err
}

const followInputFields = "id,wire_key,payload,state,target,reason,version,expires"

func scanFollowInput(row interface{ Scan(...any) error }) (GroupFollowInput, error) {
	var r GroupFollowInput
	var raw string
	err := row.Scan(&r.ID, &r.WireKey, &raw, &r.State, &r.Target, &r.Reason, &r.Version, &r.Expires)
	if err != nil {
		return r, err
	}
	if raw != "" {
		if json.Unmarshal([]byte(raw), &r.Message) != nil {
			return r, errors.New("群补充消息不可解析")
		}
	}
	return r, nil
}
func (s *Store) GroupFollowInput(id string) (GroupFollowInput, error) {
	return scanFollowInput(s.db.QueryRow("SELECT "+followInputFields+" FROM group_follow_inbox WHERE id=?", id))
}
func (s *Store) PendingGroupFollowInputs() ([]GroupFollowInput, error) {
	// @订阅与临时群订阅可能先后推送同一条消息，只保留原收件链路。
	_, err := s.db.Exec("UPDATE group_follow_inbox SET state='duplicate',payload='',updated_at=? WHERE state IN ('queued','candidate','matched') AND EXISTS(SELECT 1 FROM events e WHERE e.id=group_follow_inbox.id)", time.Now().Unix())
	if err != nil {
		return nil, err
	}
	_, err = s.db.Exec("UPDATE group_follow_inbox SET state='expired',payload='',updated_at=? WHERE state IN ('queued','candidate','matched') AND expires<=?", time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT " + followInputFields + " FROM group_follow_inbox WHERE state IN ('queued','matched','accepted') ORDER BY created_at LIMIT 32")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GroupFollowInput{}
	for rows.Next() {
		r, e := scanFollowInput(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GroupFollowupReceived(id string) (bool, string, error) {
	var target string
	err := s.db.QueryRow("SELECT coalesce(json_extract(payload,'$.FollowupTarget'),'') FROM inbound_messages WHERE event_id=?", id).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return err == nil, target, err
}
func (s *Store) ClassifyGroupFollowup(id, target, reason string, related bool) error {
	state := "candidate"
	if !related {
		state = "ignored"
	}
	_, err := s.db.Exec(`UPDATE group_follow_inbox SET state=?,target=?,reason=?,version=version+1,
payload=CASE WHEN ?='ignored' THEN '' ELSE payload END,updated_at=? WHERE id=? AND state='queued'`, state, target, reason, state, time.Now().Unix(), id)
	return err
}
func (s *Store) DecideGroupFollowup(a GroupFollowAction) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := scanFollowInput(tx.QueryRow("SELECT "+followInputFields+" FROM group_follow_inbox WHERE id=?", a.ID))
	if err != nil {
		return err
	}
	if r.State != "candidate" || r.Version != a.Version || r.Expires <= time.Now().Unix() {
		return errors.New("关联候选已变化或跟进已到期，请刷新")
	}
	state := "ignored"
	if a.Action == "accept" {
		var valid int
		if err = tx.QueryRow("SELECT count(*) FROM group_follow_tasks g JOIN sessions s ON s.thread_id=g.thread_id WHERE g.root_event=? AND g.wire_key=? AND g.state='active' AND g.expires>? AND s.status='active'", a.Target, r.WireKey, time.Now().Unix()).Scan(&valid); err != nil {
			return err
		}
		if valid != 1 {
			return errors.New("所选任务不属于此群或跟进已结束")
		}
		state = "accepted"
	} else if a.Action != "ignore" {
		return errors.New("未知关联操作")
	}
	if _, err = tx.Exec("UPDATE group_follow_inbox SET state=?,target=?,version=version+1,payload=CASE WHEN ?='ignored' THEN '' ELSE payload END,updated_at=? WHERE id=?", state, a.Target, state, time.Now().Unix(), a.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO group_follow_audit(input_id,action,target,created_at) VALUES(?,?,?,?)", a.ID, a.Action, a.Target, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) FinishGroupFollowup(id, root, kind string) error {
	r, err := s.GroupFollowInput(id)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if root != "" && r.Message.PlatformID != "" {
		_, err = tx.Exec(`INSERT INTO group_follow_refs(wire_key,platform_id,root_event,kind,created_at) VALUES(?,?,?,?,?) ON CONFLICT(wire_key,platform_id) DO NOTHING`, r.WireKey, r.Message.PlatformID, root, kind, time.Now().Unix())
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec("UPDATE group_follow_inbox SET state=?,target=?,payload='',version=version+1,updated_at=? WHERE id=?", kind, root, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CloseGroupFollowup(root string) error {
	_, err := s.db.Exec("UPDATE group_follow_tasks SET state='closed' WHERE root_event=?", root)
	return err
}
func (s *Store) ClaimGroupFollowNotice(id string) (bool, error) {
	r, err := s.db.Exec("UPDATE group_follow_inbox SET notified=1 WHERE id=? AND notified=0 AND state='candidate'", id)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *Store) RecordGroupReplyReference(session Session, item, platform string) error {
	wire, err := session.Address.DeliveryAddress()
	if err != nil {
		return err
	}
	var event, raw string
	err = s.db.QueryRow("SELECT m.event_id,i.payload FROM request_result_replies m JOIN inbound_messages i ON i.event_id=m.event_id WHERE m.thread_id=? AND m.item_id=?", session.ThreadID, item).Scan(&event, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var msg im.Message
	if json.Unmarshal([]byte(raw), &msg) != nil {
		return errors.New("原消息关联不可解析")
	}
	if msg.FollowupTarget != "" {
		event = msg.FollowupTarget
	}
	_, err = s.db.Exec(`INSERT INTO group_follow_refs(wire_key,platform_id,root_event,kind,created_at)
SELECT ?,?,root_event,'reply',? FROM group_follow_tasks WHERE root_event=?
ON CONFLICT(wire_key,platform_id) DO NOTHING`, Hash(wire.Key()), platform, time.Now().Unix(), event)
	return err
}
