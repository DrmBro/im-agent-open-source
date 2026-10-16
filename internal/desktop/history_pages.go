package desktop

import (
	"context"
	"errors"
	"sync"
	"time"

	"im-agent/internal/daemon"
	"im-agent/internal/store"
)

// GetHistoryPage 只暴露指定历史页；完整发文件目标仍使用独立入口。
func (a *API) GetHistoryPage(kind string, page, size int) (store.HistoryPage, error) {
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return store.HistoryPage{}, err
	}
	view, err := a.app.store.Read()
	parent, client := a.app.ctx, a.app.controller
	a.app.mu.Unlock()
	if err != nil || parent == nil {
		return store.HistoryPage{}, errors.New("历史列表配置暂不可读取")
	}
	ctx, stop := context.WithTimeout(parent, 5*time.Second)
	defer stop()
	out, err := store.HistoryPageSnapshot(ctx, view.Config.StateDir, kind, page, size)
	if err != nil {
		return out, err
	}
	if kind == "closures" {
		// 后台网络调用绝不持有 app 锁，也不拖延本次 API 超过上下文期限。
		warning := historyLifecycleWarning(ctx, client)
		if warning != "" {
			if out.Warning != "" {
				out.Warning += "；"
			}
			out.Warning += warning
		}
	}
	return out, nil
}

// 既有 LifecycleWarning 没有 context 参数且网络超时较长；共享尚未结束的读取，
// 防止前端连续刷新在超时后不断创建后台调用。只共享在途请求，不缓存旧告警。
type historyLifecycleRead struct {
	done    chan struct{}
	warning string
}

var historyLifecycleReads = struct {
	sync.Mutex
	pending map[*daemon.Client]*historyLifecycleRead
}{pending: make(map[*daemon.Client]*historyLifecycleRead)}

func historyLifecycleWarning(ctx context.Context, client *daemon.Client) string {
	const unavailable = "归档同步告警暂不可读取，请稍后刷新"
	if ctx.Err() != nil {
		return unavailable
	}
	historyLifecycleReads.Lock()
	read := historyLifecycleReads.pending[client]
	if read == nil {
		read = &historyLifecycleRead{done: make(chan struct{})}
		historyLifecycleReads.pending[client] = read
		go func() {
			read.warning = client.LifecycleWarning()
			historyLifecycleReads.Lock()
			delete(historyLifecycleReads.pending, client)
			close(read.done)
			historyLifecycleReads.Unlock()
		}()
	}
	historyLifecycleReads.Unlock()
	select {
	case <-read.done:
		return read.warning
	case <-ctx.Done():
		return unavailable
	}
}
