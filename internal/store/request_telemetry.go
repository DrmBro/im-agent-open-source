package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"im-agent/internal/telemetry"
)

type TelemetryCandidate struct {
	EventID, ThreadID, AddressKey, TurnRef, Path, UserItemID string
	StateJSON, DisplayJSON                                   string
	Offset, Revision, PresentedRevision                      int64
	Finished                                                 bool
}

func (store *Store) InitRequestTelemetry(ctx context.Context) error {
	_, err := store.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS request_telemetry (
    event_id TEXT PRIMARY KEY REFERENCES events(id), thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
    address_key TEXT NOT NULL, turn_ref TEXT NOT NULL, path TEXT NOT NULL, user_item_id TEXT NOT NULL,
    byte_offset INTEGER NOT NULL, state_json TEXT NOT NULL DEFAULT '{}', display_json TEXT NOT NULL DEFAULT '{}',
    digest TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0, presented_revision INTEGER NOT NULL DEFAULT 0,
    finished INTEGER NOT NULL DEFAULT 0, next_scan INTEGER NOT NULL DEFAULT 0, checked_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL)`)
	return err
}

func (store *Store) RequestTelemetryCandidates(ctx context.Context) ([]TelemetryCandidate, error) {
	cutoff := time.Now().Add(-7 * 24 * time.Hour).Unix()
	_, err := store.db.ExecContext(ctx, `INSERT INTO request_telemetry
    (event_id,thread_id,address_key,turn_ref,path,user_item_id,byte_offset,created_at)
SELECT r.event_id,r.thread_id,r.address_key,r.turn_ref,o.path,o.user_item_id,o.min_offset,r.created_at
FROM request_results r
         JOIN events e ON e.id = r.event_id
         JOIN sessions s ON s.thread_id = r.thread_id
         JOIN live_mirror_origins o ON o.event_id = r.event_id AND o.thread_id = r.thread_id
WHERE s.status = 'active' AND e.chat_key = r.address_key
  AND (r.created_at >= ? OR e.status = 'processing')
  AND o.state = 'matched' AND o.user_item_id <> '' AND o.path <> '' AND o.path = r.observer_path
  AND (SELECT count(*) FROM live_mirror_origins other
       WHERE other.thread_id = o.thread_id AND other.user_item_id = o.user_item_id AND other.state = 'matched') = 1
  AND NOT EXISTS (SELECT 1 FROM request_telemetry metric WHERE metric.event_id = r.event_id)
  AND NOT EXISTS (SELECT 1 FROM history_content_pruned pruned WHERE pruned.kind = 'event' AND pruned.key1 = r.event_id)
ORDER BY r.created_at DESC,r.event_id
LIMIT 16`, cutoff)
	if err != nil {
		return nil, err
	}
	_, err = store.db.ExecContext(ctx, `DELETE FROM request_telemetry
WHERE event_id IN (SELECT metric.event_id FROM request_telemetry metric
                           JOIN events e ON e.id = metric.event_id
                  WHERE metric.created_at < ? AND e.status <> 'processing'
                  ORDER BY metric.created_at,metric.event_id LIMIT 100)`, cutoff)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `SELECT metric.event_id,metric.thread_id,metric.address_key,metric.turn_ref,
       metric.path,metric.user_item_id,metric.byte_offset,metric.state_json,metric.display_json,
       metric.revision,metric.presented_revision,metric.finished
FROM request_telemetry metric
         JOIN request_results r ON r.event_id = metric.event_id AND r.thread_id = metric.thread_id
         JOIN events e ON e.id = metric.event_id
         JOIN sessions s ON s.thread_id = metric.thread_id
         JOIN live_mirror_origins o ON o.event_id = metric.event_id AND o.thread_id = metric.thread_id
WHERE s.status = 'active' AND r.address_key = metric.address_key AND e.chat_key = metric.address_key
  AND r.turn_ref = metric.turn_ref AND o.path = metric.path AND r.observer_path = metric.path
  AND o.state = 'matched' AND o.user_item_id = metric.user_item_id
  AND (SELECT count(*) FROM live_mirror_origins other
       WHERE other.thread_id = o.thread_id AND other.user_item_id = o.user_item_id AND other.state = 'matched') = 1
  AND metric.next_scan <= ?
  AND (metric.finished = 0
       OR (metric.revision > metric.presented_revision
           AND EXISTS (SELECT 1 FROM session_status_cards card
                       WHERE card.thread_id = metric.thread_id AND card.request_id = metric.event_id
                         AND card.address_key = metric.address_key AND card.state = 'sent' AND card.message_id <> '')))
ORDER BY metric.checked_at,metric.created_at,metric.event_id
LIMIT 8`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TelemetryCandidate
	for rows.Next() {
		var item TelemetryCandidate
		if err = rows.Scan(&item.EventID, &item.ThreadID, &item.AddressKey, &item.TurnRef,
			&item.Path, &item.UserItemID, &item.Offset, &item.StateJSON, &item.DisplayJSON,
			&item.Revision, &item.PresentedRevision, &item.Finished); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (store *Store) SaveRequestTelemetry(ctx context.Context, candidate TelemetryCandidate, stateJSON string, snapshot telemetry.Snapshot, next int64, finished bool, delay time.Duration) (bool, error) {
	if next < candidate.Offset || len(stateJSON) > 16384 {
		return false, errors.New("运行元数据进度或快照无效")
	}
	snapshot.ObservedAt = 0
	body, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	digest := Hash(string(body))
	snapshot.ObservedAt = time.Now().UnixMilli()
	display, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	result, err := store.db.ExecContext(ctx, `UPDATE request_telemetry
SET state_json = ?, byte_offset = ?, finished = ?, next_scan = ?, checked_at = ?,
    display_json = CASE WHEN digest <> ? THEN ? ELSE display_json END,
    revision = revision + CASE WHEN digest <> ? THEN 1 ELSE 0 END, digest = ?
WHERE event_id = ? AND thread_id = ? AND address_key = ? AND turn_ref = ?
  AND path = ? AND user_item_id = ? AND byte_offset = ? AND state_json = ? AND revision = ?
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.thread_id = request_telemetry.thread_id AND s.status = 'active')
  AND EXISTS (SELECT 1 FROM request_results r
              WHERE r.event_id = request_telemetry.event_id AND r.address_key = request_telemetry.address_key
                AND r.turn_ref = request_telemetry.turn_ref AND r.observer_path = request_telemetry.path)
  AND EXISTS (SELECT 1 FROM live_mirror_origins o
              WHERE o.event_id = request_telemetry.event_id AND o.thread_id = request_telemetry.thread_id
                AND o.state = 'matched' AND o.user_item_id = request_telemetry.user_item_id AND o.path = request_telemetry.path)
  AND (SELECT count(*) FROM live_mirror_origins other
       WHERE other.thread_id = request_telemetry.thread_id AND other.user_item_id = request_telemetry.user_item_id
         AND other.state = 'matched') = 1`,
		stateJSON, next, finished, time.Now().Add(delay).Unix(), time.Now().Unix(), digest, string(display), digest, digest,
		candidate.EventID, candidate.ThreadID, candidate.AddressKey, candidate.TurnRef, candidate.Path,
		candidate.UserItemID, candidate.Offset, candidate.StateJSON, candidate.Revision)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (store *Store) RequestTelemetry(event, thread, addressKey string) (*telemetry.Report, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var report telemetry.Report
	var raw string
	err := store.db.QueryRowContext(ctx, `SELECT metric.display_json,metric.revision
FROM request_telemetry metric
         JOIN request_results r ON r.event_id = metric.event_id AND r.thread_id = metric.thread_id
         JOIN events e ON e.id = metric.event_id
         JOIN sessions s ON s.thread_id = metric.thread_id
         JOIN live_mirror_origins o ON o.event_id = metric.event_id AND o.thread_id = metric.thread_id
WHERE metric.event_id = ? AND metric.thread_id = ? AND metric.address_key = ?
  AND s.status = 'active' AND r.address_key = metric.address_key AND e.chat_key = metric.address_key
  AND r.turn_ref = metric.turn_ref AND r.observer_path = metric.path AND o.path = metric.path
  AND o.state = 'matched' AND o.user_item_id = metric.user_item_id
  AND (SELECT count(*) FROM live_mirror_origins other
       WHERE other.thread_id = o.thread_id AND other.user_item_id = o.user_item_id AND other.state = 'matched') = 1`,
		event, thread, addressKey).Scan(&raw, &report.Revision)
	if err != nil {
		return nil, err
	}
	if len(raw) > 16384 || json.Unmarshal([]byte(raw), &report.Snapshot) != nil {
		return nil, errors.New("运行元数据展示快照无效")
	}
	return &report, nil
}

func (store *Store) AcknowledgeRequestTelemetry(event, thread, addressKey string, revision int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := store.db.ExecContext(ctx, `UPDATE request_telemetry
SET presented_revision = ?
WHERE event_id = ? AND thread_id = ? AND address_key = ? AND presented_revision < ? AND revision >= ?`,
		revision, event, thread, addressKey, revision, revision)
	return err
}
