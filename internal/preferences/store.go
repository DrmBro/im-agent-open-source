// Package preferences 管理桌面配置和加密凭证；不向前端返回凭证明文。
package preferences

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"im-agent/internal/channels"
	"im-agent/internal/config"
	"im-agent/internal/store"
	"im-agent/internal/vault"
	_ "modernc.org/sqlite"
)

type Snapshot struct {
	Config       config.Config                `json:"config"`
	Revision     int64                        `json:"revision"`
	Credentials  map[string]map[string]string `json:"credentials"`
	DatabasePath string                       `json:"database_path"`
	UpdatedAt    string                       `json:"updated_at"`
}
type Change struct {
	ConnectionID string `json:"connection_id"`
	Field        string `json:"field"`
	Value        string `json:"value"`
	Clear        bool   `json:"clear"`
}
type SaveRequest struct {
	Config   config.Config `json:"config"`
	Revision int64         `json:"revision"`
	Secrets  []Change      `json:"secrets"`
}
type Store struct {
	mu        sync.Mutex
	db        *sql.DB
	dir, path string
	registry  *channels.Registry
	vault     *vault.Vault
}

func New(dir string, r *channels.Registry) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, errors.New("无法创建应用数据目录")
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("应用数据目录无效或是符号链接")
	}
	if e = os.Chmod(dir, 0700); e != nil {
		return nil, e
	}
	path := filepath.Join(dir, "desktop.sqlite")
	if st, e := os.Lstat(path); e == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return nil, errors.New("数据库必须是普通文件")
	} else if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	e = f.Chmod(0600)
	_ = f.Close()
	if e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	success := false
	defer func() {
		if !success {
			_ = db.Close()
		}
	}()
	for _, query := range []string{"PRAGMA journal_mode=DELETE", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=3000", schema} {
		if _, e = db.Exec(query); e != nil {
			return nil, errors.New("初始化桌面数据库失败")
		}
	}
	var account string
	e = db.QueryRow("SELECT value FROM desktop_meta WHERE key = 'vault_account'").Scan(&account)
	if errors.Is(e, sql.ErrNoRows) {
		b := make([]byte, 16)
		if _, e = rand.Read(b); e != nil {
			return nil, e
		}
		account = hex.EncodeToString(b)
		if _, e = db.Exec("INSERT INTO desktop_meta (key, value) VALUES ('vault_account', ?)", account); e != nil {
			return nil, e
		}
	} else if e != nil {
		return nil, e
	}
	s := &Store{db: db, dir: dir, path: path, registry: r, vault: vault.New(account)}
	success = true
	return s, nil
}

const schema = `CREATE TABLE IF NOT EXISTS desktop_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS desktop_config (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL,
    body TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS desktop_config_history (
    revision INTEGER PRIMARY KEY,
    body TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS desktop_credentials (
    connection_id TEXT NOT NULL,
    field TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    PRIMARY KEY (connection_id, field)
);`

func (s *Store) Close() error            { return s.db.Close() }
func (s *Store) Read() (Snapshot, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.read() }
func (s *Store) read() (Snapshot, error) {
	view := Snapshot{Config: config.Defaults(), DatabasePath: s.path, Credentials: map[string]map[string]string{}}
	view.Config.StateDir = filepath.Join(s.dir, "runtime")
	var raw string
	e := s.db.QueryRow("SELECT revision, body, updated_at FROM desktop_config WHERE id = 1").Scan(&view.Revision, &raw, &view.UpdatedAt)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return view, errors.New("读取桌面配置失败")
	}
	if e == nil {
		view.Config, e = config.Decode([]byte(raw))
		if e != nil {
			return view, e
		}
	}
	rows, e := s.db.Query("SELECT connection_id, field FROM desktop_credentials")
	if e != nil {
		return view, e
	}
	defer rows.Close()
	saved := map[string]bool{}
	for rows.Next() {
		var id, field string
		if e = rows.Scan(&id, &field); e != nil {
			return view, e
		}
		saved[id+"\x00"+field] = true
	}
	if e = rows.Err(); e != nil {
		return view, e
	}
	for _, c := range view.Config.Connections {
		d, e := s.registry.Descriptor(c.Type)
		if e != nil {
			return view, e
		}
		view.Credentials[c.ID] = map[string]string{}
		for _, f := range d.Credentials {
			state := "missing"
			if saved[c.ID+"\x00"+f.Key] {
				state = "saved"
			}
			view.Credentials[c.ID][f.Key] = state
		}
	}
	return view, nil
}
func (s *Store) validate(c config.Config) (config.Config, error) {
	// 桌面业务数据目录固定，避免 UI 将状态库切换至敏感目录或另一服务实例。
	c.StateDir = filepath.Join(s.dir, "runtime")
	normal, e := config.ValidateDraft(c, s.dir)
	if e != nil {
		return c, e
	}
	if len(c.Connections) > 20 {
		return c, errors.New("最多配置 20 个连接")
	}
	for _, connection := range normal.Connections {
		if connection.Credentials != nil {
			return c, errors.New("普通配置中不允许凭证明文")
		}
		if e = s.registry.Validate(connection); e != nil {
			return c, e
		}
	}
	return normal, nil
}
func (s *Store) Validate(c config.Config) (config.Config, error) { return s.validate(c) }
func scope(id, field string) string                              { return "im-agent:v1:" + id + ":" + field }
func (s *Store) Save(req SaveRequest) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, e := s.validate(req.Config)
	if e != nil {
		return Snapshot{}, e
	}
	current, e := s.read()
	if e != nil {
		return Snapshot{}, e
	}
	if req.Revision != current.Revision {
		return Snapshot{}, errors.New("配置版本已变化，请重新加载后再保存；未覆盖已有内容")
	}
	byID := map[string]config.Connection{}
	for _, connection := range c.Connections {
		byID[connection.ID] = connection
	}
	for _, old := range current.Config.Connections {
		if next, ok := byID[old.ID]; ok && next.Type != old.Type {
			return Snapshot{}, errors.New("已有连接不能直接更换渠道类型，请新建连接")
		}
	}
	type write struct {
		id, field string
		data      []byte
		clear     bool
	}
	writes := []write{}
	seen := map[string]bool{}
	for _, change := range req.Secrets {
		connection, ok := byID[change.ConnectionID]
		if !ok {
			return Snapshot{}, errors.New("凭证变更不属于当前连接")
		}
		d, _ := s.registry.Descriptor(connection.Type)
		valid := false
		for _, f := range d.Credentials {
			if f.Key == change.Field {
				valid = true
			}
		}
		k := scope(change.ConnectionID, change.Field)
		if !valid || seen[k] {
			return Snapshot{}, errors.New("未知或重复的凭证字段")
		}
		seen[k] = true
		if change.Clear && change.Value != "" {
			return Snapshot{}, errors.New("不能同时清除并写入同一密钥")
		}
		if !change.Clear && change.Value == "" {
			continue
		}
		if len(change.Value) > 8192 || strings.TrimSpace(change.Value) != change.Value || strings.ContainsAny(change.Value, "\x00\r\n") {
			return Snapshot{}, errors.New("凭证长度无效或包含空白/换行")
		}
		w := write{id: change.ConnectionID, field: change.Field, clear: change.Clear}
		if !w.clear {
			var count int
			if e = s.db.QueryRow("SELECT count(*) FROM desktop_credentials").Scan(&count); e != nil {
				return Snapshot{}, e
			}
			if e = s.vault.Unlock(count > 0); e != nil {
				return Snapshot{}, e
			}
			w.data, e = s.vault.Seal(k, change.Value)
			if e != nil {
				return Snapshot{}, e
			}
		}
		writes = append(writes, w)
	}
	raw, e := json.Marshal(c)
	if e != nil || len(raw) > 1<<20 {
		return Snapshot{}, errors.New("配置大小超限")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return Snapshot{}, e
	}
	defer tx.Rollback()
	// 事务内再次校验版本，阻止两个窗口或进程覆盖配置。
	var revision int64
	e = tx.QueryRow("SELECT revision FROM desktop_config WHERE id = 1").Scan(&revision)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return Snapshot{}, e
	}
	if revision != req.Revision {
		return Snapshot{}, errors.New("配置冲突，请重新加载")
	}
	if revision > 0 {
		if _, e = tx.Exec("INSERT INTO desktop_config_history (revision, body, updated_at) SELECT revision, body, updated_at FROM desktop_config WHERE id = 1"); e != nil {
			return Snapshot{}, e
		}
	}
	for _, old := range current.Config.Connections {
		if _, ok := byID[old.ID]; !ok {
			if _, e = tx.Exec("DELETE FROM desktop_credentials WHERE connection_id = ?", old.ID); e != nil {
				return Snapshot{}, e
			}
		}
	}
	for _, w := range writes {
		if w.clear {
			_, e = tx.Exec("DELETE FROM desktop_credentials WHERE connection_id = ? AND field = ?", w.id, w.field)
		} else {
			_, e = tx.Exec(`INSERT INTO desktop_credentials (connection_id, field, ciphertext)
VALUES (?, ?, ?) ON CONFLICT(connection_id, field) DO UPDATE SET ciphertext = excluded.ciphertext`, w.id, w.field, w.data)
		}
		if e != nil {
			return Snapshot{}, errors.New("保存加密凭证失败，事务已回滚")
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, e = tx.Exec(`INSERT INTO desktop_config (id, revision, body, updated_at)
VALUES (1, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET revision = excluded.revision, body = excluded.body, updated_at = excluded.updated_at`, revision+1, string(raw), now)
	if e != nil {
		return Snapshot{}, e
	}
	if _, e = tx.Exec("DELETE FROM desktop_config_history WHERE revision < ?", revision-19); e != nil {
		return Snapshot{}, e
	}
	if e = tx.Commit(); e != nil {
		return Snapshot{}, errors.New("提交配置失败，请重新加载确认状态")
	}
	return s.read()
}
func (s *Store) Runtime() (config.Config, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, e := s.read()
	if e != nil {
		return config.Config{}, 0, e
	}
	if v.Revision == 0 {
		return config.Config{}, 0, errors.New("请先保存配置")
	}
	c, e := s.validate(v.Config)
	if e != nil {
		return c, 0, e
	}
	if len(c.Connections) == 0 {
		return c, 0, errors.New("请先添加至少一个 IM 连接")
	}

	type encrypted struct {
		id, field string
		data      []byte
	}
	all := []encrypted{}
	rows, e := s.db.Query("SELECT connection_id, field, ciphertext FROM desktop_credentials")
	if e != nil {
		return c, 0, e
	}
	for rows.Next() {
		var x encrypted
		if e = rows.Scan(&x.id, &x.field, &x.data); e != nil {
			rows.Close()
			return c, 0, e
		}
		all = append(all, x)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return c, 0, e
	}
	if len(all) > 0 {
		if e = s.vault.Unlock(true); e != nil {
			return c, 0, e
		}
	}
	for i := range c.Connections {
		connection := &c.Connections[i]
		connection.Credentials = map[string]string{}
		for _, x := range all {
			if x.id == connection.ID {
				value, e := s.vault.Open(scope(x.id, x.field), x.data)
				if e != nil {
					return c, 0, e
				}
				connection.Credentials[x.field] = value
			}
		}
		d, _ := s.registry.Descriptor(connection.Type)
		for _, f := range d.Credentials {
			value, _ := connection.Credential(f.Key)
			if f.Required && value == "" {
				return c, 0, fmt.Errorf("连接 %s 缺少 %s，请在客户端填写并保存凭证", connection.ID, f.Label)
			}
			if value != "" {
				connection.Credentials[f.Key] = value
			}
		}
	}
	return c, v.Revision, nil
}

func (s *Store) ClosureStatus() ([]store.Closure, error) {
	return store.ClosureSnapshot(filepath.Join(s.dir, "runtime"))
}

func (s *Store) PresentationStatus() ([]store.Presentation, error) {
	return store.PresentationSnapshot(filepath.Join(s.dir, "runtime"))
}

func (s *Store) EventDiagnostics() ([]store.EventDiagnostic, error) {
	return store.EventDiagnosticsSnapshot(filepath.Join(s.dir, "runtime"))
}
