package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"im-agent/internal/im"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type Store struct {
	db     *sql.DB
	unlock func()
}
type Session struct {
	ThreadID               string
	Address                im.Address
	WorkDir, Owner, Status string
	CreatedAt              int64
	Managed                bool
	// 以下是创建时元数据，不参与会话映射表的读取。
	GroupName string
	Fresh     bool
}
type Action struct {
	ID, ThreadID, MessageID, Kind, Payload, Status, UserID string
	Address                                                im.Address
	ExpiresAt                                              int64
}
type Delivery struct {
	ID      string
	Address im.Address
	Text    string
}

func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func New(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	st, e := os.Lstat(dir)
	if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("状态目录无效或是符号链接")
	}
	if e = os.Chmod(dir, 0700); e != nil {
		return nil, e
	}
	unlock, e := lockDirectory(dir)
	if e != nil {
		return nil, e
	}
	opened := false
	defer func() {
		if !opened {
			unlock()
		}
	}()
	p := filepath.Join(dir, "sessions.db")
	if stat, err := os.Lstat(p); err == nil && !stat.Mode().IsRegular() {
		return nil, errors.New("数据库不是普通文件")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return nil, e
	}
	f.Close()
	db, e := sql.Open("sqlite", p)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, unlock: unlock}
	// 单写连接和 DELETE 日志避免旧 SQLite WAL-reset 风险；不擅自覆盖已有业务记录。
	for _, q := range []string{"PRAGMA journal_mode=DELETE", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON", schema} {
		if _, e = db.Exec(q); e != nil {
			db.Close()
			return nil, e
		}
	}
	if e = s.migrateClosure(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateClosureArchive(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateTopics(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migratePresentation(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateEventDiagnostics(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateCreationForms(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateImageDrafts(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateAttachmentContext(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateAttachments(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateInboundHistory(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateInboundQueue(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateLiveMirror(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateRequestResults(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateSessionStatus(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateSessionControls(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateStatusCardRelocation(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateDesktopApprovals(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateInteractionWaiting(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateHistoryRetention(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migratePresentationRetention(); e != nil {
		db.Close()
		return nil, e
	}
	if _, e = db.Exec("UPDATE pending_actions SET status = 'abandoned' WHERE status = 'pending'"); e != nil {
		db.Close()
		return nil, e
	}
	if _, e = db.Exec(`UPDATE events SET status = 'interrupted', updated_at = ?
WHERE status = 'processing' AND NOT EXISTS
      (SELECT 1 FROM inbound_messages i WHERE i.event_id=events.id)`, time.Now().Unix()); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.migrateQA(); e != nil {
		db.Close()
		return nil, e
	}
	if e = s.RecoverQA(); e != nil {
		db.Close()
		return nil, e
	}

	opened = true
	return s, nil
}

const schema = `CREATE TABLE IF NOT EXISTS sessions (
    thread_id TEXT PRIMARY KEY,
    channel TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    work_dir TEXT NOT NULL,
    owner_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (channel, connection_id, chat_id)
);
CREATE TABLE IF NOT EXISTS pending_actions (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES sessions(thread_id),
    channel TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    message_id TEXT NOT NULL DEFAULT '',
    user_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL,
    status TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    resolved_at INTEGER
);
CREATE TABLE IF NOT EXISTS events (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS outbox (
    id TEXT PRIMARY KEY,
    channel TEXT NOT NULL,
    connection_id TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    body TEXT NOT NULL,
    sent INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS actions_thread ON pending_actions(thread_id);
CREATE INDEX IF NOT EXISTS outbox_sent ON outbox(sent, created_at);`

func (s *Store) Close() error {
	err := s.db.Close()
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return err
}
func (s *Store) scan(row *sql.Row) (Session, error) {
	var v Session
	e := row.Scan(&v.ThreadID, &v.Address.Channel, &v.Address.Connection, &v.Address.ChatID, &v.WorkDir, &v.Owner, &v.Status, &v.CreatedAt, &v.Managed, &v.Address.TopicID, &v.Address.RootMessageID)
	return v, e
}

const columns = "thread_id, channel, connection_id, chat_id, work_dir, owner_id, status, created_at, managed_chat, topic_id, root_message_id"

func (s *Store) ByChat(a im.Address) (Session, error) {
	return s.scan(s.db.QueryRow("SELECT "+columns+" FROM sessions WHERE channel = ? AND connection_id = ? AND chat_id = ? AND topic_id = ?", a.Channel, a.Connection, a.ChatID, a.TopicID))
}
func (s *Store) ByThread(id string) (Session, error) {
	return s.scan(s.db.QueryRow("SELECT "+columns+" FROM sessions WHERE thread_id = ?", id))
}
func (s *Store) Save(v Session) error {
	if v.Address.IsTopic() && v.Address.RootMessageID == "" {
		return errors.New("话题缺少根消息 ID")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var conflicts int
	if e = tx.QueryRow(`SELECT count(*) FROM sessions WHERE channel = ? AND connection_id = ? AND chat_id = ?
 AND ((topic_id = '' AND ? <> '') OR (topic_id <> '' AND ? = ''))`, v.Address.Channel, v.Address.Connection, v.Address.ChatID, v.Address.TopicID, v.Address.TopicID).Scan(&conflicts); e != nil {
		return e
	}
	if conflicts > 0 {
		return errors.New("同一个群不能同时绑定独立会话与话题会话")
	}
	_, e = tx.Exec(`INSERT INTO sessions (thread_id, channel, connection_id, chat_id, work_dir, owner_id, status, created_at, updated_at, managed_chat, topic_id, root_message_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, v.ThreadID, v.Address.Channel, v.Address.Connection, v.Address.ChatID, v.WorkDir, v.Owner, "active", time.Now().Unix(), time.Now().Unix(), v.Managed, v.Address.TopicID, v.Address.RootMessageID)
	if e != nil {
		return e
	}
	if v.Managed {
		history := "pending"
		if v.Fresh {
			history = "empty"
		}
		_, e = tx.Exec("INSERT INTO session_presentation (thread_id, target_name, name_applied, history_state, updated_at) VALUES (?, ?, ?, ?, ?)", v.ThreadID, v.GroupName, v.GroupName != "", history, time.Now().Unix())
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) Archive(id string) error {
	_, e := s.db.Exec("UPDATE sessions SET status = 'archived', updated_at = ? WHERE thread_id = ?", time.Now().Unix(), id)
	return e
}
func (s *Store) Claim(id string) (bool, error) {
	r, e := s.db.Exec("INSERT INTO events (id, status, created_at) VALUES (?, 'processing', ?) ON CONFLICT(id) DO NOTHING", id, time.Now().Unix())
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) EventDone(id, status string) error {
	_, e := s.db.Exec("UPDATE events SET status = ?, updated_at = ? WHERE id = ?", status, time.Now().Unix(), id)
	return e
}
func (s *Store) Release(id string) error {
	_, e := s.db.Exec("DELETE FROM events WHERE id = ? AND status = 'processing'", id)
	return e
}
func (s *Store) PutAction(a Action) error {
	_, e := s.db.Exec(`INSERT INTO pending_actions (id, thread_id, channel, connection_id, chat_id, message_id, user_id, kind, payload, status, expires_at, created_at, topic_id, root_message_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`, a.ID, a.ThreadID, a.Address.Channel, a.Address.Connection, a.Address.ChatID, a.MessageID, a.UserID, a.Kind, a.Payload, a.ExpiresAt, time.Now().Unix(), a.Address.TopicID, a.Address.RootMessageID)
	return e
}
func (s *Store) Action(id string) (Action, error) {
	var a Action
	e := s.db.QueryRow("SELECT id, thread_id, channel, connection_id, chat_id, message_id, user_id, kind, payload, status, expires_at, topic_id, root_message_id FROM pending_actions WHERE id = ?", id).Scan(&a.ID, &a.ThreadID, &a.Address.Channel, &a.Address.Connection, &a.Address.ChatID, &a.MessageID, &a.UserID, &a.Kind, &a.Payload, &a.Status, &a.ExpiresAt, &a.Address.TopicID, &a.Address.RootMessageID)
	return a, e
}
func (s *Store) ActionMessage(id, message string) error {
	_, e := s.db.Exec("UPDATE pending_actions SET message_id = ? WHERE id = ?", message, id)
	return e
}
func (s *Store) Resolve(id, status string) (bool, error) {
	r, e := s.db.Exec("UPDATE pending_actions SET status = ?, resolved_at = ? WHERE id = ? AND status = 'pending' AND (expires_at=0 OR expires_at > ?)", status, time.Now().Unix(), id, time.Now().Unix())
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) Expire(id, status string) error {
	_, e := s.db.Exec("UPDATE pending_actions SET status = ?, resolved_at = ? WHERE id = ? AND status = 'pending'", status, time.Now().Unix(), id)
	return e
}
func (s *Store) Enqueue(d Delivery) error {
	_, e := s.db.Exec(`INSERT INTO outbox (id, channel, connection_id, chat_id, body, created_at, topic_id, root_message_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, d.ID, d.Address.Channel, d.Address.Connection, d.Address.ChatID, d.Text, time.Now().Unix(), d.Address.TopicID, d.Address.RootMessageID)
	return e
}
func (s *Store) Deliveries() ([]Delivery, error) {
	rows, e := s.db.Query("SELECT id, channel, connection_id, chat_id, body, topic_id, root_message_id FROM outbox WHERE sent = 0 ORDER BY created_at, id LIMIT 50")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var ds []Delivery
	for rows.Next() {
		var d Delivery
		if e = rows.Scan(&d.ID, &d.Address.Channel, &d.Address.Connection, &d.Address.ChatID, &d.Text, &d.Address.TopicID, &d.Address.RootMessageID); e != nil {
			return nil, e
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}
func (s *Store) Sent(id string) error {
	_, e := s.db.Exec("UPDATE outbox SET sent = 1 WHERE id = ?", id)
	return e
}
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	e := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	return v, e
}
func (s *Store) SetMeta(key, v string) error {
	_, e := s.db.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, v)
	return e
}
func JSON(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		return fmt.Sprintf("%q", "无法序列化")
	}
	return string(b)
}

func (s *Store) Sessions() ([]Session, error) {
	rows, e := s.db.Query("SELECT " + columns + " FROM sessions WHERE status = 'active' ORDER BY created_at")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		if e = rows.Scan(&x.ThreadID, &x.Address.Channel, &x.Address.Connection, &x.Address.ChatID, &x.WorkDir, &x.Owner, &x.Status, &x.CreatedAt, &x.Managed, &x.Address.TopicID, &x.Address.RootMessageID); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// 创建意图先落盘，外部 API 超时后的下一次尝试沿用同一 thread 和建群幂等键。
func (s *Store) ReserveIntent(key, thread string) error {
	_, e := s.db.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING", "intent:"+key, thread)
	return e
}
func (s *Store) Intent(key string) (string, error)    { return s.GetMeta("intent:" + key) }
func (s *Store) LockDelivery(id string) (bool, error) { return s.Claim("delivery:" + id) }

// UpdateWorkDir 根据 Codex 实际返回值刷新元数据，不修改原任务目录或重新激活归档任务。
func (s *Store) UpdateWorkDir(thread, path string) error {
	if path == "" {
		return errors.New("Codex 未返回有效的会话目录")
	}
	result, e := s.db.Exec("UPDATE sessions SET work_dir = ?, updated_at = ? WHERE thread_id = ? AND status = 'active'", path, time.Now().Unix(), thread)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("会话已关闭，不再执行任务")
	}
	return nil
}
