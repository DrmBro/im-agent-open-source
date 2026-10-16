package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type OriginCandidate struct {
	EventID, ThreadID, AddressKey, Path, OriginalHash, RenderedHash string
	Offset, MinOffset                                               int64
}

func (store *Store) PendingRequestOrigins(ctx context.Context) ([]OriginCandidate, error) {
	_, err := store.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS request_origin_observers (
    event_id TEXT PRIMARY KEY REFERENCES events(id), path TEXT NOT NULL, byte_offset INTEGER NOT NULL,
    next_scan INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL DEFAULT 0,
    error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()
	_, err = store.db.ExecContext(ctx, `INSERT INTO request_origin_observers(event_id,path,byte_offset,created_at)
SELECT o.event_id,o.path,o.min_offset,e.created_at
FROM live_mirror_origins o
         JOIN events e ON e.id = o.event_id
         JOIN request_results r ON r.event_id = o.event_id AND r.thread_id = o.thread_id
         JOIN sessions s ON s.thread_id = o.thread_id
WHERE o.state = 'pending' AND o.path <> '' AND s.status = 'active'
  AND e.chat_key = r.address_key AND r.observer_path = o.path
  AND e.status IN ('processing','uncertain','interrupted')
  AND (e.created_at >= ? OR e.status = 'processing')
  AND NOT EXISTS (SELECT 1 FROM request_origin_observers observer WHERE observer.event_id = o.event_id)
ORDER BY e.created_at DESC,o.event_id LIMIT 16`, cutoff)
	if err != nil {
		return nil, err
	}
	_, err = store.db.ExecContext(ctx, `DELETE FROM request_origin_observers
WHERE event_id IN (SELECT observer.event_id FROM request_origin_observers observer
                           JOIN events e ON e.id = observer.event_id
                  WHERE observer.created_at < ? AND e.status <> 'processing'
                  ORDER BY observer.created_at,observer.event_id LIMIT 100)`, cutoff)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT o.event_id,o.thread_id,r.address_key,o.path,
       o.original_hash,o.rendered_hash,observer.byte_offset,o.min_offset
FROM request_origin_observers observer
         JOIN live_mirror_origins o ON o.event_id = observer.event_id AND o.path = observer.path
         JOIN request_results r ON r.event_id = o.event_id AND r.thread_id = o.thread_id
         JOIN events e ON e.id = o.event_id
         JOIN sessions s ON s.thread_id = o.thread_id
WHERE o.state = 'pending' AND s.status = 'active' AND e.chat_key = r.address_key
  AND r.observer_path = o.path AND e.status IN ('processing','uncertain','interrupted')
  AND observer.next_scan <= ?
ORDER BY observer.checked_at,observer.created_at,observer.event_id LIMIT 8`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []OriginCandidate
	for rows.Next() {
		var candidate OriginCandidate
		if err = rows.Scan(&candidate.EventID, &candidate.ThreadID, &candidate.AddressKey, &candidate.Path,
			&candidate.OriginalHash, &candidate.RenderedHash, &candidate.Offset, &candidate.MinOffset); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (store *Store) SaveOriginProgress(ctx context.Context, candidate OriginCandidate, next int64, failed bool) error {
	if next < candidate.Offset {
		return errors.New("请求来源观察不能回退")
	}
	delay, hint := 5*time.Second, ""
	if failed {
		delay, hint = time.Minute, "请求独立来源暂无法确认，未移动历史镜像游标或重放请求"
	}
	_, err := store.db.ExecContext(ctx, `UPDATE request_origin_observers
SET byte_offset = ?, next_scan = ?, checked_at = ?, error = ?
WHERE event_id = ? AND path = ? AND byte_offset = ?`, next, time.Now().Add(delay).Unix(), time.Now().Unix(), hint,
		candidate.EventID, candidate.Path, candidate.Offset)
	return err
}

func (store *Store) MatchRequestOrigin(ctx context.Context, session Session, candidate OriginCandidate, entry MirrorEntry) (bool, error) {
	if entry.Role != "user" || entry.ItemID == "" || entry.Offset < candidate.MinOffset ||
		(entry.Fingerprint != candidate.OriginalHash && entry.Fingerprint != candidate.RenderedHash) ||
		session.ThreadID != candidate.ThreadID || Hash(session.Address.Key()) != candidate.AddressKey {
		return false, errors.New("请求来源证据不一致")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var valid int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM live_mirror_origins o
         JOIN request_results r ON r.event_id = o.event_id AND r.thread_id = o.thread_id
         JOIN events e ON e.id = o.event_id
         JOIN sessions s ON s.thread_id = o.thread_id
WHERE o.event_id = ? AND o.thread_id = ? AND o.path = ? AND o.min_offset = ?
  AND o.original_hash = ? AND o.rendered_hash = ? AND o.state = 'pending'
  AND r.observer_path = o.path AND r.address_key = ? AND e.chat_key = r.address_key
  AND e.status IN ('processing','uncertain','interrupted') AND s.status = 'active'
  AND s.channel = ? AND s.connection_id = ? AND s.chat_id = ? AND s.topic_id = ?`,
		candidate.EventID, candidate.ThreadID, candidate.Path, candidate.MinOffset, candidate.OriginalHash, candidate.RenderedHash,
		candidate.AddressKey, session.Address.Channel, session.Address.Connection, session.Address.ChatID, session.Address.TopicID).Scan(&valid)
	if err != nil || valid != 1 {
		return false, err
	}
	var matches, owned int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM live_mirror_origins
WHERE thread_id = ? AND path = ? AND min_offset <= ? AND state = 'pending'
  AND (original_hash = ? OR rendered_hash = ?)`, candidate.ThreadID, candidate.Path, entry.Offset, entry.Fingerprint, entry.Fingerprint).Scan(&matches)
	if err != nil {
		return false, err
	}
	if matches != 1 {
		return false, errors.New("多个同文来源尚未关联，不能猜测请求身份")
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM live_mirror_origins
WHERE thread_id = ? AND user_item_id = ? AND event_id <> ? AND state = 'matched'`,
		candidate.ThreadID, entry.ItemID, candidate.EventID).Scan(&owned)
	if err != nil {
		return false, err
	}
	if owned != 0 {
		return false, errors.New("原始用户消息已有其它请求归属")
	}
	var role string
	var suppressed int
	err = tx.QueryRowContext(ctx, "SELECT role,suppressed FROM live_mirror_entries WHERE thread_id=? AND item_id=?", candidate.ThreadID, entry.ItemID).Scan(&role, &suppressed)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, "INSERT INTO live_mirror_entries(thread_id,item_id,role,suppressed) VALUES (?,?,'user',1)", candidate.ThreadID, entry.ItemID)
	} else if err == nil && (role != "user" || suppressed != 1) {
		return false, errors.New("该用户消息已有独立镜像记录，未覆盖投递账本")
	}
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE live_mirror_origins SET state='matched',user_item_id=? WHERE event_id=? AND state='pending'", entry.ItemID, candidate.EventID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return false, errors.New("请求来源状态已变化")
	}
	return true, tx.Commit()
}
