package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/payload"
	"im-agent/internal/config"
	"im-agent/internal/im"
)

const botTopic = "/v1.0/im/bot/messages/get"

// 复用官方 SDK 协议模型；v0.9.1 的自动重连使用 Background，无法随桥接停止。
// 连接生命周期在此受父 context 管理，复用项目已有 gorilla/websocket。
func (c *Client) Start(ctx context.Context, receive func(context.Context, im.Message) error, _ func(context.Context, im.Interaction) error) error {
	defer c.phase("stopped", "钉钉连接已停止")
	delay := time.Second
	for ctx.Err() == nil {
		c.phase("connecting", "正在连接钉钉 Stream")
		_ = c.stream(ctx, receive)
		if ctx.Err() != nil {
			break
		}
		c.phase("reconnecting", "钉钉连接中断，等待重连；请核对网络和应用配置")
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
	return ctx.Err()
}
func (c *Client) stream(parent context.Context, receive func(context.Context, im.Message) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var endpoint payload.ConnectionEndpointResponse
	request := payload.ConnectionEndpointRequest{ClientId: c.credential("app_id"), ClientSecret: c.credential("app_secret"), UserAgent: "im-agent", Subscriptions: []*payload.SubscriptionModel{{Type: "CALLBACK", Topic: botTopic}}}
	if err := c.post(ctx, "/v1.0/gateway/connections/open", "", request, &endpoint, false); err != nil {
		return err
	}
	u, err := url.Parse(endpoint.Endpoint)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Port() != "" && u.Port() != "443" || !(u.Hostname() == "dingtalk.com" || strings.HasSuffix(u.Hostname(), ".dingtalk.com")) || endpoint.Ticket == "" {
		return errors.New("钉钉返回的 Stream 地址无效")
	}
	q := u.Query()
	q.Set("ticket", endpoint.Ticket)
	u.RawQuery = q.Encode()
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	ws, resp, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return errors.New("钉钉 Stream 握手失败")
	}
	defer ws.Close()
	stop := context.AfterFunc(ctx, func() { ws.Close() })
	defer stop()
	ws.SetReadLimit(1 << 20)
	c.phase("connected", "钉钉 Stream 已连接；支持文本会话")
	var write sync.Mutex
	ack := func(f *payload.DataFrame, code int) {
		write.Lock()
		defer write.Unlock()
		r := payload.NewDataFrameResponse(code)
		r.SetHeader("messageId", f.GetMessageId())
		r.SetHeader("contentType", "application/json")
		if f.Type == "SYSTEM" {
			r.Data = f.Data
		}
		ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if ws.WriteJSON(r) != nil {
			cancel()
		}
	}
	// 回调只做持久收件和命令处理；有界并发防止慢数据库阻塞系统心跳。
	workers := make(chan struct{}, 8)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(180 * time.Second)) })
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(45 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					cancel()
					return
				}
			}
		}
	}()
	for ctx.Err() == nil {
		ws.SetReadDeadline(time.Now().Add(180 * time.Second))
		var f payload.DataFrame
		if err := ws.ReadJSON(&f); err != nil {
			return errors.New("钉钉 Stream 读取中断")
		}
		if f.Type == "SYSTEM" {
			ack(&f, 200)
			if f.GetTopic() == "disconnect" {
				return errors.New("钉钉请求重连")
			}
			continue
		}
		if f.Type != "CALLBACK" || f.GetTopic() != botTopic {
			ack(&f, 404)
			continue
		}
		select {
		case workers <- struct{}{}:
		default:
			ack(&f, 500)
			continue
		}
		wg.Add(1)
		go func(f payload.DataFrame) {
			defer wg.Done()
			defer func() { <-workers }()
			code := 200
			if c.receive(ctx, f.Data, receive) != nil {
				code = 500
			}
			ack(&f, code)
		}(f)
	}
	return ctx.Err()
}
func (c *Client) receive(ctx context.Context, raw string, receive func(context.Context, im.Message) error) error {
	var data struct {
		chatbot.BotCallbackDataModel
		RobotCode string `json:"robotCode"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil {
		return errors.New("钉钉消息格式无效")
	}
	if data.SenderCorpId != c.credential("corp_id") || data.ChatbotCorpId != c.credential("corp_id") || data.RobotCode != c.credential("robot_code") || !config.Contains(c.cfg.AllowedUsers, data.SenderStaffId) {
		c.mu.Lock()
		c.status.Rejected++
		c.mu.Unlock()
		return nil
	}
	if data.MsgId == "" || data.SenderStaffId == "" || data.ConversationId == "" {
		return errors.New("钉钉消息缺少身份字段")
	}
	a := c.Identity()
	direct := data.ConversationType == "1"
	if direct {
		a.ChatID = "user:" + data.SenderStaffId
	} else if data.ConversationType == "2" {
		if !data.IsInAtList {
			return nil
		}
		a.ChatID = "group:" + data.ConversationId
	} else {
		return nil
	}
	c.mu.Lock()
	c.status.Received++
	c.status.UpdatedAt = time.Now().Unix()
	c.mu.Unlock()
	if data.Msgtype != "text" {
		c.phase("connected", "已收到非文本消息；当前钉钉连接仅处理文本，请改为文字发送")
		return nil
	}
	text := strings.TrimSpace(data.Text.Content)
	if text == "" {
		return nil
	}
	return receive(ctx, im.Message{Address: a, ID: inboundID(c.cfg.ID, data.MsgId), UserID: data.SenderStaffId, Text: text, IsDirect: direct})
}
