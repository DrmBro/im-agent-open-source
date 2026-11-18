package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 同步诊断只返回进度和回执统计，不返回消息正文、命令、凭据或本机日志路径。
type SessionSyncHealth struct {
	ThreadID      string `json:"thread_id"`
	Name          string `json:"name"`
	ReadAt        int64  `json:"read_at"`
	DeliveryAt    int64  `json:"delivery_at"`
	ReadError     string `json:"read_error"`
	DeliveryError string `json:"delivery_error"`
	RecoveryError string `json:"recovery_error"`
	UnreadBytes   int64  `json:"unread_bytes"`
	ReadOffset    int64  `json:"read_offset"`
	SourceBytes   int64  `json:"source_bytes"`
	Messages      int    `json:"messages"`
	SentMessages  int    `json:"sent_messages"`
	Pending       int    `json:"pending"`
	Sending       int    `json:"sending"`
	Unknown       int    `json:"unknown"`
	RetryAt       int64  `json:"retry_at"`
	ReadIssues    int    `json:"read_issues"`
	HistoryState  string `json:"history_state"`
	HistoryError  string `json:"history_error"`
}

func (s *Store) migrateSyncHealth() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS session_sync_health (
 thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id),
 read_checked_at INTEGER NOT NULL DEFAULT 0,delivery_checked_at INTEGER NOT NULL DEFAULT 0,recovery_checked_at INTEGER NOT NULL DEFAULT 0,
 read_error TEXT NOT NULL DEFAULT '',delivery_error TEXT NOT NULL DEFAULT '',recovery_error TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS session_sync_sources (
 thread_id TEXT NOT NULL REFERENCES sessions(thread_id),path TEXT NOT NULL,observed_size INTEGER NOT NULL,observed_at INTEGER NOT NULL,
 PRIMARY KEY(thread_id,path));
 CREATE TABLE IF NOT EXISTS live_mirror_checkpoints(
 thread_id TEXT NOT NULL,path TEXT NOT NULL,byte_offset INTEGER NOT NULL,fingerprint TEXT NOT NULL,
 PRIMARY KEY(thread_id,path));`)
	if err != nil {
		return err
	}
	has, err := hasColumn(s.db, "live_mirror_parts", "attempts")
	if err != nil {
		return err
	}
	if !has {
		_, err = s.db.Exec(`ALTER TABLE live_mirror_parts ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0`)
	}
	return err
}

func (s *Store) RecordSyncSource(thread, path string, size int64) error {
	if size < 0 {
		return errors.New("日志长度无效")
	}
	_, err := s.db.Exec(`INSERT INTO session_sync_sources(thread_id,path,observed_size,observed_at) VALUES(?,?,?,?)
 ON CONFLICT(thread_id,path) DO UPDATE SET observed_size=excluded.observed_size,observed_at=excluded.observed_at`, thread, path, size, time.Now().Unix())
	return err
}

func (s *Store) RecordSyncStage(thread, stage, detail string) error {
	switch stage {
	case "read", "delivery", "recovery":
	default:
		return errors.New("同步阶段无效")
	}
	q := fmt.Sprintf(`INSERT INTO session_sync_health(thread_id,%s_checked_at,%s_error) VALUES(?,?,?)
 ON CONFLICT(thread_id) DO UPDATE SET %s_checked_at=excluded.%s_checked_at,%s_error=excluded.%s_error`, stage, stage, stage, stage, stage, stage)
	_, err := s.db.Exec(q, thread, time.Now().Unix(), detail)
	return err
}

func (s *Store) SyncHealth() ([]SessionSyncHealth, error) {
	rows, err := s.db.Query(`SELECT s.thread_id,coalesce(p.target_name,''),coalesce(h.read_checked_at,0),coalesce(h.delivery_checked_at,0),
 coalesce(h.read_error,''),coalesce(h.delivery_error,''),coalesce(h.recovery_error,''),
 coalesce((SELECT sum(max(0,r.observed_size-coalesce(f.byte_offset,0))) FROM session_sync_sources r LEFT JOIN live_mirror_file_cursors f ON f.thread_id=r.thread_id AND f.path=r.path WHERE r.thread_id=s.thread_id),0),
 coalesce((SELECT sum(byte_offset) FROM live_mirror_file_cursors WHERE thread_id=s.thread_id),0),
 coalesce((SELECT sum(observed_size) FROM session_sync_sources WHERE thread_id=s.thread_id),0),
 (SELECT count(*) FROM live_mirror_entries e WHERE e.thread_id=s.thread_id AND e.suppressed=0),
 (SELECT count(*) FROM live_mirror_entries e WHERE e.thread_id=s.thread_id AND e.suppressed=0 AND EXISTS(SELECT 1 FROM live_mirror_parts x WHERE x.thread_id=e.thread_id AND x.item_id=e.item_id) AND NOT EXISTS(SELECT 1 FROM live_mirror_parts x WHERE x.thread_id=e.thread_id AND x.item_id=e.item_id AND x.state<>'sent')),
 (SELECT count(*) FROM live_mirror_parts p WHERE p.thread_id=s.thread_id AND p.state='pending' AND (s.channel<>'dingtalk_personal' OR EXISTS(SELECT 1 FROM personal_reviews pr WHERE pr.thread_id=s.thread_id AND pr.delivery_item=p.item_id AND pr.state IN ('approved','sending','unknown')))),
 (SELECT count(*) FROM live_mirror_parts p WHERE p.thread_id=s.thread_id AND p.state='sending' AND (s.channel<>'dingtalk_personal' OR EXISTS(SELECT 1 FROM personal_reviews pr WHERE pr.thread_id=s.thread_id AND pr.delivery_item=p.item_id AND pr.state IN ('approved','sending','unknown')))),
 (SELECT count(*) FROM live_mirror_parts p WHERE p.thread_id=s.thread_id AND p.state='unknown' AND (s.channel<>'dingtalk_personal' OR EXISTS(SELECT 1 FROM personal_reviews pr WHERE pr.thread_id=s.thread_id AND pr.delivery_item=p.item_id AND pr.state IN ('approved','sending','unknown')))),
 coalesce((SELECT min(next_attempt) FROM live_mirror_parts WHERE thread_id=s.thread_id AND state='pending' AND next_attempt>?),0),
 (SELECT count(*) FROM live_mirror_read_issues WHERE thread_id=s.thread_id),coalesce(p.history_state,''),coalesce(p.history_error,'')
 FROM sessions s LEFT JOIN session_sync_health h ON h.thread_id=s.thread_id LEFT JOIN session_presentation p ON p.thread_id=s.thread_id
 WHERE s.status='active' AND (s.managed_chat=1 OR s.channel IN ('dingtalk','dingtalk_personal')) ORDER BY s.created_at,s.thread_id`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionSyncHealth{}
	for rows.Next() {
		var v SessionSyncHealth
		if err = rows.Scan(&v.ThreadID, &v.Name, &v.ReadAt, &v.DeliveryAt, &v.ReadError, &v.DeliveryError, &v.RecoveryError, &v.UnreadBytes, &v.ReadOffset, &v.SourceBytes, &v.Messages, &v.SentMessages, &v.Pending, &v.Sending, &v.Unknown, &v.RetryAt, &v.ReadIssues, &v.HistoryState, &v.HistoryError); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// 后台调度只遍历活跃自建会话，不让归档历史或前 100 条展示记录挤占同步机会。
func (s *Store) ActivePresentations() ([]Presentation, error) {
	fields, err := s.presentationColumns()
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT "+fields+" FROM session_presentation p JOIN sessions s ON s.thread_id=p.thread_id WHERE s.status='active' AND s.managed_chat=1 AND p.next_attempt<=? ORDER BY p.updated_at,p.thread_id", time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Presentation
	for rows.Next() {
		p, err := scanPresentation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) MirrorCheckpoint(thread, path string, offset int64) (string, error) {
	var fingerprint string
	err := s.db.QueryRow("SELECT fingerprint FROM live_mirror_checkpoints WHERE thread_id=? AND path=? AND byte_offset=?", thread, path, offset).Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fingerprint, err
}
