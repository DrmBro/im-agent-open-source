package store

import (
	"database/sql"
	"errors"
	"im-agent/internal/im"
	"time"
)

type SessionModel struct {
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Revision int64  `json:"revision"`
}
type SessionControl struct {
	ID, ThreadID, EventID, AddressKey, MessageID, Kind, Payload, State, Detail string
	ExpiresAt                                                                  int64
}

// 读取后仍在实际写入语句中比对全部地址字段，防止关闭或改绑插入读写之间。
const activeRemoteSessionSQL = `EXISTS (SELECT 1 FROM sessions s WHERE s.thread_id=? AND s.status='active'
 AND s.channel=? AND s.connection_id=? AND s.chat_id=? AND s.topic_id=?)`

func (s *Store) activeRemoteSession(thread, key string) (Session, error) {
	current, err := s.ByThread(thread)
	if err != nil {
		return current, err
	}
	if current.Status != "active" || Hash(current.Address.Key()) != key {
		return current, errors.New("远程入口已关闭或绑定已改变，旧操作失效")
	}
	return current, nil
}

func activeRemoteSessionArgs(session Session) []any {
	return []any{session.ThreadID, session.Address.Channel, session.Address.Connection, session.Address.ChatID, session.Address.TopicID}
}

func (s *Store) migrateSessionControls() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS session_model_preferences(thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id),address_key TEXT NOT NULL,model TEXT NOT NULL,effort TEXT NOT NULL,revision INTEGER NOT NULL,updated_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS session_controls(id TEXT PRIMARY KEY,thread_id TEXT NOT NULL REFERENCES sessions(thread_id),event_id TEXT NOT NULL,address_key TEXT NOT NULL,message_id TEXT NOT NULL,kind TEXT NOT NULL,payload TEXT NOT NULL,state TEXT NOT NULL,detail TEXT NOT NULL DEFAULT '',expires_at INTEGER NOT NULL,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS session_controls_card ON session_controls(thread_id,event_id,kind,state);`)
	if e != nil {
		return e
	}
	_, e = s.db.Exec(`INSERT INTO meta(key,value) VALUES('session_controls_enabled_at',?) ON CONFLICT(key) DO NOTHING`, time.Now().Unix())
	if e != nil {
		return e
	}
	_, e = s.db.Exec(`UPDATE session_controls SET state='unknown',detail='后台在处理期间退出，结果待核对，未重发操作' WHERE state='running'`)
	return e
}
func (s *Store) SessionModel(session Session) (SessionModel, error) {
	var v SessionModel
	var a string
	e := s.db.QueryRow(`SELECT address_key,model,effort,revision FROM session_model_preferences WHERE thread_id=?`, session.ThreadID).Scan(&a, &v.Model, &v.Effort, &v.Revision)
	if errors.Is(e, sql.ErrNoRows) {
		return v, nil
	}
	if e == nil && a != Hash(session.Address.Key()) {
		return SessionModel{}, errors.New("模型设置与当前会话地址不一致")
	}
	return v, e
}
func (s *Store) SessionForControlCard(address im.Address, message, event string) (Session, error) {
	rows, e := s.db.Query(`SELECT thread_id,address_key FROM session_status_cards WHERE message_id=? AND request_id=? AND state='sent'`, message, event)
	if e != nil {
		return Session{}, e
	}
	defer rows.Close()
	var thread, key string
	n := 0
	for rows.Next() {
		if e = rows.Scan(&thread, &key); e != nil {
			return Session{}, e
		}
		n++
	}
	if e = rows.Err(); e != nil {
		return Session{}, e
	}
	rows.Close()
	if n != 1 {
		return Session{}, errors.New("卡片身份不唯一或未确认送达")
	}
	session, e := s.ByThread(thread)
	if e != nil {
		return session, e
	}
	if session.Status != "active" || key != Hash(session.Address.Key()) || session.Address.BaseKey() != address.BaseKey() || address.TopicID != "" && session.Address.TopicID != address.TopicID {
		return Session{}, errors.New("卡片与活跃会话不一致")
	}
	return session, nil
}
func (s *Store) PutSessionControl(c SessionControl) (bool, error) {
	current, e := s.activeRemoteSession(c.ThreadID, c.AddressKey)
	if e != nil {
		return false, e
	}
	args := []any{c.ID, c.ThreadID, c.EventID, c.AddressKey, c.MessageID, c.Kind, c.Payload, c.ExpiresAt, time.Now().Unix(), c.ThreadID, c.EventID, c.AddressKey, c.MessageID}
	args = append(args, activeRemoteSessionArgs(current)...)
	r, e := s.db.Exec(`INSERT INTO session_controls(id,thread_id,event_id,address_key,message_id,kind,payload,state,expires_at,created_at)
 SELECT ?,?,?,?,?,?,?,'open',?,? WHERE EXISTS(SELECT 1 FROM session_status_cards WHERE thread_id=? AND request_id=? AND address_key=? AND message_id=? AND state='sent')
 AND `+activeRemoteSessionSQL+` ON CONFLICT(id) DO NOTHING`, args...)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) SessionControl(id string) (SessionControl, error) {
	var c SessionControl
	e := s.db.QueryRow(`SELECT id,thread_id,event_id,address_key,message_id,kind,payload,state,detail,expires_at FROM session_controls WHERE id=?`, id).Scan(&c.ID, &c.ThreadID, &c.EventID, &c.AddressKey, &c.MessageID, &c.Kind, &c.Payload, &c.State, &c.Detail, &c.ExpiresAt)
	return c, e
}
func (s *Store) OpenModelControl(thread, event string) (SessionControl, error) {
	return s.OpenSessionControl(thread, event, "model")
}
func (s *Store) OpenSessionControl(thread, event, kind string) (SessionControl, error) {
	rows, e := s.db.Query(`SELECT id FROM session_controls WHERE thread_id=? AND event_id=? AND kind=? AND state='open' AND (expires_at=0 OR expires_at>?)`, thread, event, kind, time.Now().Unix())
	if e != nil {
		return SessionControl{}, e
	}
	defer rows.Close()
	var id string
	count := 0
	for rows.Next() {
		if e = rows.Scan(&id); e != nil {
			return SessionControl{}, e
		}
		count++
	}
	if e = rows.Err(); e != nil {
		return SessionControl{}, e
	}
	rows.Close()
	if count == 0 {
		return SessionControl{}, sql.ErrNoRows
	}
	if count != 1 {
		return SessionControl{}, errors.New("该卡存在多个有效模型表单，未猜测选择")
	}
	return s.SessionControl(id)
}
func (s *Store) FinishSessionControl(id, state, detail string) error {
	_, e := s.db.Exec(`UPDATE session_controls SET state=?,detail=? WHERE id=? AND state IN ('open','running')`, state, detail, id)
	return e
}
func (s *Store) ClaimSessionControl(id string) (bool, error) {
	c, e := s.SessionControl(id)
	if e != nil {
		return false, e
	}
	current, e := s.activeRemoteSession(c.ThreadID, c.AddressKey)
	if e != nil {
		return false, e
	}
	args := []any{id, time.Now().Unix(), c.ThreadID, c.AddressKey}
	args = append(args, activeRemoteSessionArgs(current)...)
	r, e := s.db.Exec(`UPDATE session_controls SET state='running'
WHERE id=? AND state='open' AND (expires_at=0 OR expires_at>?) AND thread_id=? AND address_key=?
 AND `+activeRemoteSessionSQL, args...)
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) SaveSessionModel(session Session, c SessionControl, v SessionModel) error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var state string
	if e = tx.QueryRow(`SELECT state FROM session_controls WHERE id=? AND thread_id=? AND address_key=? AND (expires_at=0 OR expires_at>?)`, c.ID, session.ThreadID, Hash(session.Address.Key()), time.Now().Unix()).Scan(&state); e != nil {
		return e
	}
	if state != "open" {
		return errors.New("模型表单已处理，请重新打开")
	}
	r, e := tx.Exec(`INSERT INTO session_model_preferences(thread_id,address_key,model,effort,revision,updated_at) SELECT ?,?,?,?,1,? WHERE ?=0 AND EXISTS(SELECT 1 FROM sessions WHERE thread_id=? AND status='active') ON CONFLICT(thread_id) DO NOTHING`, session.ThreadID, Hash(session.Address.Key()), v.Model, v.Effort, time.Now().Unix(), v.Revision, session.ThreadID)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		r, e = tx.Exec(`UPDATE session_model_preferences SET model=?,effort=?,revision=revision+1,updated_at=? WHERE thread_id=? AND address_key=? AND revision=? AND EXISTS(SELECT 1 FROM sessions WHERE thread_id=? AND status='active')`, v.Model, v.Effort, time.Now().Unix(), session.ThreadID, Hash(session.Address.Key()), v.Revision, session.ThreadID)
		if e != nil {
			return e
		}
		n, _ = r.RowsAffected()
	}
	if n != 1 {
		return errors.New("模型设置已被更新，请重新打开表单")
	}
	if _, e = tx.Exec(`UPDATE session_controls SET state='done',detail='已保存下一轮模型' WHERE id=? AND state='open'`, c.ID); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) ExpireModelControls(thread, event string) error {
	_, e := s.db.Exec(`UPDATE session_controls SET state='expired' WHERE thread_id=? AND event_id=? AND kind='model' AND state='open' AND expires_at>0 AND expires_at<=?`, thread, event, time.Now().Unix())
	return e
}

func (s *Store) ControlCardsEnabledAt() (int64, error) {
	var at int64
	e := s.db.QueryRow(`SELECT value FROM meta WHERE key='session_controls_enabled_at'`).Scan(&at)
	return at, e
}

// 分组导航只更新原表单内容，使用快照比较避免迟到回调覆盖新选择。
func (s *Store) UpdateSessionControlPayload(c SessionControl, payload string) error {
	current, err := s.activeRemoteSession(c.ThreadID, c.AddressKey)
	if err != nil {
		return err
	}
	args := []any{payload, c.ID, c.Payload}
	args = append(args, activeRemoteSessionArgs(current)...)
	r, err := s.db.Exec("UPDATE session_controls SET payload=? WHERE id=? AND payload=? AND kind='forward' AND state='open' AND "+activeRemoteSessionSQL, args...)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("投递表单已变化，请刷新")
	}
	return nil
}

// 仅供发送入口在尚未创建投递记录、未调用平台时恢复表单。
func (s *Store) ReopenForwardControl(id string) error {
	_, err := s.db.Exec("UPDATE session_controls SET state='open',detail='准备失败，尚未发送，可重试' WHERE id=? AND kind='forward' AND state='running'", id)
	return err
}
