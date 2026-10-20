package service

import (
	"errors"
	"im-agent/internal/qa"
)

type QADeliveryResolution struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Confirmed bool   `json:"confirmed"`
}

func (c *Controller) ResolveQARequest(request qa.Resolution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.qa == nil || c.status.Phase != "running" {
		return errors.New("请先启动钉钉问答后台")
	}
	return c.qa.ResolveRequest(request)
}
func (c *Controller) ResolveQADelivery(request QADeliveryResolution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.qa == nil || c.status.Phase != "running" {
		return errors.New("请先启动钉钉问答后台")
	}
	return c.qa.ResolveDelivery(request.ID, request.Action, request.Confirmed)
}
