package store

import (
	"errors"
	"time"

	"im-agent/internal/im"
)

// 仅由经过渠道鉴权、白名单校验的刷新按钮回调调用。
// 已知旧代卡片不能补作新代回执；不按时间或相似正文猜测身份。
func (s *Store) RecoverStatusCardReceipt(address im.Address, event, message, user string) error {
	if message == "" || user == "" {
		return errors.New("回调回执不完整")
	}
	rows, err := s.db.Query(`SELECT c.thread_id FROM session_status_cards c JOIN sessions s ON s.thread_id=c.thread_id
 WHERE c.request_id=? AND c.state='unknown' AND c.message_id='' AND s.status='active' AND s.channel=? AND s.connection_id=? AND s.chat_id=? AND (?='' OR s.topic_id=?)`, event, address.Channel, address.Connection, address.ChatID, address.TopicID, address.TopicID)
	if err != nil {
		return err
	}
	var thread string
	n := 0
	for rows.Next() {
		if err = rows.Scan(&thread); err != nil {
			rows.Close()
			return err
		}
		n++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("未确认卡片关联不唯一")
	}
	session, err := s.ByThread(thread)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var known int
	if err = tx.QueryRow(`SELECT (SELECT count(*) FROM status_card_receipts WHERE message_id=?)+(SELECT count(*) FROM session_status_cards WHERE message_id=?)`, message, message).Scan(&known); err != nil {
		return err
	}
	if known != 0 {
		return errors.New("该回执已属于其他或历史卡片")
	}
	result, err := tx.Exec(`UPDATE session_status_cards SET message_id=?,state='sent',error='',attempts=0,next_attempt=0,digest='',observed_version='',updated_at=?
 WHERE thread_id=? AND request_id=? AND state='unknown' AND message_id='' AND address_key=?
 AND EXISTS(SELECT 1 FROM sessions s WHERE s.thread_id=session_status_cards.thread_id AND s.status='active' AND s.channel=? AND s.connection_id=? AND s.chat_id=? AND s.topic_id=?)`, message, time.Now().Unix(), thread, event, Hash(session.Address.Key()), session.Address.Channel, session.Address.Connection, session.Address.ChatID, session.Address.TopicID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("卡片状态已变化，未绑定回执")
	}
	_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO NOTHING`, "status-card-receipt-proof:"+Hash(thread+"\x00"+event+"\x00"+message), JSON(map[string]any{"source": "authenticated_refresh_callback", "message_id": message, "user_id": user, "at": time.Now().Unix()}))
	if err != nil {
		return err
	}
	return tx.Commit()
}
