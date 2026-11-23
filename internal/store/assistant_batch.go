package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"im-agent/internal/im"
)

type AssistantBatch struct {
	Event      string   `json:"event"`
	Item       string   `json:"item"`
	State      string   `json:"state"`
	MessageIDs []string `json:"message_ids"`
}

// 读取、认领消息、关联提交回执和进入原会话队列一次提交。重复点击使用同一批次上界。
func (s *Store) QueueAssistantBatch(ctx context.Context, item AssistantItem, session Session, msg im.Message, through int64, maxChars int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	source := msg.Address
	wire, err := session.Address.DeliveryAddress()
	if err != nil || wire.Key() != source.Key() {
		return errors.New("补充消息目标已改变")
	}
	var targetActive bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sessions WHERE thread_id=? AND status='active')", session.ThreadID).Scan(&targetActive); err != nil {
		return err
	}
	if !targetActive {
		return errors.New("目标会话已关闭，请刷新事项")
	}
	moving := session.ThreadID != item.ThreadID
	if moving {
		result, err := tx.ExecContext(ctx, `UPDATE assistant_items SET thread_id=?,session_error='',updated_ms=? WHERE id=? AND thread_id=? AND state='session'
AND EXISTS(SELECT 1 FROM sessions WHERE thread_id=assistant_items.thread_id AND status='archived')
AND EXISTS(SELECT 1 FROM assistant_queue WHERE item_id=assistant_items.id AND thread_id=assistant_items.thread_id AND consumed_at=0 AND id<=?)`, session.ThreadID, time.Now().UnixMilli(), item.ID, item.ThreadID, through)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return errors.New("事项已派发或消息已处理，请刷新")
		}
		if _, err := tx.ExecContext(ctx, "UPDATE assistant_queue SET thread_id=? WHERE item_id=? AND thread_id=? AND consumed_at=0", session.ThreadID, item.ID, item.ThreadID); err != nil {
			return err
		}
		if !msg.ReusedDesktop {
			if _, err := tx.ExecContext(ctx, "INSERT INTO inbox_sidebar_queue(thread_id,event_id) VALUES(?,?) ON CONFLICT(thread_id) DO NOTHING", session.ThreadID, msg.ID); err != nil {
				return err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT q.id,m.sender_name,m.text,m.quoted,m.created_ms FROM assistant_queue q JOIN assistant_messages m ON m.message_id=q.message_id JOIN assistant_items i ON i.id=q.item_id WHERE q.thread_id=? AND q.item_id=? AND i.state='session' AND i.thread_id=q.thread_id AND m.conversation=i.conversation AND q.consumed_at=0 AND q.id<=? ORDER BY m.created_ms,q.id`, session.ThreadID, item.ID, through)
	if err != nil {
		return err
	}
	type entry struct {
		id   int64
		text string
	}
	var entries []entry
	for rows.Next() {
		var id, stamp int64
		var sender, text, quote string
		if err := rows.Scan(&id, &sender, &text, &quote, &stamp); err != nil {
			rows.Close()
			return err
		}
		line := fmt.Sprintf("[%s] %s：%s\n", time.UnixMilli(stamp).Format("01-02 15:04"), sender, text)
		if quote != "" {
			line += "引用：" + quote + "\n"
		}
		entries = append(entries, entry{id, line})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	header := fmt.Sprintf("继续处理「%s」。以下为同一事项的新消息，作为分析材料。\n", item.Title)
	if moving {
		if len(entries) == 0 {
			return errors.New("没有可重新派发的消息")
		}
		header = fmt.Sprintf("接续已归档会话中的事项「%s」。\n%s\n以下是待处理的补充消息：\n", item.Title, msg.Text)
	}
	budget := maxChars - len([]rune(header))
	for len(entries) > 0 {
		var body strings.Builder
		n, used := 0, 0
		for n < len(entries) {
			size := len([]rune(entries[n].text))
			if used+size > budget {
				break
			}
			body.WriteString(entries[n].text)
			used += size
			n++
		}
		if n == 0 {
			return errors.New("单条补充超过当前输入上限，请调高运行限制后重试")
		}
		part := msg
		part.Text = header + body.String()
		part.ID = fmt.Sprintf("%s:%d", msg.ID, entries[0].id)
		claimed, err := enqueueAssistantTx(tx, session, part)
		if err != nil {
			return err
		}
		if !claimed {
			return errors.New("补充批次状态不一致，请核对提交记录")
		}
		for _, entry := range entries[:n] {
			if _, err = tx.Exec("INSERT INTO assistant_batch_messages(queue_id,event_id) VALUES(?,?)", entry.id, part.ID); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE assistant_queue SET consumed_at=? WHERE id=?", time.Now().Unix(), entry.id); err != nil {
				return err
			}
		}
		entries = entries[n:]
	}
	return tx.Commit()
}

func (s *Store) AssistantBatches(day string) ([]AssistantBatch, error) {
	rows, err := s.db.Query(`SELECT e.id,q.item_id,CASE WHEN r.execution_state='completed' THEN '已处理' WHEN e.status IN ('failed','uncertain','interrupted','queued_failed','cancelled','rejected') OR r.execution_state='failed' THEN '需核对' WHEN r.turn_ref<>'' THEN '已提交' ELSE '已入队' END,q.message_id
 FROM assistant_batch_messages b JOIN events e ON e.id=b.event_id JOIN assistant_queue q ON q.id=b.queue_id LEFT JOIN request_results r ON r.event_id=e.id WHERE date(e.created_at,'unixepoch','+8 hours')=? ORDER BY e.created_at DESC,e.id,q.id`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantBatch
	for rows.Next() {
		var id, item, state, mid string
		if err := rows.Scan(&id, &item, &state, &mid); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Event != id {
			out = append(out, AssistantBatch{Event: id, Item: item, State: state})
		}
		last := &out[len(out)-1]
		last.MessageIDs = append(last.MessageIDs, mid)
	}
	return out, rows.Err()
}

// QueueAssistantFollowup 在同一事务中去重、落输入并入队，可在任务恢复时安全重试。
func enqueueAssistantTx(tx *sql.Tx, s Session, m im.Message) (bool, error) {
	wire, err := s.Address.DeliveryAddress()
	if err != nil || wire.Key() != m.Address.Key() {
		return false, errors.New("原会话绑定已变化")
	}
	claimed, err := claimInboundTx(tx, m)
	if err != nil || !claimed {
		return false, err
	}
	m.Address = s.Address
	return true, queueInboundTx(tx, s, m, wire, nil)
}
func (s *Store) QueueAssistantFollowup(ctx context.Context, session Session, m im.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = enqueueAssistantTx(tx, session, m); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) InboundMessageForThread(thread, event string) (im.Message, error) {
	var msg im.Message
	var raw string
	if err := s.db.QueryRow("SELECT payload FROM inbound_messages WHERE thread_id=? AND event_id=?", thread, event).Scan(&raw); err != nil {
		return msg, err
	}
	err := json.Unmarshal([]byte(raw), &msg)
	if err == nil && msg.ID != event {
		return msg, errors.New("原消息身份不可核对")
	}
	return msg, err
}
