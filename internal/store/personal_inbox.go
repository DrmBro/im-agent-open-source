package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"im-agent/internal/im"
	"time"
)

type PersonalInboxItem struct {
	Analysis   *InboxAnalysis `json:"analysis,omitempty"`
	ID         string         `json:"id"`
	Connection string         `json:"connection"`
	Channel    string         `json:"channel"`
	Chat       string         `json:"chat"`
	User       string         `json:"user"`
	UserName   string         `json:"user_name"`
	Text       string         `json:"text"`
	Received   int64          `json:"received"`
	IsDirect   bool           `json:"is_direct"`
	Mention    bool           `json:"mention"`
	Quoted     bool           `json:"quoted"`
	State      string         `json:"state"`
}
type PersonalInboxAction struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

func (s *Store) migratePersonalInbox() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS personal_inbox(id TEXT PRIMARY KEY,connection_id TEXT NOT NULL,channel TEXT NOT NULL,chat TEXT NOT NULL,user_id TEXT NOT NULL,user_name TEXT NOT NULL,text TEXT NOT NULL,payload TEXT NOT NULL,received_at INTEGER NOT NULL,is_direct INTEGER NOT NULL,mention INTEGER NOT NULL,quoted INTEGER NOT NULL,state TEXT NOT NULL DEFAULT 'new',updated_at INTEGER NOT NULL);CREATE INDEX IF NOT EXISTS personal_inbox_day ON personal_inbox(received_at,state);`)
	if e != nil {
		return e
	}
	// 修复旧版提前标记处理中但尚未绑定/提交的收件；不恢复任何已执行请求。
	_, e = s.db.Exec(`UPDATE personal_inbox SET state='new',updated_at=strftime('%s','now')
WHERE state='processing'
AND EXISTS (SELECT 1 FROM inbound_messages i JOIN events e ON e.id=i.event_id
 WHERE i.event_id=personal_inbox.id AND i.thread_id='' AND e.status='done' AND e.stage='received')
AND NOT EXISTS (SELECT 1 FROM request_results r WHERE r.event_id=personal_inbox.id)`)
	if e != nil {
		return e
	}
	return s.migrateInboxAnalysis()
}
func (s *Store) PutPersonalInbox(m im.Message) error {
	raw, _ := json.Marshal(m)
	mention := !m.IsDirect && !m.Followup
	_, e := s.db.Exec(`INSERT INTO personal_inbox(id,connection_id,channel,chat,user_id,user_name,text,payload,received_at,is_direct,mention,quoted,state,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, m.ID, m.Address.Connection, m.Address.Channel, m.Address.ChatID, m.UserID, m.UserName, m.Text, string(raw), time.Now().Unix(), m.IsDirect, boolInt(mention), boolInt(m.QuotedID != ""), "new", time.Now().Unix())
	return e
}
func (s *Store) PersonalInbox(ctx context.Context, today int64) ([]PersonalInboxItem, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT id,connection_id,channel,chat,user_id,user_name,text,received_at,is_direct,mention,quoted,state,coalesce(a.result,'') FROM personal_inbox p LEFT JOIN inbox_analysis a ON a.event_id=p.id WHERE received_at>=? ORDER BY received_at DESC,id`, today)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []PersonalInboxItem{}
	for rows.Next() {
		var x PersonalInboxItem
		var d, m, q int
		var analysis string
		if e = rows.Scan(&x.ID, &x.Connection, &x.Channel, &x.Chat, &x.User, &x.UserName, &x.Text, &x.Received, &d, &m, &q, &x.State, &analysis); e != nil {
			return nil, e
		}
		if analysis != "" {
			if err := json.Unmarshal([]byte(analysis), &x.Analysis); err != nil {
				return nil, err
			}
		}
		x.IsDirect = d != 0
		x.Mention = m != 0
		x.Quoted = q != 0
		out = append(out, x)
	}
	return out, rows.Err()
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (s *Store) InboxMessage(id string) (im.Message, error) {
	var raw string
	e := s.db.QueryRow("SELECT payload FROM personal_inbox WHERE id=?", id).Scan(&raw)
	var m im.Message
	if e == nil {
		e = json.Unmarshal([]byte(raw), &m)
	}
	return m, e
}
func (s *Store) MarkPersonalInbox(id, state string) error {
	if state != "ignored" && state != "processing" {
		return errors.New("收件状态无效")
	}
	_, e := s.db.Exec("UPDATE personal_inbox SET state=?,updated_at=? WHERE id=? AND state='new'", state, time.Now().Unix(), id)
	return e
}
func (s *Store) PersonalInboxState(id string) (string, error) {
	var x string
	e := s.db.QueryRow("SELECT state FROM personal_inbox WHERE id=?", id).Scan(&x)
	if errors.Is(e, sql.ErrNoRows) {
		return "", e
	}
	return x, e
}
