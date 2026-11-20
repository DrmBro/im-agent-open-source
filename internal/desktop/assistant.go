package desktop

import (
	"context"
	"time"

	"im-agent/internal/bridge"
)

func (a *API) Assistant(r bridge.AssistantRequest) (bridge.AssistantResponse, error) {
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return bridge.AssistantResponse{}, err
	}
	client, parent := a.app.controller, a.app.ctx
	a.app.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	return client.Assistant(ctx, r)
}
