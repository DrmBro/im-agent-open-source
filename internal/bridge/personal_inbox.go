package bridge

import (
	"context"
	"im-agent/internal/store"
)

type PersonalInboxAction struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

func (b *Bridge) ReviewPersonalInbox(ctx context.Context, a PersonalInboxAction) error {
	switch a.Action {
	case "start":
		return b.CreatePersonalInboxSession(ctx, a.ID)
	case "ignore":
		return b.IgnorePersonalInbox(a.ID)
	default:
		return store.ErrTaskDeliveryUnavailable
	}
}
