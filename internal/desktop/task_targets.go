package desktop

import (
	"context"
	"time"

	"im-agent/internal/store"
)

func (a *API) GetTaskTargets() ([]store.TaskTarget, error) {
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return nil, err
	}
	view, err := a.app.store.Read()
	parent := a.app.ctx
	a.app.mu.Unlock()
	if err != nil {
		return nil, err
	}
	ctx, stop := context.WithTimeout(parent, 5*time.Second)
	defer stop()
	return store.TaskTargetsSnapshot(ctx, view.Config.StateDir)
}
