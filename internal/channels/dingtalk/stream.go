package dingtalk

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"im-agent/internal/im"
)

type streamFrame struct {
	Type    string            `json:"type"`
	Headers map[string]string `json:"headers"`
	Data    string            `json:"data"`
}

// Start confirms intake only after the callback has committed the inbound message.
func (c *Client) Start(ctx context.Context, receive func(context.Context, im.Message) error, _ func(context.Context, im.Interaction) error) error {
	backoff := 3 * time.Second
	for ctx.Err() == nil {
		c.setStatus("connecting", "正在连接钉钉 Stream")
		connected, e := c.connect(ctx, receive)
		if ctx.Err() != nil {
			break
		}
		if e != nil {
			c.setStatus("disconnected", "钉钉 Stream 已断开；请检查凭证、Stream 配置和网络，正在重连")
		}
		if connected {
			backoff = 3 * time.Second
		}
		timer := time.NewTimer(backoff + time.Duration(rand.Int64N(int64(backoff/5)+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if backoff < time.Minute {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
	c.setStatus("disconnected", "钉钉 Stream 已停止")
	return nil
}
func (c *Client) connect(ctx context.Context, receive func(context.Context, im.Message) error) (bool, error) {
	var endpoint struct {
		Endpoint string `json:"endpoint"`
		Ticket   string `json:"ticket"`
	}
	if e := c.post(ctx, "/v1.0/gateway/connections/open", "", map[string]any{"clientId": c.appID, "clientSecret": c.secret, "ua": "im-agent/0.1", "subscriptions": []map[string]string{{"type": "SYSTEM", "topic": "ping"}, {"type": "SYSTEM", "topic": "disconnect"}, {"type": "CALLBACK", "topic": messageTopic}}}, &endpoint, false); e != nil {
		return false, e
	}
	u, e := url.Parse(endpoint.Endpoint)
	if e != nil || u.Scheme != "wss" || u.User != nil || !(u.Hostname() == "dingtalk.com" || strings.HasSuffix(u.Hostname(), ".dingtalk.com")) || endpoint.Ticket == "" {
		return false, errors.New("钉钉 Stream 接入地址无效")
	}
	q := u.Query()
	q.Set("ticket", endpoint.Ticket)
	u.RawQuery = q.Encode()
	return c.readConnection(ctx, u.String(), receive)
}
func (c *Client) readConnection(ctx context.Context, endpoint string, receive func(context.Context, im.Message) error) (bool, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 20 * time.Second, Proxy: http.ProxyFromEnvironment}
	conn, resp, e := dialer.DialContext(ctx, endpoint, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if e != nil {
		return false, errors.New("钉钉 Stream 握手失败")
	}
	defer conn.Close()
	conn.SetReadLimit(1 << 20)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	closed := make(chan struct{})
	defer close(closed)
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(150 * time.Second)) })
	go func() {
		ticker := time.NewTicker(45 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	if len(c.cfg.AllowedUsers) == 0 {
		c.setStatus("connected", "钉钉身份识别模式已连接；群内 @机器人 /whoami 获取 UserID，未启用问答")
	} else {
		c.setStatus("connected", "钉钉群问答已连接；直接 @提问，引用回答续聊")
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(150 * time.Second))
		var frame streamFrame
		if e = conn.ReadJSON(&frame); e != nil {
			return true, errors.New("钉钉 Stream 读取中断")
		}
		code, data := 200, ""
		switch frame.Type {
		case "SYSTEM":
			if frame.Headers["topic"] == "ping" {
				data = frame.Data
			}
		case "CALLBACK":
			if frame.Headers["topic"] != messageTopic {
				code = 404
				break
			}
			msg, ok, parseErr := c.decodeMessage([]byte(frame.Data))
			if parseErr != nil {
				c.count(true)
				break
			}
			if !ok {
				c.count(true)
				break
			}
			if isIdentityCommand(msg.Text) {
				e = c.replyIdentity(ctx, msg)
			} else {
				e = receive(ctx, msg)
			}
			if e != nil {
				code = 500
			} else {
				c.count(false)
			}
		default:
			code = 404
		}
		ack := struct {
			Code    int               `json:"code"`
			Headers map[string]string `json:"headers"`
			Message string            `json:"message"`
			Data    string            `json:"data"`
		}{code, map[string]string{"messageId": frame.Headers["messageId"], "contentType": "application/json"}, "ok", data}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if e = conn.WriteJSON(ack); e != nil {
			return true, errors.New("钉钉 Stream 回执中断")
		}
		if frame.Type == "SYSTEM" && frame.Headers["topic"] == "disconnect" {
			return true, errors.New("钉钉请求重新连接")
		}
	}
}
