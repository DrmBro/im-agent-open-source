package service

import (
	"context"

	"im-agent/internal/bridge"
)

func (c *Controller) Assistant(ctx context.Context, r bridge.AssistantRequest) (bridge.AssistantResponse, error) {
	var out bridge.AssistantResponse
	err := c.withReviewBridge(ctx, func(op context.Context, b *bridge.Bridge) error {
		var e error
		out, e = b.Assistant(op, r)
		return e
	})
	return out, err
}
