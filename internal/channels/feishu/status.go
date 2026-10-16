package feishu

import (
	"im-agent/internal/im"
	"time"
)

func (c *Client) ConnectionStatus() im.ConnectionStatus {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	s := c.status
	s.Connection = c.cfg.ID
	if s.Phase == "" {
		s.Phase = "waiting"
		s.Message = "尚未开始连接"
	}
	return s
}
func (c *Client) setStatus(phase, message string) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.status.Phase = phase
	c.status.Message = message
	c.status.UpdatedAt = time.Now().Unix()
}
func (c *Client) recordMessage(rejected bool) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.status.Received++
	if rejected {
		c.status.Rejected++
		c.status.Message = "收到消息，但用户不在此应用的白名单内；请核对 Open ID 是否属于同一飞书应用"
	}
	c.status.UpdatedAt = time.Now().Unix()
}
