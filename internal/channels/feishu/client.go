// Package feishu 封装飞书协议；不把 SDK 类型暴露给桥接核心。
package feishu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/redact"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	callback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

const apiBase = "https://open.feishu.cn/open-apis"

type Client struct {
	cfg              config.Connection
	appID, appSecret string
	http             *http.Client
	tokenMu          sync.Mutex
	token            string
	tokenUntil       time.Time
	closedHandler    func(context.Context, im.ConversationClosed) error
	statusMu         sync.Mutex
	status           im.ConnectionStatus
}

func New(c config.Connection) (*Client, error) {
	id, e := c.Credential("app_id")
	if e != nil {
		return nil, e
	}
	secret, e := c.Credential("app_secret")
	if e != nil {
		return nil, e
	}
	return &Client{cfg: c, appID: id, appSecret: secret, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Identity() im.Address { return im.Address{Channel: "feishu", Connection: c.cfg.ID} }
func (c *Client) Capabilities() im.Capabilities {
	return im.Capabilities{CreateConversation: true, UpdateCard: true, InteractiveCards: true, StreamCards: false, CloseConversation: true, RenameConversation: true, Topics: true, SelectMenus: true}
}
func (c *Client) Start(ctx context.Context, onMessage func(context.Context, im.Message) error, onAction func(context.Context, im.Interaction) error) error {
	c.setStatus("connecting", "正在建立飞书长连接，尚未确认成功")
	verification, encrypt := "", ""
	verification, _ = c.cfg.Credential("verification_token")
	encrypt, _ = c.cfg.Credential("encrypt_key")
	d := dispatcher.NewEventDispatcher(verification, encrypt)
	d.Config.Logger = safeLogger{}

	d.OnP2ChatDisbandedV1(func(_ context.Context, event *larkim.P2ChatDisbandedV1) error {
		if event == nil || event.Event == nil || c.closedHandler == nil {
			return nil
		}
		a := c.Identity()
		a.ChatID = str(event.Event.ChatId)
		if a.ChatID == "" {
			return nil
		}
		id, actor := "", ""
		if event.EventV2Base != nil && event.EventV2Base.Header != nil {
			id = event.EventV2Base.Header.EventID
		}
		if event.Event.OperatorId != nil {
			actor = str(event.Event.OperatorId.OpenId)
		}
		return c.closedHandler(ctx, im.ConversationClosed{Address: a, EventID: id, ActorID: actor})
	})
	d.OnP2MessageReceiveV1(func(_ context.Context, e *larkim.P2MessageReceiveV1) error {
		if e == nil || e.Event == nil || e.Event.Message == nil || e.Event.Sender == nil {
			return nil
		}
		m, s := e.Event.Message, e.Event.Sender
		if str(s.SenderType) != "user" || s.SenderId == nil {
			c.setStatus("connected", "已收到非用户消息，已忽略")
			return nil
		}
		kind := str(m.ChatType)
		if kind != "p2p" && kind != "group" {
			c.setStatus("connected", "已收到不支持的会话类型（"+kind+"），已忽略")
			return nil
		}
		uid := str(s.SenderId.OpenId)
		rejected := !config.Contains(c.cfg.AllowedUsers, uid)
		c.recordMessage(rejected)
		if rejected {
			return nil
		}
		a := c.Identity()
		a.ChatID = str(m.ChatId)
		a.TopicID = str(m.ThreadId)
		if a.IsTopic() {
			a.RootMessageID = str(m.RootId)
			if a.RootMessageID == "" {
				a.RootMessageID = str(m.MessageId)
			}
		}
		var mentions []string
		for _, mention := range m.Mentions {
			if mention != nil {
				mentions = append(mentions, str(mention.Key))
			}
		}
		message, reason := decodeInbound(a, str(m.MessageId), uid, str(m.MessageType), str(m.Content), mentions, kind == "p2p")
		if reason != "" {
			c.setStatus("connected", "消息格式或身份无法解析，未提交模型")
			if reason == "unsupported_content" && a.ChatID != "" && str(m.MessageId) != "" {
				_, _ = c.SendText(ctx, a, "消息格式暂不支持，未提交 Codex。请使用文字、图片或文件；图文每组最多5张图片。", str(m.MessageId)+":format-error")
			}
			return nil
		}
		return onMessage(ctx, message)
	})
	d.OnP2CardActionTrigger(func(_ context.Context, e *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		reject := func() *callback.CardActionTriggerResponse {
			return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "error", Content: "无权操作、已处理或该卡片已过期"}}
		}
		if e == nil || e.Event == nil || e.Event.Action == nil || e.Event.Operator == nil || e.Event.Context == nil {
			return reject(), nil
		}
		v := e.Event
		uid := v.Operator.OpenID
		if !config.Contains(c.cfg.AllowedUsers, uid) {
			return reject(), nil
		}
		id, _ := v.Action.Value["action_id"].(string)
		choice, _ := v.Action.Value["choice"].(string)
		if choice == "" {
			choice = v.Action.Option
		}
		a := c.Identity()
		a.ChatID = v.Context.OpenChatID
		if id == "" || a.ChatID == "" || v.Context.OpenMessageID == "" {
			return reject(), nil
		}
		fields := map[string]string{}
		for key, value := range v.Action.FormValue {
			text, ok := value.(string)
			if !ok || len(text) > 32000 || len(fields) >= 8 {
				return reject(), nil
			}
			fields[key] = text
		}
		// 核心仅做本机校验和排队，不在回调内等待建群或模型。
		if e := onAction(ctx, im.Interaction{Fields: fields, Address: a, MessageID: v.Context.OpenMessageID, UserID: uid, ActionID: id, Choice: choice}); e != nil {
			if strings.HasPrefix(id, "new:") || strings.HasPrefix(id, "session:") || strings.HasPrefix(id, "session-form:") {
				return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "error", Content: redact.Clip(diagnostics.Error(e), 100)}}, nil
			}
			return reject(), nil
		}
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "success", Content: "选择已提交"}}, nil
	})
	w := larkws.NewClient(c.appID, c.appSecret,
		larkws.WithEventHandler(d), larkws.WithLogger(safeLogger{}), larkws.WithLogLevel(larkcore.LogLevelWarn), larkws.WithHttpClient(c.http),
		larkws.WithOnReady(func() { c.setStatus("connected", "飞书长连接已建立；等待消息或自动同步") }),
		larkws.WithOnReconnected(func() { c.setStatus("connected", "飞书长连接已重新建立") }),
		larkws.WithOnReconnecting(func() { c.setStatus("reconnecting", "连接尚未建立或已断开，正在重连") }),
		larkws.WithOnDisconnected(func() { c.setStatus("disconnected", "飞书连接已断开") }),
		larkws.WithOnError(func(err error) {
			message := "飞书连接失败，请检查凭证、网络与应用发布状态"
			var clientError *larkws.ClientError
			var serverError *larkws.ServerError
			if errors.As(err, &clientError) {
				message += fmt.Sprintf("（code=%d）", clientError.Code)
			} else if errors.As(err, &serverError) {
				message += fmt.Sprintf("（code=%d）", serverError.Code)
			}
			c.setStatus("failed", message)
		}),
	)
	err := w.Start(ctx)
	if ctx.Err() != nil {
		c.setStatus("stopped", "连接已停止")
	} else if err != nil {
		c.setStatus("failed", "飞书连接退出，请检查应用凭证与网络")
	}
	return err
}
func str(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

type safeLogger struct{}

func (safeLogger) Debug(context.Context, ...interface{}) {}
func (safeLogger) Info(context.Context, ...interface{})  {}
func (safeLogger) Warn(context.Context, ...interface{}) {
	slog.Warn("飞书 SDK 警告（原始数据已隐藏）")
}
func (safeLogger) Error(context.Context, ...interface{}) {
	slog.Error("飞书 SDK 错误，请检查凭证、权限与网络（原始数据已隐藏）")
}

type envelope struct {
	Code   int             `json:"code"`
	Data   json.RawMessage `json:"data"`
	Token  string          `json:"tenant_access_token"`
	Expire int64           `json:"expire"`
}

func (c *Client) do(ctx context.Context, method, path, token string, body any) (envelope, error) {
	var out envelope
	b, e := json.Marshal(body)
	if e != nil {
		return out, e
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, apiBase+path, bodyReader)
	if e != nil {
		return out, errors.New("飞书请求构建失败")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := c.http.Do(req)
	if e != nil {
		return out, errors.New("飞书网络请求失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, &apiError{httpStatus: resp.StatusCode}
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if e != nil || len(data) > 2<<20 || json.Unmarshal(data, &out) != nil {
		return out, errors.New("飞书响应格式或大小无效")
	}
	if out.Code != 0 {
		return out, &apiError{code: out.Code}
	}
	return out, nil
}
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenUntil) {
		return c.token, nil
	}
	v, e := c.do(ctx, "POST", "/auth/v3/tenant_access_token/internal", "", map[string]string{"app_id": c.appID, "app_secret": c.appSecret})
	if e != nil {
		return "", e
	}
	if v.Token == "" || v.Expire <= 0 {
		return "", errors.New("飞书访问令牌无效")
	}
	c.token = v.Token
	c.tokenUntil = time.Now().Add(time.Duration(v.Expire)*time.Second - 60*time.Second)
	return c.token, nil
}
func (c *Client) request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	token, e := c.accessToken(ctx)
	if e != nil {
		return nil, e
	}
	v, e := c.do(ctx, method, path, token, body)
	return v.Data, e
}
func (c *Client) CreateConversation(ctx context.Context, v im.Conversation) (im.Address, error) {
	return c.createConversation(ctx, v, "chat")
}
func (c *Client) createConversation(ctx context.Context, v im.Conversation, messageType string) (im.Address, error) {
	if len(v.Members) == 0 || len(v.Members) > 50 {
		return im.Address{}, errors.New("建群成员数量必须为 1..50")
	}
	// 群主设为会话创建者，便于用户在飞书里真正解散；机器人仍是创建者。
	// 机器人自动解散需 im:chat:operate_as_owner 权限，不能通过改权限绕过飞书检查。
	body := map[string]any{"name": redact.Clip(v.Name, 55), "user_id_list": v.Members, "chat_mode": "group", "group_message_type": messageType, "chat_type": "private", "add_member_permission": "only_owner", "share_card_permission": "not_allowed"}
	if v.Owner != "" {
		body["owner_id"] = v.Owner
	}
	raw, e := c.request(ctx, "POST", "/im/v1/chats?user_id_type=open_id&set_bot_manager=true", body)
	if e != nil {
		var platform *apiError
		if errors.As(e, &platform) && (platform.code != 0 || platform.httpStatus >= 400 && platform.httpStatus < 500) {
			return im.Address{}, e
		}
		return im.Address{}, &im.DeliveryUncertain{Cause: e}
	}
	var data struct {
		ID string `json:"chat_id"`
	}
	if json.Unmarshal(raw, &data) != nil || data.ID == "" {
		return im.Address{}, &im.DeliveryUncertain{Cause: errors.New("飞书未返回群聊 ID")}
	}
	a := c.Identity()
	a.ChatID = data.ID
	return a, nil
}
func (c *Client) send(ctx context.Context, a im.Address, kind, content, id string) (string, error) {
	if a.Channel != "feishu" || a.Connection != c.cfg.ID || a.ChatID == "" {
		return "", errors.New("消息目标与连接不匹配")
	}
	body := map[string]any{"receive_id": a.ChatID, "msg_type": kind, "content": content}
	if id != "" {
		// 同一个入站消息会产生回执、结果和错误等多条输出，不能复用同一个平台 UUID。
		// 相同目标/类型/逻辑键/内容的真正重试仍使用稳定 UUID。
		sum := sha256.Sum256([]byte(a.Key() + "\x00" + kind + "\x00" + id + "\x00" + content))
		body["uuid"] = hex.EncodeToString(sum[:16])
	}
	token, e := c.accessToken(ctx)
	if e != nil {
		return "", e
	}
	path := "/im/v1/messages?receive_id_type=chat_id"
	if a.IsTopic() {
		if a.RootMessageID == "" {
			return "", errors.New("话题缺少回复目标，不降级发送到群主界面")
		}
		path = "/im/v1/messages/" + url.PathEscape(a.RootMessageID) + "/reply"
		delete(body, "receive_id")
		body["reply_in_thread"] = true
	}
	result, e := c.do(ctx, "POST", path, token, body)
	if e != nil {
		var platform *apiError
		if errors.As(e, &platform) && (platform.code != 0 || platform.httpStatus >= 400 && platform.httpStatus < 500) {
			return "", e
		}
		return "", &im.DeliveryUncertain{Cause: e}
	}
	var v struct {
		ID string `json:"message_id"`
	}
	if json.Unmarshal(result.Data, &v) != nil || v.ID == "" {
		return "", &im.DeliveryUncertain{Cause: errors.New("飞书未返回消息 ID")}
	}

	return v.ID, nil
}
func (c *Client) SendText(ctx context.Context, a im.Address, text, key string) (string, error) {
	return c.send(ctx, a, "text", marshal(map[string]string{"text": text}), key)
}
func (c *Client) SendCard(ctx context.Context, a im.Address, card im.Card, key string) (string, error) {
	return c.send(ctx, a, "interactive", cardPayload(card), key)
}
func (c *Client) UpdateCard(ctx context.Context, a im.Address, id string, card im.Card) error {
	if a.Channel != "feishu" || a.Connection != c.cfg.ID || id == "" {
		return errors.New("卡片目标无效")
	}
	_, e := c.request(ctx, "PATCH", "/im/v1/messages/"+url.PathEscape(id), map[string]string{"content": cardPayload(card)})
	return e
}
func marshal(v any) string { b, _ := json.Marshal(v); return string(b) }
func cardPayload(c im.Card) string {
	elements := []any{map[string]any{"tag": "div", "text": map[string]any{"tag": "plain_text", "content": redact.Clip(c.Text, 7000)}}}
	if c.Form != nil {
		elements = append(elements, formElements(c)...)
	}
	if c.Selection != nil && len(c.Selection.Options) > 0 {
		options := []any{}
		for _, o := range c.Selection.Options {
			options = append(options, map[string]any{"text": map[string]string{"tag": "plain_text", "content": redact.Clip(o.Label, 100)}, "value": o.Value})
		}
		elements = append(elements, map[string]any{"tag": "action", "actions": []any{map[string]any{"tag": "select_static", "placeholder": map[string]string{"tag": "plain_text", "content": c.Selection.Placeholder}, "options": options, "value": map[string]string{"action_id": c.ActionID}}}})
	}
	// 每组至多三个按钮，适配移动端；按钮值只是随机动作 ID 和服务端定义的选择键。
	for i := 0; i < len(c.Buttons); i += 3 {
		end := i + 3
		if end > len(c.Buttons) {
			end = len(c.Buttons)
		}
		buttons := []any{}
		for _, b := range c.Buttons[i:end] {
			kind := "primary"
			if b.Destructive {
				kind = "danger"
			}
			buttons = append(buttons, map[string]any{"tag": "button", "text": map[string]any{"tag": "plain_text", "content": redact.Clip(b.Label, 40)}, "type": kind, "value": map[string]string{"action_id": c.ActionID, "choice": b.Value}})
		}
		elements = append(elements, map[string]any{"tag": "action", "actions": buttons})
	}
	return marshal(map[string]any{"config": map[string]bool{"wide_screen_mode": true, "update_multi": true}, "header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": redact.Clip(c.Title, 60)}, "template": "blue"}, "elements": elements})
}

// Factory 让上层通过渠道注册表装配飞书，不依赖 Client 具体类型。
func Factory(c config.Connection) (im.Adapter, error) { return New(c) }

// apiError 不保留平台原始错误体或请求内容。
type apiError struct{ httpStatus, code int }

func (e *apiError) Error() string {
	if e.code != 0 {
		return fmt.Sprintf("飞书业务错误 code=%d（请检查权限与发布状态）", e.code)
	}
	return fmt.Sprintf("飞书 HTTP %d", e.httpStatus)
}
