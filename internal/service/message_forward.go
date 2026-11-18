package service

import (
	"context"
	"im-agent/internal/bridge"
)

func (c *Controller) MessageForward(ctx context.Context, r bridge.MessageForwardRequest) (bridge.MessageForwardView, error) {
	var out bridge.MessageForwardView
	err := c.withReviewBridge(ctx, func(op context.Context, b *bridge.Bridge) error {
		var err error
		out, err = b.MessageForward(op, r)
		return err
	})
	return out, err
}
