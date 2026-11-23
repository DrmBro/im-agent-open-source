package store

import (
	"context"
	"errors"

	"im-agent/internal/im"
)

// 精确绑定到本次入站事件。重新派发的旧批次从成员记录取最后一条，不能用群里后来无关的消息。
func (s *Store) GroupQuoteSource(thread, event string) (im.Address, string, error) {
	if event == "" && thread == "" {
		return im.Address{}, "", errors.New("缺少来源事件或会话")
	}
	var err error
	if event == "" {
		err = s.db.QueryRow("SELECT event_id FROM inbound_messages WHERE thread_id=? ORDER BY rowid DESC LIMIT 1", thread).Scan(&event)
	} else if thread == "" {
		err = s.db.QueryRow("SELECT thread_id FROM inbound_messages WHERE event_id=?", event).Scan(&thread)
	}
	if err != nil {
		return im.Address{}, "", err
	}
	m, err := s.InboundMessageForThread(thread, event)
	if err != nil {
		return im.Address{}, "", err
	}
	wire, err := m.Address.DeliveryAddress()
	if err != nil {
		return wire, "", err
	}
	if m.PlatformID == "" {
		err = s.db.QueryRow(`SELECT q.message_id FROM assistant_batch_messages b JOIN assistant_queue q ON q.id=b.queue_id JOIN assistant_messages m ON m.message_id=q.message_id WHERE b.event_id=? AND m.conversation=? ORDER BY m.created_ms DESC,q.id DESC LIMIT 1`, event, m.ConversationID).Scan(&m.PlatformID)
		if err != nil {
			return wire, "", errors.New("此事件缺少可核对的群消息来源")
		}
	}
	return wire, m.PlatformID, nil
}

func (s *Store) GroupQuoteEvent(thread, item string) (string, error) {
	var event string
	err := s.db.QueryRow("SELECT event_id FROM request_result_replies WHERE thread_id=? AND item_id=?", thread, item).Scan(&event)
	return event, err
}

func (s *Store) AssistantReplySource(id, group string) (string, error) {
	var mid string
	err := s.db.QueryRow(`SELECT m.message_id FROM assistant_item_messages l JOIN assistant_messages m ON m.message_id=l.message_id WHERE l.item_id=? AND m.conversation=? ORDER BY m.self ASC,m.created_ms DESC,m.message_id DESC LIMIT 1`, id, group).Scan(&mid)
	return mid, err
}

func (s *Store) RecentQuoteGroups(ctx context.Context, connection, profile string) ([]string, error) {
	// 按最近活动选择，限制每个账号的监听数量。助手的关注源来自当前绑定的连接。
	prefix := "account:" + profile + ":group:"
	rows, err := s.db.QueryContext(ctx, `SELECT cid FROM (
SELECT substr(chat,length(?)+1) cid,received_at stamp FROM personal_inbox WHERE connection_id=? AND substr(chat,1,length(?))=? AND received_at>=unixepoch()-86400
UNION ALL SELECT conversation,created_ms/1000 FROM assistant_messages WHERE kind='group' AND created_ms>=(unixepoch()-86400)*1000 AND EXISTS(SELECT 1 FROM assistant_settings WHERE json_extract(body,'$.connection')=?)
) GROUP BY cid ORDER BY max(stamp) DESC,cid LIMIT 8`, prefix, connection, prefix, prefix, connection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid string
		if err := rows.Scan(&cid); err != nil {
			return nil, err
		}
		out = append(out, cid)
	}
	return out, rows.Err()
}
func (s *Store) RememberRecall(ctx context.Context, a im.Address, mid string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO group_recalled_messages(wire_key,message_id) VALUES(?,?) ON CONFLICT DO NOTHING", Hash(a.Key()), mid)
	return err
}
func (s *Store) HasRecall(ctx context.Context, a im.Address, mid string) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM group_recalled_messages WHERE wire_key=? AND message_id=?)", Hash(a.Key()), mid).Scan(&found)
	return found, err
}
