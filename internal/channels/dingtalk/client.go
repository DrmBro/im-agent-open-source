package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

type Client struct {
	cfg     config.Connection
	http    *http.Client
	mu      sync.Mutex
	token   string
	expires time.Time
	status  im.ConnectionStatus
}

func Factory(c config.Connection) (im.Adapter, error) {
	for _, key := range []string{"app_id", "app_secret", "robot_code", "corp_id"} {
		if value, _ := c.Credential(key); strings.TrimSpace(value) == "" {
			return nil, errors.New("钉钉应用凭据不完整")
		}
	}
	return &Client{cfg: c, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, status: im.ConnectionStatus{Connection: c.ID, Phase: "stopped"}}, nil
}
func (c *Client) Identity() im.Address          { return im.Address{Channel: "dingtalk", Connection: c.cfg.ID} }
func (c *Client) Capabilities() im.Capabilities { return im.Capabilities{} }
func (c *Client) ConnectionStatus() im.ConnectionStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}
func (c *Client) phase(phase, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Phase = phase
	c.status.Message = message
	c.status.UpdatedAt = time.Now().Unix()
}
func (c *Client) credential(key string) string { value, _ := c.cfg.Credential(key); return value }

// 不返回原始响应、请求 URL 或平台错误文本，以免令牌和消息内容进入诊断日志。
func (c *Client) post(ctx context.Context, path, token string, body, result any, delivery bool) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return errors.New("钉钉请求编码失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.dingtalk.com"+path, bytes.NewReader(raw))
	if err != nil {
		return errors.New("钉钉请求构造失败")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	uncertain := func(err error) error {
		if delivery {
			return &im.DeliveryUncertain{Cause: err}
		}
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return uncertain(errors.New("钉钉请求中断或超时"))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return uncertain(errors.New("钉钉响应读取失败"))
	}
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("钉钉接口 HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 500 || resp.StatusCode == 408 {
			return uncertain(err)
		}
		return err
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return uncertain(errors.New("钉钉响应格式无效"))
	}
	if envelope.Code != "" && envelope.Code != "0" {
		return uncertain(errors.New("钉钉接口未确认成功"))
	}
	if json.Unmarshal(data, result) != nil {
		return uncertain(errors.New("钉钉响应解析失败"))
	}
	return nil
}
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	var r struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int    `json:"expireIn"`
	}
	err := c.post(ctx, "/v1.0/oauth2/accessToken", "", map[string]string{"appKey": c.credential("app_id"), "appSecret": c.credential("app_secret")}, &r, false)
	if err != nil {
		return "", err
	}
	if r.AccessToken == "" || r.ExpireIn <= 60 {
		return "", errors.New("钉钉访问令牌回执无效")
	}
	c.token = r.AccessToken
	c.expires = time.Now().Add(time.Duration(r.ExpireIn-60) * time.Second)
	return c.token, nil
}

// 主动发送不依赖回调里的临时 sessionWebhook，重启后的回复仍使用持久地址。
func (c *Client) SendText(ctx context.Context, a im.Address, text, key string) (string, error) {
	if a.Channel != "dingtalk" || a.Connection != c.cfg.ID || a.TopicID != "" {
		return "", errors.New("钉钉发送地址不匹配")
	}
	param, _ := json.Marshal(map[string]string{"content": text})
	body := map[string]any{"robotCode": c.credential("robot_code"), "msgKey": "sampleText", "msgParam": string(param)}
	path := "/v1.0/robot/groupMessages/send"
	if strings.HasPrefix(a.ChatID, "user:") {
		user := strings.TrimPrefix(a.ChatID, "user:")
		if !config.Contains(c.cfg.AllowedUsers, user) {
			return "", errors.New("钉钉私聊收件人不在白名单")
		}
		body["userIds"] = []string{user}
		path = "/v1.0/robot/oToMessages/batchSend"
	} else if strings.HasPrefix(a.ChatID, "group:") && len(a.ChatID) > 6 {
		body["openConversationId"] = strings.TrimPrefix(a.ChatID, "group:")
	} else {
		return "", errors.New("钉钉会话地址无效")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	var r struct {
		ProcessQueryKey           string   `json:"processQueryKey"`
		InvalidStaffIdList        []string `json:"invalidStaffIdList"`
		FlowControlledStaffIdList []string `json:"flowControlledStaffIdList"`
	}
	if err = c.post(ctx, path, token, body, &r, true); err != nil {
		return "", err
	}
	if len(r.InvalidStaffIdList) > 0 || len(r.FlowControlledStaffIdList) > 0 {
		return "", errors.New("钉钉收件人无效或被限流")
	}
	if r.ProcessQueryKey == "" {
		return "", &im.DeliveryUncertain{Cause: errors.New("钉钉缺少消息接收回执")}
	}
	return r.ProcessQueryKey, nil
}

// 无卡片模板时保留真实能力边界；文字命令由桥接层查询原审批并校验操作者。
func (c *Client) SendCard(ctx context.Context, a im.Address, card im.Card, key string) (string, error) {
	if card.Form != nil {
		return "", errors.New("钉钉尚未接入表单模板")
	}
	text := card.Title + "\n\n" + card.Text
	buttons := card.Buttons
	if card.Selection != nil {
		buttons = append(append([]im.Button{}, buttons...), card.Selection.Options...)
	}
	for _, b := range buttons {
		if card.ActionID != "" {
			text += "\n" + b.Label + "：/respond " + card.ActionID + " " + b.Value
		}
	}
	return c.SendText(ctx, a, text, key)
}
func (c *Client) UpdateCard(context.Context, im.Address, string, im.Card) error {
	return errors.New("钉钉当前使用文字消息，不支持原地更新卡片")
}
func (c *Client) CreateConversation(context.Context, im.Conversation) (im.Address, error) {
	return im.Address{}, errors.New("请使用钉钉机器人私聊或已有群聊")
}

func inboundID(connection, id string) string { return "dingtalk:" + store.Hash(connection+"\x00"+id) }
