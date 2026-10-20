package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	WorkDir     string       `json:"work_dir"`
	StateDir    string       `json:"state_dir"`
	Codex       Codex        `json:"codex"`
	Connections []Connection `json:"connections"`
	Limits      Limits       `json:"limits"`
	Watch       Watch        `json:"watch"`
}
type Codex struct {
	SubmissionMode          string `json:"submission_mode"`
	AutoSwitchThread        bool   `json:"auto_switch_thread"`
	PreserveDesktopDraft    bool   `json:"preserve_desktop_draft"`
	AutoRestoreDesktopDraft bool   `json:"auto_restore_desktop_draft"`
	Mode                    string `json:"mode"`
	Binary                  string `json:"binary"`
	Home                    string `json:"home"`
	Model                   string `json:"model"`
	Sandbox                 string `json:"sandbox"`
	ApprovalPolicy          string `json:"approval_policy"`
	CdpPort                 int    `json:"cdp_port"`
}
type Connection struct {
	ID               string            `json:"id"`
	Type             string            `json:"type"`
	AllowedUsers     []string          `json:"allowed_users"`
	AdminUsers       []string          `json:"admin_users"`
	ChatNamePrefix   string            `json:"chat_name_prefix"`
	AutoCreateChat   bool              `json:"auto_create_chat"`
	ConversationMode string            `json:"conversation_mode"`
	SharedChatID     string            `json:"shared_chat_id"`
	Credentials      map[string]string `json:"-"`
}
type Limits struct {
	TurnTimeoutSeconds     int `json:"turn_timeout_seconds"`
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds"` // 旧配置兼容字段；用户交互不再按此值超时。
	MaxMessageChars        int `json:"max_message_chars"`
	MaxInputChars          int `json:"max_input_chars"`
	QueueSize              int `json:"queue_size"`
	Workers                int `json:"workers"`
	IdlePushMS             int `json:"idle_push_ms"`
}
type Watch struct {
	Enabled         bool   `json:"enabled"`
	ConnectionID    string `json:"connection_id"`
	IntervalSeconds int    `json:"interval_seconds"`
	ImportExisting  bool   `json:"import_existing"`
}

func Defaults() Config {
	return Config{Codex: Codex{Mode: "follow_local", SubmissionMode: "native_queue", Sandbox: "workspace-write", ApprovalPolicy: "untrusted"}, Limits: Limits{900, 0, 2000, 24000, 32, 4, 3000}, Watch: Watch{IntervalSeconds: 10}, Connections: []Connection{}}
}

// Decode 可导入旧版环境变量字段，但仅丢弃字段，不读取或导入环境值。
func Decode(data []byte) (Config, error) {
	c := Defaults()
	if len(data) > 1<<20 {
		return c, errors.New("配置大小超过 1 MB")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return c, errors.New("配置必须是 JSON 对象")
	}
	if connections, ok := raw["connections"]; ok {
		var entries []map[string]json.RawMessage
		if json.Unmarshal(connections, &entries) != nil {
			return c, errors.New("connections 必须是连接列表")
		}
		for _, entry := range entries {
			for _, key := range []string{"app_id_env", "app_secret_env", "verification_token_env", "encrypt_key_env"} {
				delete(entry, key)
			}
		}
		raw["connections"], _ = json.Marshal(entries)
	}
	normalized, _ := json.Marshal(raw)
	d := json.NewDecoder(strings.NewReader(string(normalized)))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return c, errors.New("配置格式无效或包含未知字段；不支持明文密钥")
	}
	var oldCodex map[string]json.RawMessage
	_ = json.Unmarshal(raw["codex"], &oldCodex)
	if _, hasMode := oldCodex["mode"]; !hasMode && (c.Codex.Home != "" || c.Codex.Model != "" || c.Codex.Binary != "" && c.Codex.Binary != "codex") {
		c.Codex.Mode = "custom"
	}
	if c.Codex.SubmissionMode == "" {
		c.Codex.SubmissionMode = "native_queue"
	}
	if c.Codex.SubmissionMode != "native_queue" && c.Codex.SubmissionMode != "desktop_queue" && c.Codex.SubmissionMode != "direct" {
		return c, errors.New("Codex 提交方式无效")
	}
	if c.Codex.Mode == "follow_local" {
		c.Codex.Home = ""
		c.Codex.Model = ""
		c.Codex.Binary = ""
	}
	return c, nil
}
func Load(path string) (Config, error) {
	f, e := os.Open(path)
	if e != nil {
		return Config{}, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil {
		return Config{}, e
	}
	c, e := Decode(data)
	if e != nil {
		return c, e
	}
	return Validate(c, filepath.Dir(path))
}

// Validate 返回运行时路径规范化副本，不修改环境变量，不读取密钥。
func Validate(c Config, base string) (Config, error) { return validate(c, base, true) }

// ValidateDraft 允许删除最后一个连接后保存；启动时仍要求至少一个连接。
func ValidateDraft(c Config, base string) (Config, error) { return validate(c, base, false) }
func validate(c Config, base string, requireConnections bool) (Config, error) {
	var e error
	base, e = filepath.Abs(base)
	if e != nil {
		return c, errors.New("配置基目录无效")
	}
	home, _ := os.UserHomeDir()
	resolve := func(p string) string {
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p)
	}
	if strings.TrimSpace(c.StateDir) == "" {
		return c, errors.New("应用数据目录尚未初始化，请重新打开客户端")
	}
	c.StateDir = resolve(c.StateDir)
	c.WorkDir = strings.TrimSpace(c.WorkDir)
	if c.WorkDir != "" {
		c.WorkDir = resolve(c.WorkDir)
		c.WorkDir, e = ExistingDirectory(c.WorkDir)
		if e != nil {
			return c, errors.New("可选的默认项目目录不存在或不可访问；可以清空该项，由每个新会话自动分配目录")
		}
	}
	if c.Codex.Mode == "" {
		c.Codex.Mode = "follow_local"
	}
	if c.Codex.Mode != "follow_local" && c.Codex.Mode != "custom" {
		return c, errors.New("Codex 模式无效")
	}
	if c.Codex.SubmissionMode == "" {
		c.Codex.SubmissionMode = "native_queue"
	}
	if c.Codex.SubmissionMode != "native_queue" && c.Codex.SubmissionMode != "desktop_queue" && c.Codex.SubmissionMode != "direct" {
		return c, errors.New("Codex 提交方式无效")
	}
	if c.Codex.Mode == "follow_local" {
		c.Codex.Home = ""
		c.Codex.Model = ""
		c.Codex.Binary = ""
	} else {
		if c.Codex.Home != "" {
			c.Codex.Home = resolve(c.Codex.Home)
		}
		if strings.ContainsAny(c.Codex.Binary, "/\\") || strings.HasPrefix(c.Codex.Binary, "~") {
			c.Codex.Binary = resolve(c.Codex.Binary)
		}
	}
	if c.Codex.Sandbox != "read-only" && c.Codex.Sandbox != "workspace-write" {
		return c, errors.New("不允许无沙箱模式")
	}
	if c.Codex.ApprovalPolicy != "untrusted" && c.Codex.ApprovalPolicy != "on-request" {
		return c, errors.New("approval_policy 仅支持 untrusted/on-request")
	}
	l := c.Limits
	if l.Workers < 1 || l.Workers > 16 || l.QueueSize < 1 || l.QueueSize > 512 || l.TurnTimeoutSeconds < 30 || l.TurnTimeoutSeconds > 7200 || (l.ApprovalTimeoutSeconds != 0 && (l.ApprovalTimeoutSeconds < 15 || l.ApprovalTimeoutSeconds > 1800)) || l.MaxMessageChars < 200 || l.MaxMessageChars > 3000 || l.MaxInputChars < 1 || l.MaxInputChars > 64000 || l.IdlePushMS < 1000 || l.IdlePushMS > 60000 {
		return c, errors.New("limits 超出允许范围")
	}
	if requireConnections && len(c.Connections) == 0 {
		return c, errors.New("至少配置一个 IM 连接")
	}
	ids := map[string]bool{}
	for i := range c.Connections {
		x := &c.Connections[i]
		if x.Type == "" {
			return c, errors.New("IM 连接 type 不能为空")
		}
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`).MatchString(x.ID) || ids[x.ID] {
			return c, errors.New("连接 ID 无效或重复")
		}
		ids[x.ID] = true
		if len(x.AllowedUsers) == 0 || len(x.AdminUsers) == 0 || len(x.AllowedUsers) > 500 || len(x.AdminUsers) > 49 {
			return c, errors.New("白名单必须为 1..500 人，管理员必须为 1..49 人，禁止默认全员开放")
		}
		for _, u := range append(append([]string{}, x.AllowedUsers...), x.AdminUsers...) {
			if strings.TrimSpace(u) == "" || len(u) > 256 || strings.ContainsAny(u, "\x00\r\n") {
				return c, errors.New("用户 ID 不能为空或含换行")
			}
		}
		for _, u := range x.AdminUsers {
			if !Contains(x.AllowedUsers, u) {
				return c, errors.New("管理员必须在用户白名单内")
			}
		}
		if x.ConversationMode == "" {
			x.ConversationMode = "group"
		}
		if x.ConversationMode != "group" && x.ConversationMode != "topic" {
			return c, errors.New("会话模式只能是独立群或共享群话题")
		}
		x.SharedChatID = strings.TrimSpace(x.SharedChatID)
		if len(x.SharedChatID) > 256 || strings.ContainsAny(x.SharedChatID, "\x00\r\n") {
			return c, errors.New("共享群 ID 格式无效")
		}
		if x.ChatNamePrefix == "" {
			x.ChatNamePrefix = "Codex"
		}
	}
	if c.Watch.Enabled {
		if !ids[c.Watch.ConnectionID] || (c.Watch.IntervalSeconds < 5 || c.Watch.IntervalSeconds > 3600) {
			return c, errors.New("watch 连接或轮询间隔无效")
		}
		for _, x := range c.Connections {
			if x.ID == c.Watch.ConnectionID && !x.AutoCreateChat {
				return c, errors.New("自动发现需要 auto_create_chat")
			}
		}
	}
	return c, nil
}
func Contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Credential 仅访问客户端 SQLite 解密后的连接快照，绝不回退环境变量。
func (c Connection) Credential(field string) (string, error) {
	if value := c.Credentials[field]; value != "" {
		return value, nil
	}
	return "", fmt.Errorf("请在客户端配置连接 %s 的 %s", c.ID, field)
}

// ExistingDirectory 核对会话实际目录，不把空路径当作进程当前目录。
func ExistingDirectory(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("会话工作目录为空或不是绝对路径")
	}
	resolved, e := filepath.EvalSymlinks(path)
	if e != nil {
		return "", errors.New("会话原工作目录不存在或不可访问")
	}
	st, e := os.Stat(resolved)
	if e != nil || !st.IsDir() {
		return "", errors.New("会话原工作目录无效")
	}
	return filepath.Clean(resolved), nil
}
