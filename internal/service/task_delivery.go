package service

import (
	"context"
	"errors"

	"im-agent/internal/bridge"
)

func (c *Controller) SendTaskReport(ctx context.Context, request bridge.SendTaskReportRequest) error {
	c.mu.Lock()
	b, phase, parent := c.bridge, c.status.Phase, c.operationContext
	if b == nil || phase != "running" || parent == nil {
		c.mu.Unlock()
		return errors.New("后台桥接未运行")
	}
	c.operations.Add(1)
	c.mu.Unlock()
	defer c.operations.Done()
	op, stop := context.WithCancel(ctx)
	defer stop()
	detach := context.AfterFunc(parent, stop)
	defer detach()
	return b.SendTaskReport(op, request)
}
