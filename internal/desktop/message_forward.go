package desktop

import (
	"context"
	"im-agent/internal/bridge"
	"time"
)

func (a *API) MessageForward(r bridge.MessageForwardRequest) (bridge.MessageForwardView, error) {
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return bridge.MessageForwardView{}, err
	}
	client, parent := a.app.controller, a.app.ctx
	a.app.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	return client.MessageForward(ctx, r)
}
