package desktop

import (
	"context"
	"errors"
	"time"

	"im-agent/internal/store"
)

func (a *API) taskBoardContext() (context.Context, context.CancelFunc, string, error) {
	a.app.mu.Lock()
	defer a.app.mu.Unlock()
	if err := a.app.ready(); err != nil {
		return nil, nil, "", err
	}
	view, err := a.app.store.Read()
	if err != nil || a.app.ctx == nil {
		return nil, nil, "", errors.New("任务看板配置暂不可读取")
	}
	ctx, stop := context.WithTimeout(a.app.ctx, 5*time.Second)
	return ctx, stop, view.Config.StateDir, nil
}

func (a *API) GetTaskBoard(query store.TaskBoardQuery) (store.TaskBoardView, error) {
	ctx, stop, dir, err := a.taskBoardContext()
	if err != nil {
		return store.TaskBoardView{}, err
	}
	defer stop()
	return store.TaskBoardSnapshot(ctx, dir, query)
}

func (a *API) GetTaskBoardDetail(source, id string) (store.TaskBoardDetail, error) {
	ctx, stop, dir, err := a.taskBoardContext()
	if err != nil {
		return store.TaskBoardDetail{}, err
	}
	defer stop()
	return store.TaskBoardDetailSnapshot(ctx, dir, source, id)
}
