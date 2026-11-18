package dwspersonal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"im-agent/internal/im"
	"im-agent/internal/store"
)

const singleEvent = "user_im_message_receive_o2o"
const allDirectEvent = "user_im_message_receive_o2o_all"
const mentionEvent = "user_im_message_receive_at"

type subscription struct{ event, peer, group string }

func (s subscription) key() string { return s.event + ":" + s.peer }

func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (c *Client) Start(ctx context.Context, receive func(context.Context, im.Message) error, _ func(context.Context, im.Interaction) error) error {
	defer c.phase("stopped", "钉钉个人账号监听已停止")
	stopDirectory := c.startDirectoryJobs(ctx)
	defer stopDirectory()
	var subscriptions []subscription
	if c.cfg.PersonalAllDirect {
		subscriptions = append(subscriptions, subscription{event: allDirectEvent})
	} else {
		for _, peer := range c.cfg.AllowedUsers {
			if peer != c.self {
				subscriptions = append(subscriptions, subscription{event: singleEvent, peer: peer})
			}
		}
	}
	if c.cfg.PersonalMentions {
		subscriptions = append(subscriptions, subscription{event: mentionEvent})
	}
	if len(subscriptions) == 0 {
		c.phase("waiting_config", "请选择个人消息监听范围或添加联系人 openDingtalkId")
		<-ctx.Done()
		return ctx.Err()
	}
	for !c.authenticated(ctx) {
		c.phase("waiting_auth", "DWS 未登录或账号不可用；请先完成本人授权")
		if !pause(ctx, 15*time.Second) {
			return ctx.Err()
		}
	}
	var wg sync.WaitGroup
	for _, sub := range subscriptions {
		sub := sub
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				c.peerReady(sub.key(), false, len(subscriptions))
				_ = c.listen(ctx, sub, len(subscriptions), receive)
				c.peerReady(sub.key(), false, len(subscriptions))
				if !pause(ctx, 10*time.Second) {
					return
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}
func (c *Client) peerReady(peer string, ready bool, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready[peer] = ready
	n := 0
	for _, value := range c.ready {
		if value {
			n++
		}
	}
	c.status.Phase = "connecting"
	c.status.Message = "正在建立个人消息订阅；未就绪时请检查 DWS 授权和权限"
	if n == total {
		c.status.Phase = "connected"
		c.status.Message = "个人消息监听已就绪；默认生成待审草稿，由本人确认后回复"
	}
	c.status.UpdatedAt = time.Now().Unix()
}

// stderr 仅识别就绪标记，不记录完整错误、个人信息或令牌。
type readyWriter struct {
	line    []byte
	onReady func()
}

func (w *readyWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			if strings.Contains(string(w.line), "[event] ready") {
				w.onReady()
			}
			w.line = nil
		} else if len(w.line) < 4096 {
			w.line = append(w.line, b)
		}
	}
	return len(p), nil
}

func (c *Client) listen(parent context.Context, sub subscription, total int, receive func(context.Context, im.Message) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	args := []string{"event", "consume", sub.event, "--flatten", "--format", "ndjson"}
	if sub.peer != "" {
		args = append(args, "--open-dingtalk-id", sub.peer)
	}
	if sub.group != "" {
		args = append(args, "--group", sub.group)
	}
	cmd := c.command(ctx, args...)
	// DWS 支持关闭 stdin 后优雅退出；保持 pipe 存活直到本次订阅结束。
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = &readyWriter{onReady: func() {
		if sub.event == groupEvent {
			c.groupReady(sub.group, true)
		} else {
			c.peerReady(sub.key(), true, total)
		}
	}}
	if err = cmd.Start(); err != nil {
		return errors.New("无法启动 DWS 监听")
	}
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 64<<10), 1<<20)
	for scan.Scan() {
		var event struct {
			Type   string `json:"type"`
			Quoted *struct {
				MessageID      string `json:"message_id"`
				ConversationID string `json:"conversation_id"`
			} `json:"quoted_message"`
			SenderName     string `json:"sender"`
			MessageID      string `json:"message_id"`
			ConversationID string `json:"conversation_id"`
			Sender         string `json:"sender_open_dingtalk_id"`
			Content        string `json:"content"`
		}
		if json.Unmarshal(scan.Bytes(), &event) != nil {
			cancel()
			break
		}
		if event.Type != sub.event || (sub.peer != "" && event.Sender != sub.peer) || !validID(event.Sender) || (event.Sender == c.self && sub.event != groupEvent) || event.MessageID == "" || event.ConversationID == "" || strings.TrimSpace(event.Content) == "" {
			c.mu.Lock()
			c.status.Rejected++
			c.mu.Unlock()
			continue
		}
		a := c.Identity()
		a.ChatID = c.addressPrefix() + event.Sender
		if sub.event == mentionEvent || sub.event == groupEvent {
			a.ChatID = c.accountPrefix() + "group:" + event.ConversationID
		}
		msg := im.Message{Address: a, ID: "dws:" + store.Hash(c.profile+"\x00"+c.cfg.ID+"\x00"+event.ConversationID+"\x00"+event.MessageID), UserID: event.Sender, UserName: event.SenderName, Text: event.Content, IsDirect: sub.event != mentionEvent && sub.event != groupEvent}
		msg.Followup = sub.event == groupEvent
		msg.SelfMessage = event.Sender == c.self
		if msg.Followup && (event.ConversationID != sub.group || !c.AcceptsMessage(msg)) {
			continue
		}
		msg.PlatformID = event.MessageID
		msg.ConversationID = event.ConversationID
		if event.Quoted != nil {
			msg.QuotedID = event.Quoted.MessageID
			msg.QuotedConversationID = event.Quoted.ConversationID
		}
		// 暂时不能持久化时重试同一输入，不直接丢弃；桥接负责消息 ID 去重。
		for receive(ctx, msg) != nil {
			if !pause(ctx, time.Second) {
				break
			}
		}
		c.mu.Lock()
		c.status.Received++
		c.status.UpdatedAt = time.Now().Unix()
		c.mu.Unlock()
		if ctx.Err() != nil {
			break
		}
	}
	cancel()
	stdin.Close()
	if err = cmd.Wait(); err != nil {
		return errors.New("DWS 监听中断")
	}
	return scan.Err()
}
