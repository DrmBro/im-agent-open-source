package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// 仅使用明确用户/助手消息ID识别摘要来源，不按相同正文合并不同轮次。
type PresentationSource struct {
	UserItemID      string
	AssistantItemID string
}

func (p PresentationSource) key() string {
	if p.UserItemID == "" || p.AssistantItemID == "" || p.UserItemID == p.AssistantItemID {
		return ""
	}
	return Hash(JSON([]string{p.UserItemID, p.AssistantItemID}))
}
func presentationSource(sources []PresentationSource) PresentationSource {
	if len(sources) == 1 {
		return sources[0]
	}
	return PresentationSource{}
}
func (s *Store) migratePresentationRetention() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS history_presentation_clock (
    thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id), source_key TEXT NOT NULL,
    user_item_id TEXT NOT NULL, assistant_item_id TEXT NOT NULL,
    content_hash TEXT NOT NULL, confirmed_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS history_presentation_pruned (
    thread_id TEXT NOT NULL, source_key TEXT NOT NULL, pruned_at INTEGER NOT NULL,
    PRIMARY KEY(thread_id, source_key)
)`)
	return err
}

// 正文hash只复核当前缓存，过期身份与保留时钟绑定来源消息ID。
func recordPresentationClock(tx *sql.Tx, thread, body string, now int64, sources ...PresentationSource) error {
	source := presentationSource(sources)
	if len(sources) == 0 {
		// HistorySent沿用PreparePresentation同事务留下的来源；旧缓存无来源则不猜。
		var hash string
		err := tx.QueryRow("SELECT user_item_id,assistant_item_id,content_hash FROM history_presentation_clock WHERE thread_id=?", thread).Scan(&source.UserItemID, &source.AssistantItemID, &hash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err != nil || hash != Hash(body) {
			source = PresentationSource{}
		}
	}
	_, err := tx.Exec(`INSERT INTO history_presentation_clock
    (thread_id, source_key, user_item_id, assistant_item_id, content_hash, confirmed_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(thread_id) DO UPDATE SET
    confirmed_at = CASE WHEN history_presentation_clock.source_key <> ''
                             AND history_presentation_clock.source_key = excluded.source_key
                             AND history_presentation_clock.confirmed_at > 0
                        THEN history_presentation_clock.confirmed_at ELSE excluded.confirmed_at END,
    source_key = excluded.source_key, user_item_id = excluded.user_item_id,
    assistant_item_id = excluded.assistant_item_id, content_hash = excluded.content_hash`, thread, source.key(), source.UserItemID, source.AssistantItemID, Hash(body), now)
	return err
}

func presentationPruned(tx *sql.Tx, thread string, source PresentationSource) (bool, error) {
	if source.key() == "" {
		return false, nil
	}
	var count int
	err := tx.QueryRow("SELECT count(*) FROM history_presentation_pruned WHERE thread_id=? AND source_key=?", thread, source.key()).Scan(&count)
	return count > 0, err
}
func (s *Store) PresentationSourcePruned(thread string, source PresentationSource) (bool, error) {
	if source.key() == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRow("SELECT count(*) FROM history_presentation_pruned WHERE thread_id=? AND source_key=?", thread, source.key()).Scan(&count)
	return count > 0, err
}

// 只清来源可关联到完整完成且全部送达的旧请求；无来源旧摘要/桌面独立任务不猜终态。
func (s *Store) PrunePresentationHistory(parent context.Context, now time.Time) (HistoryPruneResult, error) {
	result := HistoryPruneResult{CheckedAt: now.Unix()}
	ctx, stop := context.WithTimeout(parent, 3*time.Second)
	defer stop()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, errors.New("无法开始摘要历史维护")
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, historyClockCTE+historyRequestCTEs+`
SELECT p.thread_id, p.history_payload, c.content_hash, c.source_key
FROM session_presentation p
         JOIN history_presentation_clock c ON c.thread_id = p.thread_id
WHERE p.history_state = 'sent' AND p.history_payload <> '' AND c.source_key <> ''
  AND c.confirmed_at > 0 AND c.confirmed_at < (SELECT cutoff FROM retention_clock)
  AND EXISTS (SELECT 1 FROM old_requests r
                       JOIN live_mirror_origins o ON o.event_id = r.event_id AND o.thread_id = r.thread_id
                       JOIN request_result_replies rr ON rr.event_id = r.event_id AND rr.thread_id = r.thread_id
              WHERE r.thread_id = p.thread_id AND o.state = 'matched'
                AND o.user_item_id = c.user_item_id AND rr.item_id = c.assistant_item_id)
  AND NOT EXISTS (SELECT 1 FROM history_presentation_pruned h
                  WHERE h.thread_id = c.thread_id AND h.source_key = c.source_key)
ORDER BY c.confirmed_at, p.thread_id
LIMIT 100`, now.Add(-time.Duration(HistoryRetentionDays)*24*time.Hour).Unix())
	if err != nil {
		return result, errors.New("无法读取可确认过期的摘要")
	}
	type candidate struct{ thread, source string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		var body, digest string
		if err = rows.Scan(&c.thread, &body, &digest, &c.source); err != nil {
			rows.Close()
			return result, errors.New("摘要读取失败")
		}
		if Hash(body) != digest {
			rows.Close()
			return result, errors.New("摘要内容证据变化，未清理")
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, errors.New("摘要读取中断")
	}
	for _, c := range candidates {
		if _, err = tx.ExecContext(ctx, "UPDATE session_presentation SET history_payload='' WHERE thread_id=? AND history_state='sent'", c.thread); err != nil {
			return result, errors.New("摘要清理未完成")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO history_presentation_pruned(thread_id,source_key,pruned_at) VALUES(?,?,?)", c.thread, c.source, now.Unix()); err != nil {
			return result, errors.New("摘要过期来源未保存")
		}
		result.Pruned++
	}
	if err = tx.Commit(); err != nil {
		return HistoryPruneResult{}, errors.New("摘要维护未提交")
	}
	result.More = len(candidates) == 100
	return result, nil
}
