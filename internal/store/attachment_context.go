package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"im-agent/internal/im"
	"time"
)

func (s *Store) migrateAttachmentContext() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS attachment_message_contexts (
 event_id TEXT PRIMARY KEY REFERENCES events(id),thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
 token TEXT NOT NULL,display_text TEXT NOT NULL,created_at INTEGER NOT NULL
 );`)
	return err
}
func (s *Store) EnsureAttachmentContext(thread, event, display string) (string, error) {
	if thread == "" || event == "" || display == "" {
		return "", errors.New("附件上下文身份或展示内容无效")
	}
	_, err := s.db.Exec(`INSERT INTO attachment_message_contexts(event_id,thread_id,token,display_text,created_at)
VALUES (?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, event, thread, ID(), display, time.Now().Unix())
	if err != nil {
		return "", err
	}
	var savedThread, token, savedDisplay string
	err = s.db.QueryRow("SELECT thread_id,token,display_text FROM attachment_message_contexts WHERE event_id=?", event).Scan(&savedThread, &token, &savedDisplay)
	if err != nil {
		return "", err
	}
	if savedThread != thread || savedDisplay != display {
		return "", errors.New("附件上下文请求或用户原文已有冲突，不覆盖")
	}
	return token, nil
}
func (s *Store) AttachmentContextDisplay(thread, event, token string) (string, bool, error) {
	var display string
	err := s.db.QueryRow("SELECT display_text FROM attachment_message_contexts WHERE thread_id=? AND event_id=? AND token=?", thread, event, token).Scan(&display)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return display, err == nil, err
}

func (s *Store) PruneAttachmentDisplay(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE attachment_message_contexts SET display_text='（附件任务展示内容已按7天保留策略清理）'
WHERE event_id IN (SELECT a.event_id FROM attachment_message_contexts a JOIN events e ON e.id=a.event_id
                  WHERE a.created_at<? AND e.status IN ('done','failed','cancelled','expired','bundled')
                    AND a.display_text<>'（附件任务展示内容已按7天保留策略清理）'
                  ORDER BY a.created_at,a.event_id LIMIT 100)`, now.Add(-HistoryRetentionDays*24*time.Hour).Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// 旧版桥接生成的附件提示只能凭已保存的完整来源指纹还原，不能按段落相似度删除。
func (s *Store) LegacyAttachmentDisplay(thread, fingerprint string) (string, bool, error) {
	rows, err := s.db.Query(`SELECT i.payload FROM live_mirror_origins o
         JOIN inbound_messages i ON i.event_id=o.event_id AND i.thread_id=o.thread_id
WHERE o.thread_id=? AND (o.original_hash=? OR o.rendered_hash=?)`, thread, fingerprint, fingerprint)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var raw string
	count := 0
	for rows.Next() {
		count++
		if err = rows.Scan(&raw); err != nil {
			return "", false, err
		}
	}
	if err = rows.Err(); err != nil {
		return "", false, err
	}
	if count != 1 {
		return "", false, nil
	}
	var msg im.Message
	if json.Unmarshal([]byte(raw), &msg) != nil || len(msg.Attachments) == 0 {
		return "", false, nil
	}
	display := msg.Text
	for i, a := range msg.Attachments {
		display += fmt.Sprintf("\n[附件 %d：%s]", i+1, a.Name)
	}
	return display, true, nil
}
