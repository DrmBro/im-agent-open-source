package service

import (
	"context"
	"errors"
	"im-agent/internal/bridge"
	"im-agent/internal/store"
)

func (c *Controller) ReviewPersonalReply(ctx context.Context, r store.PersonalReviewAction) error {
	return c.withReviewBridge(ctx, func(op context.Context, b *bridge.Bridge) error { return b.ReviewPersonalReply(op, r) })
}
func (c *Controller) ReviewGroupFollowup(ctx context.Context, r store.GroupFollowAction) error {
	return errors.New("群任务跟进已移除，请通过群 @补充问题")
}
func (c *Controller) withReviewBridge(ctx context.Context, action func(context.Context, *bridge.Bridge) error) error {
	c.mu.Lock()
	b, parent := c.bridge, c.operationContext
	if b == nil || parent == nil || c.status.Phase != "running" {
		c.mu.Unlock()
		return errors.New("请先启动桥接，再处理待审回复")
	}
	c.operations.Add(1)
	c.mu.Unlock()
	defer c.operations.Done()
	op, cancel := context.WithCancel(ctx)
	defer cancel()
	detach := context.AfterFunc(parent, cancel)
	defer detach()
	return action(op, b)
}
