package store

import (
	"errors"
	"strconv"
	"time"
)

type InboundHistoryCursor struct {
	ThreadID, AddressKey, Token string
	BaselineMS, ThroughMS       int64
	FromMS, ToMS                int64
}
type InboundBackfill struct {
	EventID   string
	CreatedMS int64
}

func (s *Store) migrateInboundHistory() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbound_history_cursors (
 thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id), address_key TEXT NOT NULL,
 baseline_ms INTEGER NOT NULL, through_ms INTEGER NOT NULL,
 from_ms INTEGER NOT NULL DEFAULT 0, to_ms INTEGER NOT NULL DEFAULT 0,
 page_token TEXT NOT NULL DEFAULT '', checked_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', recovered INTEGER NOT NULL DEFAULT 0
 );
 CREATE TABLE IF NOT EXISTS inbound_history_receipts (
 thread_id TEXT NOT NULL REFERENCES sessions(thread_id), event_id TEXT NOT NULL,
 disposition TEXT NOT NULL, created_ms INTEGER NOT NULL, recorded_at INTEGER NOT NULL,
 PRIMARY KEY(thread_id,event_id)
 );
 CREATE TABLE IF NOT EXISTS inbound_history_backfills (
 event_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
 address_key TEXT NOT NULL, created_ms INTEGER NOT NULL, approved_at INTEGER NOT NULL
 );`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO meta(key,value) VALUES ('inbound-history-enabled-at',?) ON CONFLICT(key) DO NOTHING", strconv.FormatInt(time.Now().UnixMilli(), 10))
	return err
}

func (s *Store) InboundHistoryCursor(session Session) (InboundHistoryCursor, error) {
	var v InboundHistoryCursor
	raw, err := s.GetMeta("inbound-history-enabled-at")
	if err != nil {
		return v, err
	}
	start, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || start <= 0 {
		return v, errors.New("收件对账基线无效")
	}
	if session.CreatedAt*1000 > start {
		start = session.CreatedAt * 1000
	}
	key := Hash(session.Address.Key())
	_, err = s.db.Exec(`INSERT INTO inbound_history_cursors(thread_id,address_key,baseline_ms,through_ms)
SELECT ?,?,?,? WHERE EXISTS (SELECT 1 FROM sessions WHERE thread_id=? AND status='active' AND managed_chat=1)
ON CONFLICT(thread_id) DO NOTHING`, session.ThreadID, key, start, start, session.ThreadID)
	if err != nil {
		return v, err
	}
	err = s.db.QueryRow(`SELECT thread_id,address_key,baseline_ms,through_ms,from_ms,to_ms,page_token
FROM inbound_history_cursors WHERE thread_id=?`, session.ThreadID).Scan(&v.ThreadID, &v.AddressKey, &v.BaselineMS, &v.ThroughMS, &v.FromMS, &v.ToMS, &v.Token)
	if err == nil && v.AddressKey != key {
		return v, errors.New("收件对账绑定地址已改变，未继续旧群查询")
	}
	return v, err
}
func (s *Store) AuthorizedInboundBackfills(v InboundHistoryCursor) ([]InboundBackfill, error) {
	rows, err := s.db.Query(`SELECT b.event_id,b.created_ms
FROM inbound_history_backfills b
WHERE b.thread_id=? AND b.address_key=?
  AND NOT EXISTS (SELECT 1 FROM events e WHERE e.id=b.event_id)
  AND NOT EXISTS (SELECT 1 FROM inbound_history_receipts r WHERE r.thread_id=b.thread_id AND r.event_id=b.event_id)
ORDER BY b.created_ms,b.event_id`, v.ThreadID, v.AddressKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboundBackfill
	for rows.Next() {
		var b InboundBackfill
		if err = rows.Scan(&b.EventID, &b.CreatedMS); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *Store) SaveInboundHistoryWindow(old, next InboundHistoryCursor) error {
	result, err := s.db.Exec(`UPDATE inbound_history_cursors SET through_ms=?,from_ms=?,to_ms=?,page_token=?,checked_at=?,error=''
WHERE thread_id=? AND address_key=? AND baseline_ms=? AND through_ms=? AND from_ms=? AND to_ms=? AND page_token=?
  AND EXISTS (SELECT 1 FROM sessions WHERE thread_id=? AND status='active' AND managed_chat=1)`,
		next.ThroughMS, next.FromMS, next.ToMS, next.Token, time.Now().Unix(), old.ThreadID, old.AddressKey, old.BaselineMS, old.ThroughMS, old.FromMS, old.ToMS, old.Token, old.ThreadID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("收件对账游标版本已变化")
	}
	return nil
}
func (s *Store) InboundHistoryHandled(thread, event string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT (SELECT count(*) FROM events WHERE id=?) +
(SELECT count(*) FROM inbound_history_receipts WHERE thread_id=? AND event_id=?)`, event, thread, event).Scan(&count)
	return count > 0, err
}
func (s *Store) RecordInboundHistoryReceipt(v InboundHistoryCursor, event, disposition string, createdMS int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if disposition == "received" {
		var n int
		if err = tx.QueryRow("SELECT count(*) FROM events WHERE id=?", event).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return errors.New("历史消息尚未持久收件，不能标记完成")
		}
	}
	result, err := tx.Exec(`INSERT INTO inbound_history_receipts(thread_id,event_id,disposition,created_ms,recorded_at)
VALUES (?,?,?,?,?) ON CONFLICT(thread_id,event_id) DO NOTHING`, v.ThreadID, event, disposition, createdMS, time.Now().Unix())
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if disposition == "received" && inserted == 1 {
		if _, err = tx.Exec("UPDATE inbound_history_cursors SET recovered=recovered+1 WHERE thread_id=? AND address_key=?", v.ThreadID, v.AddressKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) RecordInboundHistoryFailure(v InboundHistoryCursor, hint string) error {
	_, err := s.db.Exec("UPDATE inbound_history_cursors SET error=? WHERE thread_id=? AND address_key=?", hint, v.ThreadID, v.AddressKey)
	return err
}
