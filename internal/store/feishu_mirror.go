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

// 反查飞书入口时保留其发送者和群地址；钉钉主绑定只提供会话身份、工作目录与状态。
func (s *Store) FeishuInputSession(a im.Address) (Session, error) {
	var v Session
	if a.Channel != "feishu" {
		return v, sql.ErrNoRows
	}
	err := s.db.QueryRow(`SELECT s.thread_id,s.work_dir,s.status,max(s.created_at,m.since_ms/1000) FROM feishu_mirrors m JOIN sessions s ON s.thread_id=m.thread
	WHERE m.connection=? AND m.chat=? AND m.topic=?`, a.Connection, a.ChatID, a.TopicID).Scan(&v.ThreadID, &v.WorkDir, &v.Status, &v.CreatedAt)
	v.Address = a
	return v, err
}

// 与长连接和分页补收共用同一个平台 ID。领取和绑定会话一次提交。
func (s *Store) QueueFeishuInput(v Session, msg im.Message) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	claimed, err := claimInboundTx(tx, msg)
	if err != nil || !claimed {
		return err
	}
	if err = queueInboundTx(tx, v, msg, msg.Address, nil); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) FinishFeishuInput(event, stage, status, detail string) error {
	_, err := s.db.Exec("UPDATE events SET status=?,stage=?,error_summary=?,updated_at=unixepoch() WHERE id=? AND status='processing'", status, stage, detail, event)
	return err
}

// 原群已有用户输入，只关联已提交后出现的唯一同文条目；回复仍正常同步。
func (s *Store) IsFeishuInputEcho(m FeishuMirror, item, path, hash string, offset int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	const scope = `thread_id=? AND path=? AND EXISTS(SELECT 1 FROM inbound_messages i WHERE i.event_id=live_mirror_origins.event_id AND i.address_key=?)`
	args := []any{m.Thread, path, Hash(m.Address.Key())}
	var n int
	if err = tx.QueryRow("SELECT count(*) FROM live_mirror_origins WHERE "+scope+" AND state='matched' AND user_item_id=?", append(args, item)...).Scan(&n); err != nil || n > 0 {
		return n > 0, err
	}
	const pending = " AND state='pending' AND min_offset<=? AND (original_hash=? OR rendered_hash=?)"
	args = append(args, offset, hash, hash)
	if err = tx.QueryRow("SELECT count(*) FROM live_mirror_origins WHERE "+scope+pending, args...).Scan(&n); err != nil || n != 1 {
		return false, err
	}
	if _, err = tx.Exec("UPDATE live_mirror_origins SET state='matched',user_item_id=? WHERE "+scope+pending, append([]any{item}, args...)...); err != nil {
		return false, err
	}
	return true, tx.Commit()
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
