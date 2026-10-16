package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"im-agent/internal/im"
)

type QueuedInbound struct {
	EventID, ThreadID, AddressKey string
	Message                       im.Message
}

func (s *Store) migrateInboundQueue() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbound_messages (
 event_id TEXT PRIMARY KEY REFERENCES events(id),
 thread_id TEXT NOT NULL DEFAULT '',
 address_key TEXT NOT NULL,
 payload TEXT NOT NULL,
 created_at INTEGER NOT NULL
 );
 CREATE INDEX IF NOT EXISTS inbound_messages_thread ON inbound_messages(thread_id,created_at,event_id);`)
	return err
}

// 与幂等事件在同一事务持久化原始输入，避免已领取但进程退出后消息正文丢失。
func (s *Store) ClaimInbound(msg im.Message) (bool, error) {
	if msg.ID == "" || msg.Address.ChatID == "" {
		return false, errors.New("入站消息身份无效")
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec("INSERT INTO events(id,status,created_at,chat_key,stage,updated_at) VALUES (?,'processing',?,?,'received',?) ON CONFLICT(id) DO NOTHING", msg.ID, time.Now().Unix(), Hash(msg.Address.Key()), time.Now().Unix())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return false, err
	}
	if _, err = tx.Exec("INSERT INTO inbound_messages(event_id,address_key,payload,created_at) VALUES (?,?,?,?)", msg.ID, Hash(msg.Address.Key()), string(payload), time.Now().Unix()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// 队列状态和最终输入同一事务保存；这一阶段没有调用过 Codex 提交接口。
func (s *Store) QueueInbound(session Session, msg im.Message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE inbound_messages SET thread_id=?,address_key=?,payload=?
WHERE event_id=? AND address_key=?`, session.ThreadID, Hash(session.Address.Key()), string(payload), msg.ID, Hash(msg.Address.Key()))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return errors.New("入站消息与目标地址不一致，未排队")
	}
	result, err = tx.Exec(`UPDATE events SET stage='queued',chat_key=?,error_summary='',updated_at=?
WHERE id=? AND status='processing' AND stage='received'`, Hash(session.Address.Key()), time.Now().Unix(), msg.ID)
	if err != nil {
		return err
	}
	count, err = result.RowsAffected()
	if err != nil || count != 1 {
		return errors.New("入站阶段已变化，未排队")
	}
	return tx.Commit()
}

// 启动时只补偿尚未进入任何副作用阶段的收件记录。
func (s *Store) ReceivedInbounds() ([]im.Message, error) {
	rows, err := s.db.Query(`SELECT i.event_id,i.payload FROM inbound_messages i
         JOIN events e ON e.id=i.event_id AND e.chat_key=i.address_key
WHERE e.status='processing' AND e.stage='received' AND i.thread_id=''
ORDER BY e.created_at,e.rowid LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []im.Message
	invalid := 0
	for rows.Next() {
		var event, raw string
		if err = rows.Scan(&event, &raw); err != nil {
			return nil, err
		}
		var msg im.Message
		if err = json.Unmarshal([]byte(raw), &msg); err != nil || msg.ID != event || msg.Address.ChatID == "" {
			invalid++
			continue // 原记录保留待核对，其它独立请求仍继续处理。
		}
		messages = append(messages, msg)
	}
	if err = rows.Err(); err != nil {
		return messages, err
	}
	if invalid != 0 {
		return messages, errors.New("存在损坏的已收件快照，需要核对原请求")
	}
	return messages, nil
}

func (s *Store) ReceivedInbound(msg im.Message) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM inbound_messages i JOIN events e ON e.id=i.event_id
WHERE i.event_id=? AND i.address_key=? AND i.thread_id=''
  AND e.chat_key=i.address_key AND e.status='processing' AND e.stage='received'`,
		msg.ID, Hash(msg.Address.Key())).Scan(&count)
	return count == 1, err
}

// 只读明确未提交的请求；不按文本推断、不复活用户取消或提交结果未知的请求。
func (s *Store) QueuedInbounds() ([]QueuedInbound, error) {
	rows, err := s.db.Query(`SELECT i.event_id,i.thread_id,i.address_key,i.payload
FROM inbound_messages i
         JOIN events e ON e.id=i.event_id AND e.chat_key=i.address_key
         JOIN sessions s ON s.thread_id=i.thread_id AND s.status='active'
WHERE e.status='processing' AND e.stage='queued'
  AND NOT EXISTS (SELECT 1 FROM request_results r WHERE r.event_id=i.event_id)
  AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o WHERE o.event_id=i.event_id)
ORDER BY e.created_at,e.rowid
LIMIT 256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedInbound
	invalid := 0
	for rows.Next() {
		var v QueuedInbound
		var payload string
		if err = rows.Scan(&v.EventID, &v.ThreadID, &v.AddressKey, &payload); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(payload), &v.Message); err != nil || v.Message.ID != v.EventID {
			invalid++
			continue
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	if invalid != 0 {
		return out, errors.New("存在损坏的本地待调度快照，需要核对原请求")
	}
	return out, nil
}

// 运行期结束后的旧 processing 状态不能在下次启动时永久伪装成仍在执行。
// 明确处于提交前且无提交证据才允许恢复排队；其余仅进入原请求恢复观察。
func (s *Store) RecoverInboundStates() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE events SET stage='queued',updated_at=?
WHERE status='processing' AND stage IN ('starting','resuming','downloading_attachment')
  AND id IN (SELECT event_id FROM inbound_messages WHERE thread_id<>'')
  AND NOT EXISTS (SELECT 1 FROM request_results r WHERE r.event_id=events.id)
  AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o WHERE o.event_id=events.id)`, time.Now().Unix()); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE events SET status='uncertain',stage='execution_unconfirmed',
    error_summary='桥接重启后继续按原请求核对已提交轮次，不重新提交输入',updated_at=?
WHERE status='processing' AND stage='waiting_model'
  AND id IN (SELECT event_id FROM inbound_messages)
  AND EXISTS (SELECT 1 FROM request_results r WHERE r.event_id=events.id)`, time.Now().Unix()); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE events SET status='uncertain',stage='submission_unconfirmed',
    error_summary='桥接重启前提交阶段未取得完整回执，先核对原请求，不重新提交',updated_at=?
WHERE status='processing' AND stage='submitting'
  AND id IN (SELECT event_id FROM inbound_messages)`, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// 在提交之前原子领取本地排队记录；重启后的提交前记录可按证据恢复。
func (s *Store) BeginQueuedInbound(event, thread, key string) (bool, error) {
	result, err := s.db.Exec(`UPDATE events SET stage='starting',updated_at=?
WHERE id=? AND chat_key=? AND status='processing' AND stage='queued'
  AND EXISTS (SELECT 1 FROM inbound_messages i WHERE i.event_id=events.id
              AND i.thread_id=? AND i.address_key=?)
  AND NOT EXISTS (SELECT 1 FROM request_results r WHERE r.event_id=events.id)
  AND NOT EXISTS (SELECT 1 FROM live_mirror_origins o WHERE o.event_id=events.id)`,
		time.Now().Unix(), event, key, thread, key)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// 终态超过保留期后仅清空原始入站正文，不删除请求、回执和审计身份。
func (s *Store) PruneInboundPayloads(ctx context.Context, now time.Time) (int64, error) {
	cutoff := now.Add(-HistoryRetentionDays * 24 * time.Hour).Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE inbound_messages SET payload='{}'
WHERE event_id IN (SELECT i.event_id FROM inbound_messages i
                       JOIN events e ON e.id=i.event_id
                   WHERE e.status NOT IN ('processing','waiting_input')
                     AND i.created_at < ? AND i.payload <> '{}'
                   ORDER BY i.created_at,i.event_id LIMIT 100)`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
