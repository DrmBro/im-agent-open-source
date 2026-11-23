package store

import (
	"database/sql"
	"errors"

	"im-agent/internal/im"
)

// 飞书副本与原执行会话共用 thread_id，但投递记录独立于钉钉审核。
type FeishuMirror struct {
	Thread  string
	Address im.Address
	Since   int64
}

func (s *Store) migrateFeishuMirrors() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS feishu_mirrors(
	thread TEXT NOT NULL REFERENCES sessions(thread_id),connection TEXT NOT NULL,chat TEXT NOT NULL,topic TEXT NOT NULL,root TEXT NOT NULL,since_ms INTEGER NOT NULL,
	PRIMARY KEY(thread,connection),UNIQUE(connection,chat,topic));
	CREATE TABLE IF NOT EXISTS feishu_mirror_receipts(
	thread TEXT NOT NULL,connection TEXT NOT NULL,item TEXT NOT NULL,part INTEGER NOT NULL,state TEXT NOT NULL,platform_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(thread,connection,item,part),FOREIGN KEY(thread,connection) REFERENCES feishu_mirrors(thread,connection));
	UPDATE feishu_mirror_receipts SET state='unknown' WHERE state='sending';`)
	return err
}
func (s *Store) FeishuMirror(thread, connection string) (FeishuMirror, error) {
	m := FeishuMirror{Thread: thread, Address: im.Address{Channel: "feishu", Connection: connection}}
	err := s.db.QueryRow("SELECT chat,topic,root,since_ms FROM feishu_mirrors WHERE thread=? AND connection=?", thread, connection).Scan(&m.Address.ChatID, &m.Address.TopicID, &m.Address.RootMessageID, &m.Since)
	return m, err
}
func (s *Store) FeishuMirrorAddress(a im.Address) (bool, error) {
	var found bool
	err := s.db.QueryRow("SELECT EXISTS(SELECT 1 FROM feishu_mirrors WHERE connection=? AND chat=? AND topic=?)", a.Connection, a.ChatID, a.TopicID).Scan(&found)
	return found, err
}
func (s *Store) SaveFeishuMirror(thread string, a im.Address, since int64) error {
	_, err := s.db.Exec("INSERT INTO feishu_mirrors(thread,connection,chat,topic,root,since_ms) VALUES(?,?,?,?,?,?)", thread, a.Connection, a.ChatID, a.TopicID, a.RootMessageID, since)
	return err
}
func (s *Store) MirrorItemTitle(thread string) (string, error) {
	var title string
	err := s.db.QueryRow("SELECT title FROM assistant_items WHERE thread_id=? ORDER BY updated_ms DESC LIMIT 1", thread).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return title, err
}
func (s *Store) ClaimFeishuMirrorPart(m FeishuMirror, item string, part int) (bool, error) {
	res, err := s.db.Exec("INSERT INTO feishu_mirror_receipts(thread,connection,item,part,state) VALUES(?,?,?,?,'sending') ON CONFLICT DO NOTHING", m.Thread, m.Address.Connection, item, part)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
func (s *Store) FinishFeishuMirrorPart(m FeishuMirror, item string, part int, receipt string, uncertain bool) error {
	args := []any{m.Thread, m.Address.Connection, item, part}
	if receipt == "" && !uncertain {
		_, err := s.db.Exec("DELETE FROM feishu_mirror_receipts WHERE thread=? AND connection=? AND item=? AND part=? AND state='sending'", args...)
		return err
	}
	state := "sent"
	if uncertain {
		state = "unknown"
	}
	_, err := s.db.Exec("UPDATE feishu_mirror_receipts SET state=?,platform_id=? WHERE thread=? AND connection=? AND item=? AND part=? AND state='sending'", append([]any{state, receipt}, args...)...)
	return err
}

func (s *Store) ActiveFeishuMirrors(connection string) ([]FeishuMirror, error) {
	rows, err := s.db.Query("SELECT m.thread,m.chat,m.topic,m.root,m.since_ms FROM feishu_mirrors m JOIN sessions s ON s.thread_id=m.thread WHERE s.status='active' AND m.connection=? ORDER BY m.thread", connection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeishuMirror
	for rows.Next() {
		m := FeishuMirror{Address: im.Address{Channel: "feishu", Connection: connection}}
		if err := rows.Scan(&m.Thread, &m.Address.ChatID, &m.Address.TopicID, &m.Address.RootMessageID, &m.Since); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
