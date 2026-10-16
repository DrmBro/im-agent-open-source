package store

import (
	"database/sql"
	"errors"
	"time"
)

// 消息身份为 Codex 提供的 thread_id + item_id；不是正文去重。
type MirrorCursor struct {
	Path   string
	Offset int64
}
type MirrorEntry struct {
	ItemID, Role, Fingerprint string
	Offset                    int64
	Parts                     []string
	// 有多个来源候选时的中立展示，不把正文相同等同于请求身份。
	OriginUnknownParts []string
}

type MirrorReadIssue struct {
	Offset, Length    int64
	Kind, Fingerprint string
}
type MirrorPart struct {
	ThreadID, ItemID, Role, Body, State, Error string
	Index                                      int
	NextAttempt                                int64
}

func (s *Store) migrateLiveMirror() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS live_mirror_cursors (
 thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id), path TEXT NOT NULL,
 byte_offset INTEGER NOT NULL, updated_at INTEGER NOT NULL
 );
 CREATE TABLE IF NOT EXISTS live_mirror_file_cursors (
 thread_id TEXT NOT NULL REFERENCES sessions(thread_id), path TEXT NOT NULL,
 byte_offset INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(thread_id,path)
 );
 CREATE TABLE IF NOT EXISTS live_mirror_origins (
 event_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
 path TEXT NOT NULL, min_offset INTEGER NOT NULL, original_hash TEXT NOT NULL,
 rendered_hash TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending', user_item_id TEXT NOT NULL DEFAULT ''
 );
 CREATE INDEX IF NOT EXISTS live_mirror_origins_thread ON live_mirror_origins(thread_id, state);
 CREATE TABLE IF NOT EXISTS live_mirror_entries (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
 item_id TEXT NOT NULL, role TEXT NOT NULL, suppressed INTEGER NOT NULL DEFAULT 0,
 UNIQUE(thread_id, item_id)
 );
 CREATE TABLE IF NOT EXISTS live_mirror_parts (
 thread_id TEXT NOT NULL, item_id TEXT NOT NULL, part_index INTEGER NOT NULL,
 body TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending', platform_id TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '', next_attempt INTEGER NOT NULL DEFAULT 0,
 updated_at INTEGER NOT NULL, PRIMARY KEY(thread_id, item_id, part_index),
 FOREIGN KEY(thread_id, item_id) REFERENCES live_mirror_entries(thread_id, item_id)
 );
 CREATE TABLE IF NOT EXISTS live_mirror_origin_issues (
 thread_id TEXT NOT NULL REFERENCES sessions(thread_id), item_id TEXT NOT NULL,
 path TEXT NOT NULL, byte_offset INTEGER NOT NULL, candidate_count INTEGER NOT NULL CHECK(candidate_count > 1),
 created_at INTEGER NOT NULL, PRIMARY KEY(thread_id,item_id),
 FOREIGN KEY(thread_id,item_id) REFERENCES live_mirror_entries(thread_id,item_id)
 );
 CREATE TABLE IF NOT EXISTS live_mirror_read_issues (
 thread_id TEXT NOT NULL REFERENCES sessions(thread_id), path TEXT NOT NULL,
 byte_offset INTEGER NOT NULL, byte_length INTEGER NOT NULL,
 kind TEXT NOT NULL, fingerprint TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY(thread_id, path, byte_offset)
 );`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO live_mirror_file_cursors(thread_id,path,byte_offset,updated_at)
SELECT thread_id,path,byte_offset,updated_at FROM live_mirror_cursors WHERE 1=1
ON CONFLICT(thread_id,path) DO NOTHING`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE live_mirror_parts SET state='unknown', error='上次发送中断，可能已送达；需核对后处理，不自动重发' WHERE state='sending'")
	return err
}

func (s *Store) MirrorCursor(thread string) (MirrorCursor, error) {
	var c MirrorCursor
	err := s.db.QueryRow("SELECT path, byte_offset FROM live_mirror_cursors WHERE thread_id=?", thread).Scan(&c.Path, &c.Offset)
	return c, err
}
func (s *Store) InitMirrorCursor(thread, path string, offset int64) error {
	_, err := s.db.Exec("INSERT INTO live_mirror_cursors (thread_id,path,byte_offset,updated_at) VALUES (?,?,?,?) ON CONFLICT(thread_id) DO NOTHING", thread, path, offset, time.Now().Unix())
	return err
}

// 新文件从已核验的元数据末尾开始；已有路径只使用持久进度。
func (s *Store) MirrorFileCursor(thread, path string, initial int64) (MirrorCursor, error) {
	var c MirrorCursor
	if initial <= 0 {
		return c, errors.New("会话日志初始位置无效")
	}
	_, err := s.db.Exec(`INSERT INTO live_mirror_file_cursors(thread_id,path,byte_offset,updated_at)
VALUES (?,?,?,?) ON CONFLICT(thread_id,path) DO NOTHING`, thread, path, initial, time.Now().Unix())
	if err != nil {
		return c, err
	}
	c.Path = path
	err = s.db.QueryRow("SELECT byte_offset FROM live_mirror_file_cursors WHERE thread_id=? AND path=?", thread, path).Scan(&c.Offset)
	return c, err
}

func (s *Store) RegisterMirrorOrigin(thread, event, path string, offset int64, original, rendered string) error {
	_, err := s.db.Exec(`INSERT INTO live_mirror_origins (event_id,thread_id,path,min_offset,original_hash,rendered_hash)
 VALUES (?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, event, thread, path, offset, original, rendered)
	return err
}
func (s *Store) CancelMirrorOrigin(event string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE live_mirror_origins SET state='cancelled' WHERE event_id=? AND state='pending'", event); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE request_results SET execution_state='not_sent',delivery_state='not_applicable',updated_at=?
WHERE event_id=? AND execution_state='pending_submission' AND sealed=0`, time.Now().Unix(), event); err != nil {
		return err
	}
	return tx.Commit()
}

func prepareMirrorEntry(tx *sql.Tx, thread, path string, e MirrorEntry) error {
	if e.ItemID == "" || (e.Role != "user" && e.Role != "assistant") {
		return errors.New("实时消息缺少有效身份")
	}
	var exists int
	if err := tx.QueryRow("SELECT count(*) FROM live_mirror_entries WHERE thread_id=? AND item_id=?", thread, e.ItemID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	suppressed := false
	candidates := 0
	parts := e.Parts
	if e.Role == "user" {
		rows, err := tx.Query(`SELECT event_id FROM live_mirror_origins
 WHERE thread_id=? AND path=? AND min_offset<=? AND state='pending'
   AND (original_hash=? OR rendered_hash=?)`, thread, path, e.Offset, e.Fingerprint, e.Fingerprint)
		if err != nil {
			return err
		}
		var matches []string
		for rows.Next() {
			var event string
			if err = rows.Scan(&event); err != nil {
				rows.Close()
				return err
			}
			matches = append(matches, event)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(matches) > 1 {
			// 不把同文用户条目归给任何请求；只隔离归属判断，不阻断后续日志同步。
			candidates = len(matches)
			if len(e.OriginUnknownParts) == 0 {
				return errors.New("来源不明确的消息缺少独立展示内容，未伪造来源")
			}
			parts = e.OriginUnknownParts
		}
		if len(matches) == 1 {
			if _, err = tx.Exec("UPDATE live_mirror_origins SET state='matched', user_item_id=? WHERE event_id=? AND state='pending'", e.ItemID, matches[0]); err != nil {
				return err
			}
			suppressed = true
		}
	}
	if _, err := tx.Exec("INSERT INTO live_mirror_entries (thread_id,item_id,role,suppressed) VALUES (?,?,?,?)", thread, e.ItemID, e.Role, suppressed); err != nil {
		return err
	}
	if suppressed {
		return nil
	}
	if candidates > 1 {
		if _, err := tx.Exec(`INSERT INTO live_mirror_origin_issues
(thread_id,item_id,path,byte_offset,candidate_count,created_at)
VALUES (?,?,?,?,?,?)`, thread, e.ItemID, path, e.Offset, candidates, time.Now().Unix()); err != nil {
			return err
		}
	}
	if len(parts) == 0 {
		return errors.New("实时消息没有可发送正文")
	}
	for i, part := range parts {
		if _, err := tx.Exec("INSERT INTO live_mirror_parts (thread_id,item_id,part_index,body,updated_at) VALUES (?,?,?,?,?)", thread, e.ItemID, i, part, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// 新增消息与消费位置同一事务提交，避免推进游标后丢掉消息。
func (s *Store) CommitMirrorBatch(thread string, c MirrorCursor, next int64, entries []MirrorEntry, issues []MirrorReadIssue) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range entries {
		if err = prepareMirrorEntry(tx, thread, c.Path, e); err != nil {
			return err
		}
	}
	for _, issue := range issues {
		if issue.Offset < 0 || issue.Length <= 0 || issue.Offset+issue.Length > next || issue.Fingerprint == "" {
			return errors.New("日志异常记录位置无效")
		}
		if _, err = tx.Exec(`INSERT INTO live_mirror_read_issues
(thread_id,path,byte_offset,byte_length,kind,fingerprint,created_at)
VALUES (?,?,?,?,?,?,?) ON CONFLICT(thread_id,path,byte_offset) DO NOTHING`,
			thread, c.Path, issue.Offset, issue.Length, issue.Kind, issue.Fingerprint, time.Now().Unix()); err != nil {
			return err
		}
	}
	result, err := tx.Exec("UPDATE live_mirror_file_cursors SET byte_offset=?,updated_at=? WHERE thread_id=? AND path=? AND byte_offset=?", next, time.Now().Unix(), thread, c.Path, c.Offset)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("实时消息消费位置发生变化，未提交部分结果")
	}
	// 旧游标只作为最近已处理文件的可见进度；逐文件账本才是扫描权威。
	if _, err = tx.Exec("UPDATE live_mirror_cursors SET path=?,byte_offset=?,updated_at=? WHERE thread_id=?", c.Path, next, time.Now().Unix(), thread); err != nil {
		return err
	}
	return tx.Commit()
}

// 歧义条目已独立展示，原请求归属仍待核对，不等于整条会话停止同步。
func (s *Store) MirrorOriginIssueCount(thread string) (int, error) {
	var count int
	err := s.db.QueryRow("SELECT count(*) FROM live_mirror_origin_issues WHERE thread_id=?", thread).Scan(&count)
	return count, err
}

func (s *Store) MirrorReadIssueCount(thread string) (int, error) {
	var count int
	err := s.db.QueryRow("SELECT count(*) FROM live_mirror_read_issues WHERE thread_id=?", thread).Scan(&count)
	return count, err
}

func (s *Store) EnqueueMirrorReply(thread string, e MirrorEntry) error {
	if e.Role != "assistant" {
		return errors.New("回复出口只接受助手消息")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = prepareMirrorEntry(tx, thread, "", e); err != nil {
		return err
	}
	return tx.Commit()
}

// 未确认送达与暂未到重试时间的片段均不占用批次，不阻塞后续独立消息。
// 同一消息内仍按片段顺序发送；先前片段未确认时不跳到后续片段。
func (s *Store) MirrorParts(thread string) ([]MirrorPart, error) {
	rows, err := s.db.Query(`SELECT p.item_id, p.part_index, p.body, p.state, p.error, p.next_attempt, e.role
FROM live_mirror_parts p
         JOIN live_mirror_entries e ON e.thread_id = p.thread_id AND e.item_id = p.item_id
WHERE p.thread_id = ?
  AND p.state = 'pending' AND p.next_attempt <= ?
  AND NOT EXISTS (SELECT 1 FROM live_mirror_parts earlier
                  WHERE earlier.thread_id = p.thread_id AND earlier.item_id = p.item_id
                    AND earlier.part_index < p.part_index AND earlier.state NOT IN ('sent', 'unknown'))
ORDER BY e.seq, p.part_index
LIMIT 30`, thread, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MirrorPart
	for rows.Next() {
		p := MirrorPart{ThreadID: thread}
		if err = rows.Scan(&p.ItemID, &p.Index, &p.Body, &p.State, &p.Error, &p.NextAttempt, &p.Role); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) ClaimMirrorPart(p MirrorPart) (bool, error) {
	r, err := s.db.Exec(`UPDATE live_mirror_parts SET state='sending',updated_at=?
 WHERE thread_id=? AND item_id=? AND part_index=? AND state='pending' AND next_attempt<=?`, time.Now().Unix(), p.ThreadID, p.ItemID, p.Index, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
func (s *Store) MirrorPartSent(p MirrorPart, platformID string) error {
	if platformID == "" {
		return errors.New("缺少平台消息回执")
	}
	_, err := s.db.Exec(`UPDATE live_mirror_parts SET state='sent',platform_id=?,error='',updated_at=?
 WHERE thread_id=? AND item_id=? AND part_index=? AND state='sending'`, platformID, time.Now().Unix(), p.ThreadID, p.ItemID, p.Index)
	return err
}
func (s *Store) MirrorPartFailed(p MirrorPart, hint string, unknown bool) error {
	state := "pending"
	if unknown {
		state = "unknown"
	}
	_, err := s.db.Exec(`UPDATE live_mirror_parts SET state=?,error=?,next_attempt=?,updated_at=?
 WHERE thread_id=? AND item_id=? AND part_index=? AND state='sending'`, state, hint, time.Now().Unix()+30, time.Now().Unix(), p.ThreadID, p.ItemID, p.Index)
	return err
}
func (s *Store) MirrorReplySent(thread, item string) (bool, error) {
	var total, remaining int
	err := s.db.QueryRow("SELECT count(*),coalesce(sum(CASE WHEN state='sent' THEN 0 ELSE 1 END),0) FROM live_mirror_parts WHERE thread_id=? AND item_id=?", thread, item).Scan(&total, &remaining)
	return total > 0 && remaining == 0, err
}
func (s *Store) LiveMirrorSessions() ([]Session, error) {
	rows, err := s.db.Query("SELECT " + columns + " FROM sessions WHERE status='active' AND managed_chat=1 ORDER BY created_at,thread_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var v Session
		if err = rows.Scan(&v.ThreadID, &v.Address.Channel, &v.Address.Connection, &v.Address.ChatID, &v.WorkDir, &v.Owner, &v.Status, &v.CreatedAt, &v.Managed, &v.Address.TopicID, &v.Address.RootMessageID); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
