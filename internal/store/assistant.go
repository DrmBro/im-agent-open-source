package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"im-agent/internal/im"
)

// 消息助理：主动拉取今日消息、按事项分流、复杂事项转会话，新消息排队等会话自取。

type AssistantTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type AssistantSettings struct {
	Enabled         bool              `json:"enabled"`
	Connection      string            `json:"connection"`
	Model           string            `json:"model"`
	IntervalSeconds int               `json:"interval_seconds"`
	People          []AssistantTarget `json:"people"`
	Groups          []AssistantTarget `json:"groups"`
	Mentions        bool              `json:"mentions"`
	SkipRobots      bool              `json:"skip_robots"`
	AutoSession     bool              `json:"auto_session"`
	AutoSend        bool              `json:"auto_send"`
	AutoDeliverIdle bool              `json:"auto_deliver_idle"`
	Profile         string            `json:"profile"`
	RuleNeedMe      string            `json:"rule_need_me"`
	RuleAutoReply   string            `json:"rule_auto_reply"`
	RuleNoReply     string            `json:"rule_no_reply"`
	AwaitHours      int               `json:"await_hours"`
}

func DefaultAssistantSettings() AssistantSettings {
	return AssistantSettings{IntervalSeconds: 120, Mentions: true, SkipRobots: true, AutoSession: true, AwaitHours: 4}
}

func (a *AssistantSettings) Normalize() error {
	a.AutoSend = false
	if a.IntervalSeconds == 0 {
		a.IntervalSeconds = 120
	}
	if a.AwaitHours == 0 {
		a.AwaitHours = 4
	}
	if a.AwaitHours < 1 || a.AwaitHours > 168 {
		return errors.New("等待回复超时需在 1 至 168 小时之间")
	}
	if a.IntervalSeconds < 30 || a.IntervalSeconds > 3600 {
		return errors.New("拉取间隔需在 30 至 3600 秒之间")
	}
	if len(a.People) > 100 || len(a.Groups) > 100 {
		return errors.New("关注的人和群各最多 100 个")
	}
	for _, list := range [][]AssistantTarget{a.People, a.Groups} {
		seen := map[string]bool{}
		for _, t := range list {
			if t.ID == "" || len(t.ID) > 256 || strings.ContainsAny(t.ID, "\x00\r\n ,@") || seen[t.ID] || len([]rune(t.Name)) > 100 {
				return errors.New("关注对象标识无效或重复")
			}
			seen[t.ID] = true
		}
	}
	for _, s := range []string{a.Profile, a.RuleNeedMe, a.RuleAutoReply, a.RuleNoReply} {
		if len([]rune(s)) > 2000 {
			return errors.New("分流规则每项最多 2000 字")
		}
	}
	if len(a.Model) > 128 || strings.ContainsAny(a.Model, "\x00\r\n\"") {
		return errors.New("模型名称无效")
	}
	return nil
}

type AssistantMessage struct {
	im.PulledMessage
	Kind     string `json:"kind"`
	ChatName string `json:"chat_name"`
	Peer     string `json:"peer,omitempty"`
	ItemID   string `json:"item_id,omitempty"`
	Number   int    `json:"-"`
}

type AssistantItem struct {
	ID           string `json:"id"`
	Day          string `json:"day"`
	Conversation string `json:"conversation"`
	Kind         string `json:"kind"`
	Peer         string `json:"peer,omitempty"`
	ChatName     string `json:"chat_name"`
	Title        string `json:"title"`
	Category     string `json:"category"` // need_me, auto_reply, no_reply
	Complex      bool   `json:"complex"`
	Reason       string `json:"reason"`
	Summary      string `json:"summary"`
	Reply        string `json:"reply,omitempty"`
	ReplyState   string `json:"reply_state,omitempty"` // '', sending, sent, unknown, failed
	ReplyError   string `json:"reply_error,omitempty"`
	State        string `json:"state"` // open, session, done, ignored
	ThreadID     string `json:"thread_id,omitempty"`
	SessionError string `json:"session_error,omitempty"`
	CreatedMs    int64  `json:"created_ms"`
	UpdatedMs    int64  `json:"updated_ms"`
	Queued       int    `json:"queued"`
	// 等待对方回复：主人发出问题或请求后，对方尚未答复。
	Awaiting      bool               `json:"awaiting"`
	AwaitingSince int64              `json:"awaiting_since,omitempty"`
	AwaitingWho   string             `json:"awaiting_who,omitempty"`
	Overdue       bool               `json:"overdue,omitempty"`
	Messages      []AssistantMessage `json:"messages,omitempty"`
}

type AssistantQueued struct {
	ID         int64  `json:"id"`
	ThreadID   string `json:"thread_id"`
	ItemID     string `json:"item_id"`
	Title      string `json:"title"`
	MessageID  string `json:"message_id"`
	ChatName   string `json:"chat_name"`
	SenderName string `json:"sender_name"`
	Text       string `json:"text"`
	CreatedMs  int64  `json:"created_ms"`
}

type AssistantFeedback struct {
	Sender   string `json:"sender"`
	ChatName string `json:"chat_name"`
	Text     string `json:"text"`
	From     string `json:"from"`
	To       string `json:"to"`
}

func (s *Store) migrateAssistant() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS assistant_settings(id INTEGER PRIMARY KEY CHECK(id=1), body TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS assistant_cursors(conversation TEXT PRIMARY KEY, kind TEXT NOT NULL, peer TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', since_ms INTEGER NOT NULL, pulled_at INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS assistant_messages(message_id TEXT PRIMARY KEY, conversation TEXT NOT NULL, kind TEXT NOT NULL, peer TEXT NOT NULL DEFAULT '', chat_name TEXT NOT NULL, sender_id TEXT NOT NULL, sender_name TEXT NOT NULL, text TEXT NOT NULL, quoted TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, self INTEGER NOT NULL, robot INTEGER NOT NULL, item_id TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0, retry_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS assistant_messages_pending ON assistant_messages(item_id, conversation, created_ms);
CREATE TABLE IF NOT EXISTS assistant_items(id TEXT PRIMARY KEY, day TEXT NOT NULL, conversation TEXT NOT NULL, kind TEXT NOT NULL, peer TEXT NOT NULL DEFAULT '', chat_name TEXT NOT NULL, title TEXT NOT NULL, category TEXT NOT NULL, complex INTEGER NOT NULL, reason TEXT NOT NULL, summary TEXT NOT NULL, reply TEXT NOT NULL DEFAULT '', reply_state TEXT NOT NULL DEFAULT '', reply_error TEXT NOT NULL DEFAULT '', reply_version INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, thread_id TEXT NOT NULL DEFAULT '', session_error TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, updated_ms INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS assistant_desktop_sessions(thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS assistant_items_day ON assistant_items(day, conversation, state);
CREATE TABLE IF NOT EXISTS assistant_item_messages(item_id TEXT NOT NULL, message_id TEXT NOT NULL, PRIMARY KEY(item_id, message_id));
CREATE TABLE IF NOT EXISTS assistant_queue(id INTEGER PRIMARY KEY AUTOINCREMENT, thread_id TEXT NOT NULL, item_id TEXT NOT NULL, message_id TEXT NOT NULL, consumed_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, UNIQUE(thread_id, message_id));
CREATE INDEX IF NOT EXISTS assistant_queue_thread ON assistant_queue(thread_id, consumed_at);
CREATE TABLE IF NOT EXISTS assistant_feedback(id INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT NOT NULL, chat_name TEXT NOT NULL, text TEXT NOT NULL, from_category TEXT NOT NULL, to_category TEXT NOT NULL, created_at INTEGER NOT NULL);`)
	if err != nil {
		return err
	}
	for _, col := range []string{"awaiting INTEGER NOT NULL DEFAULT 0", "awaiting_since INTEGER NOT NULL DEFAULT 0", "awaiting_who TEXT NOT NULL DEFAULT ''", "awaiting_notified INTEGER NOT NULL DEFAULT 0"} {
		name := strings.Fields(col)[0]
		var n int
		if err = s.db.QueryRow("SELECT count(*) FROM pragma_table_info('assistant_items') WHERE name=?", name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err = s.db.Exec("ALTER TABLE assistant_items ADD COLUMN " + col); err != nil {
				return err
			}
		}
	}
	_, err = s.db.Exec("CREATE INDEX IF NOT EXISTS assistant_items_awaiting ON assistant_items(awaiting, conversation)")
	return err
}

func (s *Store) AssistantSettings() (AssistantSettings, error) {
	out := DefaultAssistantSettings()
	var raw string
	err := s.db.QueryRow("SELECT body FROM assistant_settings WHERE id=1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(raw), &out)
	out.AutoSend = false // 兼容旧配置；发送须由主人明确触发。
	return out, err
}

func (s *Store) SaveAssistantSettings(a AssistantSettings) error {
	if err := a.Normalize(); err != nil {
		return err
	}
	raw, _ := json.Marshal(a)
	_, err := s.db.Exec(`INSERT INTO assistant_settings(id,body,updated_at) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body, updated_at=excluded.updated_at`, string(raw), time.Now().Unix())
	return err
}

// AssistantCursor 返回拉取起点；跨天自动从今日零点开始。
func (s *Store) AssistantCursor(conversation string, dayStartMs int64) int64 {
	var since int64
	_ = s.db.QueryRow("SELECT since_ms FROM assistant_cursors WHERE conversation=?", conversation).Scan(&since)
	if since < dayStartMs {
		return dayStartMs
	}
	return since
}

func (s *Store) SaveAssistantCursor(conversation, kind, peer, name string, since int64, pullErr string) error {
	_, err := s.db.Exec(`INSERT INTO assistant_cursors(conversation,kind,peer,name,since_ms,pulled_at,error) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(conversation) DO UPDATE SET kind=excluded.kind, peer=CASE WHEN excluded.peer<>'' THEN excluded.peer ELSE assistant_cursors.peer END, name=CASE WHEN excluded.name<>'' THEN excluded.name ELSE assistant_cursors.name END, since_ms=max(assistant_cursors.since_ms, excluded.since_ms), pulled_at=excluded.pulled_at, error=excluded.error`, conversation, kind, peer, name, since, time.Now().Unix(), pullErr)
	return err
}

type AssistantCursorView struct {
	Conversation string `json:"conversation"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	SinceMs      int64  `json:"since_ms"`
	PulledAt     int64  `json:"pulled_at"`
	Error        string `json:"error,omitempty"`
}

func (s *Store) AssistantCursors(dayStartMs int64) ([]AssistantCursorView, error) {
	rows, err := s.db.Query("SELECT conversation,kind,name,since_ms,pulled_at,error FROM assistant_cursors WHERE pulled_at>=? ORDER BY pulled_at DESC", dayStartMs/1000)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantCursorView{}
	for rows.Next() {
		var c AssistantCursorView
		if err = rows.Scan(&c.Conversation, &c.Kind, &c.Name, &c.SinceMs, &c.PulledAt, &c.Error); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutAssistantMessages 去重保存；返回新增条数。
func (s *Store) PutAssistantMessages(kind, peer, chatName string, msgs []im.PulledMessage) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, m := range msgs {
		r, err := tx.Exec(`INSERT INTO assistant_messages(message_id,conversation,kind,peer,chat_name,sender_id,sender_name,text,quoted,created_ms,self,robot) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(message_id) DO NOTHING`,
			m.MessageID, m.ConversationID, kind, peer, chatName, m.SenderID, m.SenderName, m.Text, m.Quoted, m.CreatedMs, boolInt(m.Self), boolInt(m.Robot))
		if err != nil {
			return 0, err
		}
		c, _ := r.RowsAffected()
		n += int(c)
	}
	return n, tx.Commit()
}

const assistantMessageColumns = "message_id,conversation,kind,peer,chat_name,sender_id,sender_name,text,quoted,created_ms,self,robot,item_id"

func scanAssistantMessage(row interface{ Scan(...any) error }) (AssistantMessage, error) {
	var m AssistantMessage
	var self, robot int
	err := row.Scan(&m.MessageID, &m.ConversationID, &m.Kind, &m.Peer, &m.ChatName, &m.SenderID, &m.SenderName, &m.Text, &m.Quoted, &m.CreatedMs, &self, &robot, &m.ItemID)
	m.Self, m.Robot = self != 0, robot != 0
	return m, err
}

// PendingAssistantConversations 列出有待分流消息的会话，失败退避中的除外。
func (s *Store) PendingAssistantConversations(dayStartMs int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT conversation FROM assistant_messages WHERE item_id='' AND created_ms>=? AND attempts<5 AND retry_at<=? GROUP BY conversation ORDER BY min(created_ms)`, dayStartMs, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err = rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PendingAssistantMessages(conversation string, dayStartMs int64, limit int) ([]AssistantMessage, error) {
	rows, err := s.db.Query("SELECT "+assistantMessageColumns+" FROM assistant_messages WHERE conversation=? AND item_id='' AND created_ms>=? AND attempts<5 ORDER BY created_ms, message_id LIMIT ?", conversation, dayStartMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantMessage{}
	for rows.Next() {
		m, err := scanAssistantMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeferAssistantMessages(ids []string) error {
	for _, id := range ids {
		if _, err := s.db.Exec("UPDATE assistant_messages SET attempts=attempts+1, retry_at=? WHERE message_id=? AND item_id=''", time.Now().Add(2*time.Minute).Unix(), id); err != nil {
			return err
		}
	}
	return nil
}

const assistantItemColumns = "id,day,conversation,kind,peer,chat_name,title,category,complex,reason,summary,reply,reply_state,reply_error,state,thread_id,session_error,created_ms,updated_ms,(SELECT count(*) FROM assistant_queue q WHERE q.item_id=assistant_items.id AND q.consumed_at=0),awaiting,awaiting_since,awaiting_who"

// 等待回复的事项跨天保留，最多回看 7 天。
const assistantAwaitWindowMs = 7 * 24 * 3600 * 1000

func scanAssistantItem(row interface{ Scan(...any) error }) (AssistantItem, error) {
	var x AssistantItem
	var complex, awaiting int
	err := row.Scan(&x.ID, &x.Day, &x.Conversation, &x.Kind, &x.Peer, &x.ChatName, &x.Title, &x.Category, &complex, &x.Reason, &x.Summary, &x.Reply, &x.ReplyState, &x.ReplyError, &x.State, &x.ThreadID, &x.SessionError, &x.CreatedMs, &x.UpdatedMs, &x.Queued, &awaiting, &x.AwaitingSince, &x.AwaitingWho)
	x.Complex, x.Awaiting = complex != 0, awaiting != 0
	return x, err
}

// OpenAssistantItems 是同一会话里仍可能承接新消息的事项（含已转会话的）。
// 包括前几天仍在等待对方回复的事项，便于今天的答复归回原事项。
func (s *Store) OpenAssistantItems(conversation, day string) ([]AssistantItem, error) {
	rows, err := s.db.Query("SELECT "+assistantItemColumns+" FROM assistant_items WHERE conversation=? AND ((day=? AND state IN ('open','session','done')) OR (awaiting=1 AND state<>'ignored' AND created_ms>=?)) ORDER BY updated_ms DESC LIMIT 20", conversation, day, time.Now().UnixMilli()-assistantAwaitWindowMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantItem{}
	for rows.Next() {
		x, err := scanAssistantItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) AssistantItem(id string) (AssistantItem, error) {
	x, err := scanAssistantItem(s.db.QueryRow("SELECT "+assistantItemColumns+" FROM assistant_items WHERE id=?", id))
	if err != nil {
		return x, err
	}
	x.Messages, err = s.assistantItemMessages(id)
	return x, err
}

func (s *Store) assistantItemMessages(id string) ([]AssistantMessage, error) {
	rows, err := s.db.Query("SELECT "+prefixed("m.", assistantMessageColumns)+" FROM assistant_item_messages l JOIN assistant_messages m ON m.message_id=l.message_id WHERE l.item_id=? ORDER BY m.created_ms, m.message_id LIMIT 200", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantMessage{}
	for rows.Next() {
		m, err := scanAssistantMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AssistantAssignment 是一次分流的原子结果。
type AssistantAssignment struct {
	Item     AssistantItem
	New      bool
	Messages []string
	// 已转会话的事项：新消息进入队列而非直接提交。
	Queue bool
}

func (s *Store) ApplyAssistantTriage(assign []AssistantAssignment, resolved []string, ignored []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, a := range assign {
		x := a.Item
		if a.New {
			_, err = tx.Exec(`INSERT INTO assistant_items(id,day,conversation,kind,peer,chat_name,title,category,complex,reason,summary,reply,state,created_ms,updated_ms,awaiting,awaiting_since,awaiting_who) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				x.ID, x.Day, x.Conversation, x.Kind, x.Peer, x.ChatName, x.Title, x.Category, boolInt(x.Complex), x.Reason, x.Summary, x.Reply, x.State, now, now, boolInt(x.Awaiting), awaitSince(x.Awaiting, now), x.AwaitingWho)
		} else if a.Queue {
			_, err = tx.Exec(`UPDATE assistant_items SET updated_ms=?, summary=CASE WHEN ?<>'' THEN ? ELSE summary END WHERE id=?`, now, x.Summary, x.Summary, x.ID)
		} else {
			// 已发送的回复不再覆盖；新消息使事项重新需要判断。
			_, err = tx.Exec(`UPDATE assistant_items SET title=?, category=?, complex=?, reason=?, summary=?, reply=CASE WHEN reply_state IN ('sending','unknown') THEN reply ELSE ? END, reply_state=CASE WHEN reply_state IN ('sending','unknown') THEN reply_state ELSE '' END, reply_error='', state=CASE WHEN state='session' THEN 'session' WHEN ?='no_reply' THEN 'done' ELSE 'open' END, updated_ms=? WHERE id=?`,
				x.Title, x.Category, boolInt(x.Complex), x.Reason, x.Summary, x.Reply, x.Category, now, x.ID)
		}
		if err != nil {
			return err
		}
		if !a.New {
			// 主人再次发问时重新计时；对方答复后清除等待。
			if _, err = tx.Exec(`UPDATE assistant_items SET awaiting_since=CASE WHEN ?=1 THEN ? ELSE 0 END, awaiting=?, awaiting_who=CASE WHEN ?=1 THEN ? ELSE '' END, awaiting_notified=CASE WHEN ?=1 THEN awaiting_notified ELSE 0 END WHERE id=?`,
				boolInt(x.Awaiting), now, boolInt(x.Awaiting), boolInt(x.Awaiting), x.AwaitingWho, boolInt(x.Awaiting), x.ID); err != nil {
				return err
			}
		}
		for _, id := range a.Messages {
			if _, err = tx.Exec("UPDATE assistant_messages SET item_id=? WHERE message_id=? AND item_id=''", x.ID, id); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO assistant_item_messages(item_id,message_id) VALUES(?,?) ON CONFLICT DO NOTHING", x.ID, id); err != nil {
				return err
			}
			if a.Queue && x.ThreadID != "" {
				if _, err = tx.Exec("INSERT INTO assistant_queue(thread_id,item_id,message_id,created_at) VALUES(?,?,?,?) ON CONFLICT(thread_id,message_id) DO NOTHING", x.ThreadID, x.ID, id, time.Now().Unix()); err != nil {
					return err
				}
			}
		}
	}
	for _, id := range ignored {
		if _, err = tx.Exec("UPDATE assistant_messages SET item_id='-' WHERE message_id=? AND item_id=''", id); err != nil {
			return err
		}
	}
	for _, id := range resolved {
		if _, err = tx.Exec("UPDATE assistant_items SET state='done', updated_ms=? WHERE id=? AND state='open'", now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type AssistantView struct {
	Day      string                `json:"day"`
	Items    []AssistantItem       `json:"items"`
	Cursors  []AssistantCursorView `json:"cursors"`
	Pending  int                   `json:"pending"`
	Reviews  []PersonalReview      `json:"reviews"`
	Settings AssistantSettings     `json:"settings"`
	Status   string                `json:"status"`
	LastRun  int64                 `json:"last_run"`
	Models   []string              `json:"models,omitempty"`
}

func awaitSince(on bool, now int64) int64 {
	if on {
		return now
	}
	return 0
}

func (s *Store) AssistantItems(day string) ([]AssistantItem, error) {
	rows, err := s.db.Query("SELECT "+assistantItemColumns+" FROM assistant_items WHERE day=? OR (awaiting=1 AND state<>'ignored' AND created_ms>=?) ORDER BY CASE state WHEN 'open' THEN 0 WHEN 'session' THEN 1 ELSE 2 END, updated_ms DESC LIMIT 300", day, time.Now().UnixMilli()-assistantAwaitWindowMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantItem{}
	for rows.Next() {
		x, err := scanAssistantItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Messages, err = s.assistantItemMessages(out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) PendingAssistantCount(dayStartMs int64) int {
	var n int
	_ = s.db.QueryRow("SELECT count(*) FROM assistant_messages WHERE item_id='' AND created_ms>=?", dayStartMs).Scan(&n)
	return n
}

func (s *Store) SetAssistantItemState(id, state string) error {
	if state != "open" && state != "done" && state != "ignored" {
		return errors.New("事项状态无效")
	}
	r, err := s.db.Exec("UPDATE assistant_items SET state=?, updated_ms=? WHERE id=? AND state<>'session'", state, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return errors.New("事项已转为会话或不存在")
	}
	return nil
}

func (s *Store) RecategorizeAssistantItem(id, category string) error {
	if category != "need_me" && category != "auto_reply" && category != "no_reply" {
		return errors.New("分类无效")
	}
	x, err := s.AssistantItem(id)
	if err != nil {
		return err
	}
	if x.Category == category {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE assistant_items SET category=?, updated_ms=? WHERE id=?", category, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	var sample, sender string
	for _, m := range x.Messages {
		if !m.Self {
			sender = m.SenderName
			sample += m.Text + "\n"
		}
	}
	if _, err = tx.Exec("INSERT INTO assistant_feedback(sender,chat_name,text,from_category,to_category,created_at) VALUES(?,?,?,?,?,?)", sender, x.ChatName, clipText(sample, 300), x.Category, category, time.Now().Unix()); err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM assistant_feedback WHERE id NOT IN (SELECT id FROM assistant_feedback ORDER BY id DESC LIMIT 200)")
	if err != nil {
		return err
	}
	return tx.Commit()
}

func clipText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func (s *Store) AssistantFeedback(limit int) ([]AssistantFeedback, error) {
	rows, err := s.db.Query("SELECT sender,chat_name,text,from_category,to_category FROM assistant_feedback ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantFeedback{}
	for rows.Next() {
		var f AssistantFeedback
		if err = rows.Scan(&f.Sender, &f.ChatName, &f.Text, &f.From, &f.To); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ClaimAssistantReply 将回复置为发送中并返回幂等版本；同一版本重复调用不会再次领取。
// followup=true 用于催办：允许在已发送后再发一条，但发送中或结果待核对时仍拒绝。
func (s *Store) ClaimAssistantReply(id, text string, followup bool) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var state string
	var version int64
	if err = tx.QueryRow("SELECT reply_state,reply_version FROM assistant_items WHERE id=?", id).Scan(&state, &version); err != nil {
		return 0, err
	}
	if state == "sending" || state == "unknown" || (state == "sent" && !followup) {
		return 0, errors.New("该回复已发送或结果待核对，不重复发送")
	}
	version++
	if _, err = tx.Exec("UPDATE assistant_items SET reply=?, reply_state='sending', reply_error='', reply_version=?, updated_ms=? WHERE id=?", text, version, time.Now().UnixMilli(), id); err != nil {
		return 0, err
	}
	return version, tx.Commit()
}

// await=true 表示这条已发送的消息在等对方答复，开始（或重新）计时。
func (s *Store) FinishAssistantReply(id, state, detail string, await bool) error {
	itemState := "open"
	if state == "sent" {
		itemState = "done"
	}
	_, err := s.db.Exec("UPDATE assistant_items SET reply_state=?, reply_error=?, state=CASE WHEN state='session' THEN state ELSE ? END, updated_ms=? WHERE id=?", state, detail, itemState, time.Now().UnixMilli(), id)
	if err == nil && state == "sent" && await {
		err = s.SetAssistantAwaiting(id, true, "")
	}
	return err
}

// SetAssistantAwaiting 手动或发送后切换等待状态；重新开始等待会重置超时提醒。
func (s *Store) SetAssistantAwaiting(id string, on bool, who string) error {
	now := time.Now().UnixMilli()
	r, err := s.db.Exec(`UPDATE assistant_items SET awaiting=?, awaiting_since=?, awaiting_notified=0, awaiting_who=CASE WHEN ?<>'' THEN ? WHEN ?=1 THEN awaiting_who ELSE '' END, updated_ms=? WHERE id=?`,
		boolInt(on), awaitSince(on, now), who, who, boolInt(on), now, id)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return errors.New("事项不存在")
	}
	return nil
}

// ClaimOverdueAwaiting 领取超时仍未获答复、尚未提醒过的事项，每轮等待只提醒一次。
func (s *Store) ClaimOverdueAwaiting(beforeMs int64) ([]AssistantItem, error) {
	rows, err := s.db.Query("SELECT "+assistantItemColumns+" FROM assistant_items WHERE awaiting=1 AND state<>'ignored' AND awaiting_since>0 AND awaiting_since<? AND awaiting_notified=0 LIMIT 20", beforeMs)
	if err != nil {
		return nil, err
	}
	var out []AssistantItem
	for rows.Next() {
		x, err := scanAssistantItem(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, x)
	}
	rows.Close()
	for _, x := range out {
		if _, err = s.db.Exec("UPDATE assistant_items SET awaiting_notified=1 WHERE id=? AND awaiting_since=?", x.ID, x.AwaitingSince); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

func (s *Store) ResetAssistantReply(id string) error {
	_, err := s.db.Exec("UPDATE assistant_items SET reply_state='', reply_error='' WHERE id=? AND reply_state IN ('unknown','failed')", id)
	return err
}

func (s *Store) LinkAssistantSession(id, thread, failure string) error {
	if thread != "" {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("UPDATE assistant_items SET state='session', thread_id=?, session_error='', updated_ms=? WHERE id=?", thread, time.Now().UnixMilli(), id); err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err := s.db.Exec("UPDATE assistant_items SET session_error=?, updated_ms=? WHERE id=?", failure, time.Now().UnixMilli(), id)
	return err
}

func (s *Store) AssistantItemsNeedingSession(day string) ([]AssistantItem, error) {
	rows, err := s.db.Query("SELECT "+assistantItemColumns+" FROM assistant_items WHERE day=? AND state='open' AND complex=1 AND category='need_me' AND thread_id='' AND session_error='' ORDER BY created_ms LIMIT 3", day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantItem{}
	for rows.Next() {
		x, err := scanAssistantItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AssistantQueue 读取会话的待读新消息；consume=true 时在同一事务标记已读。
func (s *Store) AssistantQueue(ctx context.Context, thread string, consume bool) ([]AssistantQueued, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT q.id,q.thread_id,q.item_id,coalesce(i.title,''),q.message_id,m.chat_name,m.sender_name,m.text,m.created_ms FROM assistant_queue q
JOIN assistant_messages m ON m.message_id=q.message_id LEFT JOIN assistant_items i ON i.id=q.item_id
WHERE q.thread_id=? AND q.consumed_at=0 ORDER BY m.created_ms, q.id LIMIT 200`, thread)
	if err != nil {
		return nil, err
	}
	out := []AssistantQueued{}
	for rows.Next() {
		var q AssistantQueued
		if err = rows.Scan(&q.ID, &q.ThreadID, &q.ItemID, &q.Title, &q.MessageID, &q.ChatName, &q.SenderName, &q.Text, &q.CreatedMs); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, q)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if consume {
		for _, q := range out {
			if _, err = tx.ExecContext(ctx, "UPDATE assistant_queue SET consumed_at=? WHERE id=? AND consumed_at=0", time.Now().Unix(), q.ID); err != nil {
				return nil, err
			}
		}
	}
	return out, tx.Commit()
}

// AssistantQueuedThreads 列出有未读新消息的会话。
func (s *Store) AssistantQueuedThreads() ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT thread_id FROM assistant_queue WHERE consumed_at=0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err = rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ThreadBusy 判断会话是否仍有桥接排队或执行中的请求。
func (s *Store) ThreadBusy(thread string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM inbound_messages i JOIN events e ON e.id=i.event_id WHERE i.thread_id=? AND e.status='processing'`, thread).Scan(&n)
	return n > 0, err
}

func (s *Store) PendingPersonalReviewList(limit int) ([]PersonalReview, error) {
	rows, err := s.db.Query("SELECT "+reviewFields+" FROM personal_reviews WHERE state IN ('pending','unknown') ORDER BY updated_at DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PersonalReview{}
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func prefixed(p, columns string) string {
	parts := strings.Split(columns, ",")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ",")
}

// AssistantContext 返回该会话今日已归类的最近消息，供分流理解上下文。
func (s *Store) AssistantContext(conversation string, dayStartMs int64, limit int) ([]AssistantMessage, error) {
	rows, err := s.db.Query("SELECT "+assistantMessageColumns+" FROM (SELECT * FROM assistant_messages WHERE conversation=? AND item_id<>'' AND created_ms>=? ORDER BY created_ms DESC LIMIT ?) ORDER BY created_ms", conversation, dayStartMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantMessage{}
	for rows.Next() {
		m, err := scanAssistantMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AssistantPeerConversation 返回此前拉取时记录的单聊会话 ID。
func (s *Store) AssistantPeerConversation(peer string) string {
	var c string
	_ = s.db.QueryRow("SELECT conversation FROM assistant_cursors WHERE peer=? AND kind='direct' ORDER BY pulled_at DESC LIMIT 1", peer).Scan(&c)
	return c
}

func (s *Store) PersonalInboxState(id string) (string, error) {
	var x string
	err := s.db.QueryRow("SELECT state FROM personal_inbox WHERE id=?", id).Scan(&x)
	return x, err
}

// AwaitingConversations 返回仍在等待对方答复的会话，供跨天继续拉取。
func (s *Store) AwaitingConversations(sinceMs int64) ([]AssistantItem, error) {
	rows, err := s.db.Query("SELECT conversation,kind,peer,chat_name FROM assistant_items WHERE awaiting=1 AND state<>'ignored' AND created_ms>=? GROUP BY conversation", sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AssistantItem{}
	for rows.Next() {
		var x AssistantItem
		if err = rows.Scan(&x.Conversation, &x.Kind, &x.Peer, &x.ChatName); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// AssistantSenders 在本机已拉取的消息中按姓名查找发送人，不访问钉钉。
func (s *Store) AssistantSenders(query string, limit int) ([]im.Person, error) {
	rows, err := s.db.Query(`SELECT sender_id, sender_name FROM assistant_messages WHERE self=0 AND robot=0 AND sender_id<>'' AND instr(lower(sender_name), lower(?))>0
GROUP BY sender_id ORDER BY max(created_ms) DESC LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []im.Person{}
	for rows.Next() {
		var p im.Person
		if err = rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		p.Title = "最近联系过"
		out = append(out, p)
	}
	return out, rows.Err()
}
