package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"im-agent/internal/config"
	"im-agent/internal/im"
)

const apiBase = "https://api.dingtalk.com"
const messageTopic = "/v1.0/im/bot/messages/get"

type Client struct {
	cfg                  config.Connection
	appID, secret, robot string
	http                 *http.Client
	tokenMu              sync.Mutex
	token                string
	tokenUntil           time.Time
	statusMu             sync.Mutex
	status               im.ConnectionStatus
	identityMu           sync.Mutex
	identityReplies      map[string]time.Time
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
	robot, e := c.Credential("robot_code")
	if e != nil {
		return nil, e
	}
	if id == "" || secret == "" || robot == "" {
		return nil, errors.New("钉钉应用凭证与 RobotCode 不能为空")
	}
	return &Client{identityReplies: map[string]time.Time{}, cfg: c, appID: id, secret: secret, robot: robot, http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, status: im.ConnectionStatus{Connection: c.ID, Phase: "disconnected", Message: "等待钉钉 Stream 连接"}}, nil
}

func (c *Client) Identity() im.Address          { return im.Address{Channel: "dingtalk", Connection: c.cfg.ID} }
func (c *Client) Capabilities() im.Capabilities { return im.Capabilities{} }
func (c *Client) CreateConversation(context.Context, im.Conversation) (im.Address, error) {
	return im.Address{}, errors.New("钉钉问答使用已有群，不自动建群")
}
func (c *Client) SendCard(context.Context, im.Address, im.Card, string) (string, error) {
	return "", errors.New("钉钉问答使用文本消息；审批请在本机处理")
}
func (c *Client) UpdateCard(context.Context, im.Address, string, im.Card) error {
	return errors.New("钉钉问答未启用卡片更新")
}
func (c *Client) ConnectionStatus() im.ConnectionStatus {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.status
}
func (c *Client) setStatus(phase, message string) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.status.Phase = phase
	c.status.Message = message
	c.status.UpdatedAt = time.Now().Unix()
}
func (c *Client) count(rejected bool) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	if rejected {
		c.status.Rejected++
	} else {
		c.status.Received++
	}
	c.status.UpdatedAt = time.Now().Unix()
}

// post never returns the upstream body or URL in an error: both can contain credentials.
func (c *Client) post(ctx context.Context, path, token string, body, out any, delivery bool) error {
	raw, e := json.Marshal(body)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, bytes.NewReader(raw))
	if e != nil {
		return errors.New("钉钉请求初始化失败")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("x-acs-dingtalk-access-token", token)
	}
	resp, e := c.http.Do(req)
	if e != nil {
		if delivery {
			return &im.DeliveryUncertain{Cause: errors.New("钉钉发送请求未获得确定回执")}
		}
		return errors.New("钉钉网络请求失败")
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if e != nil || len(data) > 1<<20 {
		if delivery {
			return &im.DeliveryUncertain{Cause: errors.New("钉钉发送回执读取失败")}
		}
		return errors.New("钉钉响应读取失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized && token != "" {
			c.tokenMu.Lock()
			if c.token == token {
				c.token = ""
			}
			c.tokenMu.Unlock()
		}
		if delivery && resp.StatusCode >= 500 {
			return &im.DeliveryUncertain{Cause: errors.New("钉钉服务端未确认发送结果")}
		}
		return errors.New("钉钉接口拒绝请求，请检查凭证、机器人权限和群成员关系")
	}
	var result struct {
		Code    json.RawMessage `json:"code"`
		ErrCode int             `json:"errcode"`
	}
	if json.Unmarshal(data, &result) != nil {
		if delivery {
			return &im.DeliveryUncertain{Cause: errors.New("钉钉发送回执格式无效")}
		}
		return errors.New("钉钉响应格式无效")
	}
	if result.ErrCode != 0 || (len(result.Code) > 0 && string(result.Code) != "null" && string(result.Code) != "\"\"" && string(result.Code) != "\"0\"" && string(result.Code) != "0" && string(result.Code) != "\"OK\"") {
		return errors.New("钉钉接口返回错误，请检查应用权限及发送限额")
	}
	if e = json.Unmarshal(data, out); e != nil {
		if delivery {
			return &im.DeliveryUncertain{Cause: errors.New("钉钉发送回执无法识别")}
		}
		return errors.New("钉钉响应无法识别")
	}
	return nil
}
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenUntil) {
		return c.token, nil
	}
	var reply struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    int64  `json:"expireIn"`
	}
	if e := c.post(ctx, "/v1.0/oauth2/accessToken", "", map[string]string{"appKey": c.appID, "appSecret": c.secret}, &reply, false); e != nil {
		return "", e
	}
	if reply.AccessToken == "" || reply.ExpireIn <= 0 {
		return "", errors.New("钉钉未返回有效访问凭证")
	}
	c.token = reply.AccessToken
	c.tokenUntil = time.Now().Add(time.Duration(reply.ExpireIn)*time.Second - time.Minute)
	return c.token, nil
}
func (c *Client) SendText(ctx context.Context, a im.Address, text, key string) (string, error) {
	return c.SendMarkdown(ctx, a, text, key)
}
func (c *Client) SendMarkdown(ctx context.Context, a im.Address, text, key string) (string, error) {
	if a.Channel != "dingtalk" || a.Connection != c.cfg.ID || a.ChatID == "" || a.IsTopic() {
		return "", errors.New("钉钉发送地址无效")
	}
	token, e := c.accessToken(ctx)
	if e != nil {
		return "", e
	}
	param, _ := json.Marshal(map[string]string{"title": "IM Agent 回答", "text": text})
	var reply struct {
		ProcessQueryKey string `json:"processQueryKey"`
	}
	e = c.post(ctx, "/v1.0/robot/groupMessages/send", token, map[string]string{"robotCode": c.robot, "openConversationId": a.ChatID, "msgKey": "sampleMarkdown", "msgParam": string(param)}, &reply, true)
	if e != nil {
		return "", e
	}
	if reply.ProcessQueryKey == "" {
		return "", &im.DeliveryUncertain{Cause: errors.New("钉钉未返回可关联的消息回执")}
	}
	return "process:" + reply.ProcessQueryKey, nil
}

var _ im.Adapter = (*Client)(nil)
var _ im.MarkdownAdapter = (*Client)(nil)
var _ im.StatusProvider = (*Client)(nil)
