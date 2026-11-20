package store

import (
	"encoding/json"
	"im-agent/internal/im"
	"time"
)

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
	if err := s.migrateInboxTopics(); err != nil {
		return err
	}
	return s.migrateAssistant()
}
func (s *Store) PutPersonalInbox(m im.Message) error {
	raw, _ := json.Marshal(m)
	mention := !m.IsDirect && !m.Followup
	_, e := s.db.Exec(`INSERT INTO personal_inbox(id,connection_id,channel,chat,user_id,user_name,text,payload,received_at,is_direct,mention,quoted,state,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, m.ID, m.Address.Connection, m.Address.Channel, m.Address.ChatID, m.UserID, m.UserName, m.Text, string(raw), time.Now().Unix(), m.IsDirect, boolInt(mention), boolInt(m.QuotedID != ""), "new", time.Now().Unix())
	return e
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
