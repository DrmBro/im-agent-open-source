package store

import (
	"database/sql"
	"errors"
	"time"
)

func (s *Store) migrateStatusCardRelocation() error {
	var found int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('session_status_cards') WHERE name='generation'`).Scan(&found); err != nil {
		return err
	}
	if found == 0 {
		if _, err := s.db.Exec(`ALTER TABLE session_status_cards ADD COLUMN generation INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS status_card_receipts (
 thread_id TEXT NOT NULL, request_id TEXT NOT NULL, generation INTEGER NOT NULL, address_key TEXT NOT NULL,
 message_id TEXT NOT NULL, state TEXT NOT NULL, digest TEXT NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(thread_id,request_id,generation));
 CREATE TABLE IF NOT EXISTS status_card_commands(command_id TEXT PRIMARY KEY,thread_id TEXT NOT NULL,request_id TEXT NOT NULL,created_at INTEGER NOT NULL);`)
	return err
}

// 用户主动查询时把卡片重新展示到消息底部。旧回执归档保留，未知送达不轮换。
// command为平台消息ID，同一命令重复回调不产生第二张卡；后台定时刷新不调用此函数。
func (s *Store) RelocateStatusCard(session Session, request, command string) error {
	if command == "" {
		return errors.New("主动查询缺少消息标识")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var already int
	if err = tx.QueryRow(`SELECT count(*) FROM status_card_commands WHERE command_id=?`, command).Scan(&already); err != nil {
		return err
	}
	if already != 0 {
		return nil
	}
	var currentKey string
	if err = tx.QueryRow(`SELECT channel || char(0) || connection_id || char(0) || chat_id || CASE WHEN topic_id='' THEN '' ELSE char(0) || topic_id END FROM sessions WHERE thread_id=? AND status='active'`, session.ThreadID).Scan(&currentKey); err != nil {
		return err
	}
	if currentKey != session.Address.Key() {
		return errors.New("会话绑定已经变化，未重新展示卡片")
	}
	var c SessionStatusCard
	err = tx.QueryRow(`SELECT address_key,message_id,state,digest,updated_at,generation FROM session_status_cards WHERE thread_id=? AND request_id=?`, session.ThreadID, request).Scan(&c.AddressKey, &c.MessageID, &c.State, &c.Digest, &c.UpdatedAt, &c.Generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if c.AddressKey != Hash(session.Address.Key()) {
			return errors.New("原卡片绑定地址已经变化")
		}
		if c.State == "creating" || c.State == "unknown" {
			return errors.New("原状态卡发送结果尚未确认，没有重复发卡")
		}
		if c.MessageID != "" {
			if _, err = tx.Exec(`INSERT INTO status_card_receipts(thread_id,request_id,generation,address_key,message_id,state,digest,updated_at) VALUES(?,?,?,?,?,?,?,?)`, session.ThreadID, request, c.Generation, c.AddressKey, c.MessageID, c.State, c.Digest, c.UpdatedAt); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`UPDATE session_status_cards SET message_id='',state='failed',digest='',error='',attempts=0,next_attempt=0,observed_version='',generation=generation+1,updated_at=? WHERE thread_id=? AND request_id=?`, time.Now().Unix(), session.ThreadID, request); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE session_controls SET state='expired',detail='用户重新查询，旧卡片已失效' WHERE thread_id=? AND event_id=? AND state='open'`, session.ThreadID, request); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(`INSERT INTO session_status_cards(thread_id,request_id,address_key,event_id,state,updated_at) VALUES(?,?,?,?,'failed',?)`, session.ThreadID, request, Hash(session.Address.Key()), request, time.Now().Unix()); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO status_card_commands(command_id,thread_id,request_id,created_at) VALUES(?,?,?,?)`, command, session.ThreadID, request, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) EmptyStatusCardNeedsRefresh(session Session) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM session_status_cards c WHERE c.thread_id=? AND c.request_id='' AND c.address_key=? AND c.state NOT IN ('creating','unknown') AND c.next_attempt<=? AND (c.state='failed' OR c.error<>'' OR EXISTS(SELECT 1 FROM session_controls sc WHERE sc.thread_id=c.thread_id AND sc.event_id='' AND sc.kind='model' AND sc.state='open'))`, session.ThreadID, Hash(session.Address.Key()), time.Now().Unix()).Scan(&n)
	return n == 1, err
}
