package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"im-agent/internal/im"
)

// PreviousIndependentGroup 仅读取先前显式迁移的创建证明和历史回执，不猜测同名群。
func (s *Store) PreviousIndependentGroup(thread string) (Session, *Presentation, error) {
	raw, err := s.GetMeta("topic-migration-snapshot:" + thread)
	if err != nil {
		return Session{}, nil, err
	}
	var snap struct {
		Session      Session `json:"session"`
		Presentation struct {
			Name         string `json:"target_name"`
			NameApplied  int    `json:"name_applied"`
			HistoryState string `json:"history_state"`
			Count        int    `json:"history_count"`
			Payload      string `json:"history_payload"`
			MessageID    string `json:"history_message_id"`
		} `json:"presentation"`
	}
	if json.Unmarshal([]byte(raw), &snap) != nil || snap.Session.ThreadID != thread || !snap.Session.Managed || snap.Session.Address.IsTopic() || snap.Session.Address.ChatID == "" {
		return Session{}, nil, errors.New("原独立群的迁移快照无效，未猜测替代群")
	}
	p := &Presentation{ThreadID: thread, Name: snap.Presentation.Name, NameApplied: snap.Presentation.NameApplied == 1, HistoryState: snap.Presentation.HistoryState, HistoryCount: snap.Presentation.Count, Payload: snap.Presentation.Payload, MessageID: snap.Presentation.MessageID}
	return snap.Session, p, nil
}

// MoveToIndependentGroup 仅由停止桥接且持有 Store 独占锁的显式迁移调用。
// 已发送的实时消息记录/游标不改变；未确认投递不允许跨目标挪动。
func (s *Store) MoveToIndependentGroup(old Session, target im.Address, name string) error {
	if old.Status != "active" || !old.Managed || !old.Address.IsTopic() || target.IsTopic() || target.RootMessageID != "" || target.ChatID == "" || target.ChatID == old.Address.ChatID || target.Channel != old.Address.Channel || target.Connection != old.Address.Connection {
		return errors.New("目标必须是同连接的独立群，仅迁移活跃的客户端自建话题")
	}
	p := Presentation{Name: name, HistoryState: "pending"}
	previous, preserved, err := s.PreviousIndependentGroup(old.ThreadID)
	if err == nil && previous.Address.Key() == target.Key() {
		p = *preserved
		p.Name = name
		p.NameApplied = false
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if p.HistoryState == "" {
		p.HistoryState = "pending"
	}
	if p.HistoryState == "sending" {
		p.HistoryState = "unknown"
	}
	if p.HistoryState == "sent" && p.MessageID == "" {
		return errors.New("原群历史标为已发送但缺少回执，停止迁移")
	}
	currentPresentation, err := s.Presentation(old.ThreadID)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var blockers int
	err = tx.QueryRow(`SELECT (SELECT count(*) FROM events WHERE status='processing')
                     + (SELECT count(*) FROM pending_actions WHERE status='pending')
                     + (SELECT count(*) FROM session_closures WHERE thread_id=?)
                     + (SELECT count(*) FROM live_mirror_parts WHERE thread_id=? AND state<>'sent')
                     + (SELECT count(*) FROM live_mirror_origins WHERE thread_id=? AND state='pending')
                     + (SELECT count(*) FROM outbox WHERE sent=0 AND channel=? AND connection_id=? AND chat_id=? AND topic_id=?)
                     + (SELECT count(*) FROM sessions WHERE channel=? AND connection_id=? AND chat_id=?)
                     + (SELECT count(*) FROM meta WHERE key=?)`,
		old.ThreadID, old.ThreadID, old.ThreadID, old.Address.Channel, old.Address.Connection, old.Address.ChatID, old.Address.TopicID, target.Channel, target.Connection, target.ChatID, "shared-chat:"+target.BaseKey()).Scan(&blockers)
	if err != nil {
		return err
	}
	if blockers != 0 {
		return errors.New("存在执行、审批、归档、未确认投递或目标群占用，不能迁移")
	}
	var retired string
	err = tx.QueryRow("SELECT value FROM meta WHERE key=?", "retired-group:"+target.BaseKey()).Scan(&retired)
	if err == nil {
		var destination im.Address
		if previous.Address.Key() != target.Key() || json.Unmarshal([]byte(retired), &destination) != nil || destination.Key() != old.Address.Key() {
			return errors.New("目标群的旧迁移标记不属于当前话题，未解除保护")
		}
		if _, err = tx.Exec("DELETE FROM meta WHERE key=? AND value=?", "retired-group:"+target.BaseKey(), retired); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Payload 的 json:"-" 仅用于前端隐藏；迁移存档另外保存，以便人工回滚核对。
	snapshot := JSON(map[string]any{"session": old, "presentation": currentPresentation, "history_payload": currentPresentation.Payload, "target": target})
	if _, err = tx.Exec("INSERT INTO meta (key,value) VALUES (?,?)", "group-migration-snapshot:"+old.ThreadID+":"+old.Address.Key(), snapshot); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO meta (key,value) VALUES (?,?)", "retired-topic:"+old.Address.Key(), JSON(target)); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE sessions SET chat_id=?,topic_id='',root_message_id='',managed_chat=1,updated_at=?
 WHERE thread_id=? AND channel=? AND connection_id=? AND chat_id=? AND topic_id=? AND status='active'`, target.ChatID, time.Now().Unix(), old.ThreadID, old.Address.Channel, old.Address.Connection, old.Address.ChatID, old.Address.TopicID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("原会话映射已经变化，未迁移")
	}
	_, err = tx.Exec(`UPDATE session_presentation SET target_name=?,name_applied=0,
 history_state=?,history_count=?,history_payload=?,history_message_id=?,name_error='',history_error='',attempts=0,next_attempt=0,updated_at=?
 WHERE thread_id=?`, name, p.HistoryState, p.HistoryCount, p.Payload, p.MessageID, time.Now().Unix(), old.ThreadID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RetiredTarget 支持旧群与旧话题各自的停用记录，不停用承载其他话题的共享群。
func (s *Store) RetiredTarget(a im.Address) (im.Address, bool, error) {
	key := "retired-group:" + a.BaseKey()
	if a.IsTopic() {
		key = "retired-topic:" + a.Key()
	}
	raw, err := s.GetMeta(key)
	if errors.Is(err, sql.ErrNoRows) {
		return im.Address{}, false, nil
	}
	if err != nil {
		return im.Address{}, false, err
	}
	var target im.Address
	if json.Unmarshal([]byte(raw), &target) != nil || target.ChatID == "" {
		return target, true, errors.New("入口迁移标记无效，禁止执行")
	}
	return target, true, nil
}
