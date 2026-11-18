package service

import (
	"context"
	"im-agent/internal/bridge"
	"im-agent/internal/store"
)

func (c *Controller) PersonalInbox(ctx context.Context) ([]store.PersonalInboxItem, error) {
	var out []store.PersonalInboxItem
	err := c.withReviewBridge(ctx, func(op context.Context, b *bridge.Bridge) error { var e error; out, e = b.PersonalInbox(op); return e })
	return out, err
}
func (c *Controller) ReviewPersonalInbox(ctx context.Context, a bridge.PersonalInboxAction) error {
	return c.withReviewBridge(ctx, func(op context.Context, b *bridge.Bridge) error { return b.ReviewPersonalInbox(op, a) })
}
