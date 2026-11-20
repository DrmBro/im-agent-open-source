package dwspersonal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/queue"
	"im-agent/internal/store"
)

type Client struct {
	directoryActivity     sync.Map
	directoryScheduled    sync.Map
	directoryIndexMu      sync.Mutex
	directoryIndexAt      time.Time
	directoryJobs         *queue.Queue
	directoryCache        sync.Map
	groupNames            sync.Map
	nameJobs              *queue.Queue
	cfg                   config.Connection
	binary, profile, self string
	mu                    sync.Mutex
	status                im.ConnectionStatus
	ready                 map[string]bool
	followGroups          map[string]int64
	followReady           map[string]bool
	assistantPeers        map[string]bool
	assistantGroups       map[string]bool
}

func Factory(cfg config.Connection) (im.Adapter, error) {
	if err := validateCredentials(cfg); err != nil {
		return nil, err
	}
	binary, _ := cfg.Credential("dws_binary")
	if binary == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("无法确定 DWS 安装目录")
		}
		binary = filepath.Join(home, ".local/bin/dws")
	}
	// 程序缺失不能导致已有飞书连接启动失败，由 Start 显示 waiting_config。
	profile, _ := cfg.Credential("profile")
	self, _ := cfg.Credential("self_open_id")
	return &Client{cfg: cfg, binary: binary, profile: profile, self: self, ready: map[string]bool{}, followGroups: map[string]int64{}, followReady: map[string]bool{}, status: im.ConnectionStatus{Connection: cfg.ID, Phase: "stopped"}}, nil
}
func (c *Client) Identity() im.Address  { return im.Address{Channel: channel, Connection: c.cfg.ID} }
func (c *Client) addressPrefix() string { return "account:" + c.profile + ":peer:" }
func (c *Client) accountPrefix() string { return "account:" + c.profile + ":" }
func (c *Client) AcceptsMessage(msg im.Message) bool {
	var err error
	msg.Address, err = msg.Address.DeliveryAddress()
	if err != nil {
		return false
	}
	if msg.Address.Channel != channel || msg.Address.Connection != c.cfg.ID || msg.Address.TopicID != "" || !validID(msg.UserID) {
		return false
	}
	if msg.Followup {
		if !c.cfg.PersonalGroupFollowup || !c.cfg.PersonalMentions || msg.IsDirect {
			return false
		}
		group := strings.TrimPrefix(msg.Address.ChatID, c.accountPrefix()+"group:")
		if group == msg.Address.ChatID || !validID(group) {
			return false
		}
		if msg.SelfMessage != (msg.UserID == c.self) {
			return false
		}
		if msg.FollowupTarget != "" {
			return true
		} // 已由本机明确关联并持久化的输入可完成后续分析。
		c.mu.Lock()
		until := c.followGroups[group]
		c.mu.Unlock()
		return until > time.Now().Unix()
	}
	if msg.UserID == c.self {
		return false
	}
	if msg.IsDirect {
		return msg.Address.ChatID == c.addressPrefix()+msg.UserID && (c.cfg.PersonalAllDirect || config.Contains(c.cfg.AllowedUsers, msg.UserID) || c.assistantPeer(msg.UserID))
	}
	if c.assistantGroup(strings.TrimPrefix(msg.Address.ChatID, c.accountPrefix()+"group:")) {
		return true
	}
	return c.cfg.PersonalMentions && strings.HasPrefix(msg.Address.ChatID, c.accountPrefix()+"group:") && len(msg.Address.ChatID) > len(c.accountPrefix()+"group:")
}
func (c *Client) Capabilities() im.Capabilities { return im.Capabilities{PersonalInbox: true} }
func (c *Client) ConnectionStatus() im.ConnectionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	return s
}
func (c *Client) phase(phase, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Phase = phase
	c.status.Message = message
	c.status.UpdatedAt = time.Now().Unix()
}

func (c *Client) authenticated(ctx context.Context) bool {
	raw, err := c.run(ctx, "", "auth", "status")
	if err != nil {
		return false
	}
	raw, err = successPayload(raw)
	if err != nil {
		return false
	}
	var r struct {
		Authenticated bool `json:"authenticated"`
		Success       bool `json:"success"`
	}
	return json.Unmarshal(raw, &r) == nil && r.Authenticated && r.Success
}

// 仅桥接最终回复调用此入口。平台返回不明确时由现有账本阻止自动重发。
func (c *Client) SendText(ctx context.Context, a im.Address, text, key string) (string, error) {
	return c.sendPersonal(ctx, a, text, key, "--text")
}
func (c *Client) sendPersonal(ctx context.Context, a im.Address, text, key, bodyFlag string, workDir ...string) (string, error) {
	var addressErr error
	a, addressErr = a.DeliveryAddress()
	if addressErr != nil {
		return "", addressErr
	}
	if a.Channel != channel || a.Connection != c.cfg.ID || a.TopicID != "" {
		return "", errors.New("个人代回复目标未获授权")
	}
	var target []string
	group := ""
	if strings.HasPrefix(a.ChatID, c.addressPrefix()) {
		peer := strings.TrimPrefix(a.ChatID, c.addressPrefix())
		if !validID(peer) || peer == c.self || (!c.cfg.PersonalAllDirect && !config.Contains(c.cfg.AllowedUsers, peer) && !c.assistantPeer(peer)) {
			return "", errors.New("私聊目标不在当前监听范围内")
		}
		target = []string{"--open-dingtalk-id", peer}
	} else if strings.HasPrefix(a.ChatID, c.accountPrefix()+"group:") && (c.cfg.PersonalMentions || c.assistantGroup(strings.TrimPrefix(a.ChatID, c.accountPrefix()+"group:"))) {
		group = strings.TrimPrefix(a.ChatID, c.accountPrefix()+"group:")
		if !validID(group) {
			return "", errors.New("原群会话标识无效")
		}
		target = []string{"--group", group}
	} else {
		return "", errors.New("个人回复目标与登录账号或监听范围不一致")
	}
	if strings.TrimSpace(text) == "" || len(text) > 256<<10 {
		return "", errors.New("代回复正文为空或过长")
	}
	if !strings.HasPrefix(key, "personal-reply:") {
		return "", errors.New("个人账号仅发送已入账的最终回复")
	}
	if !c.authenticated(ctx) {
		return "", errors.New("DWS 账号未登录或无法读取授权状态，尚未发送")
	}
	idempotency := store.Hash(channel + "\x00" + c.profile + "\x00" + a.Key() + "\x00" + key)
	args := append([]string{"chat", "+messages-send", "--identity", "user"}, target...)
	args = append(args, bodyFlag, "-", "--body-stdin", "--ai-tag=true", "--idempotency-key", idempotency, "--wait-delivery", "--yes")
	var raw []byte
	var err error
	if len(workDir) == 1 && workDir[0] != "" {
		raw, err = c.runInDirectory(ctx, text, workDir[0], 85*time.Second, args...)
	} else {
		raw, err = c.run(ctx, text, args...)
	}
	if err != nil {
		var rejected *im.DeliveryRejected
		if errors.As(err, &rejected) {
			return "", err
		}
		return "", &im.DeliveryUncertain{Cause: err}
	}
	raw, err = successPayload(raw)
	if err != nil {
		return "", &im.DeliveryUncertain{Cause: err}
	}
	var receipt struct {
		MessageID      string `json:"openMessageId"`
		Delivery       string `json:"deliveryStatus"`
		Key            string `json:"idempotencyKey"`
		ConversationID string `json:"conversationId"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.MessageID == "" || receipt.Key != idempotency || receipt.Delivery != "delivered" || (group != "" && receipt.ConversationID != group) {
		return "", &im.DeliveryUncertain{Cause: errors.New("DWS 未返回确定的消息投递回执")}
	}
	return receipt.MessageID, nil
}
func (c *Client) SendCard(context.Context, im.Address, im.Card, string) (string, error) {
	return "", errors.New("个人代回复不向联系人发送控制卡或审批请求")
}
func (c *Client) UpdateCard(context.Context, im.Address, string, im.Card) error {
	return errors.New("个人代回复不支持更新卡片")
}
func (c *Client) CreateConversation(context.Context, im.Conversation) (im.Address, error) {
	return im.Address{}, errors.New("个人代回复只使用已授权联系人的单聊")
}
