// Package qa manages independent DingTalk questions without desktop session bindings.
package qa

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/queue"
	"im-agent/internal/store"
)

type Runner struct {
	cfg            config.Config
	db             *store.Store
	cx             *codex.Client
	adapters       map[string]im.Adapter
	mu             sync.Mutex
	scheduled      map[string]bool
	threads        map[string]string
	lastUsed       map[string]time.Time
	active         map[string]*execution
	approvals      map[string]*approval
	message, phase string
	messageAt      int64
	fatal          chan error
	stop           context.CancelFunc
}

func New(cfg config.Config, db *store.Store, cx *codex.Client, adapters map[string]im.Adapter) *Runner {
	return &Runner{cfg: cfg, db: db, cx: cx, adapters: adapters, scheduled: map[string]bool{}, threads: map[string]string{}, lastUsed: map[string]time.Time{}, active: map[string]*execution{}, approvals: map[string]*approval{}, fatal: make(chan error, 1)}
}
func (r *Runner) diagnostic(phase, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.phase = phase
	r.message = message
	r.messageAt = time.Now().Unix()
}
func (r *Runner) Diagnostics() (string, string, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase, r.message, r.messageAt
}

// fail cancels scheduling synchronously before the current worker can release its slot.
func (r *Runner) fail(e error) {
	select {
	case r.fatal <- e:
	default:
	}
	r.mu.Lock()
	stop := r.stop
	r.mu.Unlock()
	if stop != nil {
		stop()
	}
}
func (r *Runner) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.stop = cancel
	r.mu.Unlock()
	defer cancel()
	if e := r.db.RecoverQA(); e != nil {
		return e
	}
	connections := make([]string, 0, len(r.adapters))
	for id := range r.adapters {
		connections = append(connections, id)
	}
	if e := r.db.QAReconcileConnections(connections); e != nil {
		return e
	}
	r.cx.SetHandler(r.handleRequest)
	jobs := queue.New(ctx, r.cfg.Limits.Workers, r.cfg.Limits.QueueSize)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait(); jobs.Wait() }()
	for _, a := range r.adapters {
		adapter := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := adapter.Start(ctx, r.receive, func(context.Context, im.Interaction) error { return errors.New("钉钉问答未启用群内审批") })
			if e != nil && ctx.Err() == nil {
				r.fail(e)
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); r.deliver(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); r.notifyLoop(ctx) }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				r.retireIdle(ctx)
			}
		}
	}()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	r.diagnostic("waiting", "钉钉问答已启动；直接 @新开，引用回答续聊")
	for {
		select {
		case <-ctx.Done():
			if parent.Err() != nil {
				return nil
			}
			select {
			case e := <-r.fatal:
				return e
			default:
				return errors.New("钉钉问答已停止调度")
			}
		case <-r.cx.Done():
			return errors.New("钉钉 Codex 后台进程已退出")
		case e := <-r.fatal:
			return e
		case <-ticker.C:
			turns, e := r.db.QAQueued(256)
			if e != nil {
				return e
			}
			for _, turn := range turns {
				t := turn
				r.mu.Lock()
				scheduled := r.scheduled[t.ID]
				if !scheduled {
					r.scheduled[t.ID] = true
				}
				r.mu.Unlock()
				if scheduled {
					continue
				}
				if e = jobs.Push(t.QuestionID, func(jobCtx context.Context) {
					defer func() { r.mu.Lock(); delete(r.scheduled, t.ID); r.mu.Unlock() }()
					r.execute(jobCtx, t)
				}); e != nil {
					r.mu.Lock()
					delete(r.scheduled, t.ID)
					r.mu.Unlock()
					if !errors.Is(e, queue.ErrFull) && ctx.Err() == nil {
						return e
					}
				}
			}
		}
	}
}
func (r *Runner) receive(ctx context.Context, m im.Message) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var connection *config.Connection
	for i := range r.cfg.Connections {
		if r.cfg.Connections[i].ID == m.Address.Connection && r.cfg.Connections[i].Type == m.Address.Channel {
			connection = &r.cfg.Connections[i]
			break
		}
	}
	if connection == nil || !config.Contains(connection.AllowedUsers, m.UserID) {
		return nil
	}
	if m.IsDirect || m.Address.Channel != "dingtalk" {
		return nil
	}
	if isProgressQuery(m.Text) {
		return r.replyProgress(m)
	}
	e := r.db.AcceptQA(m, r.cfg.Limits.MaxInputChars, r.cfg.Limits.Workers*r.cfg.Limits.QueueSize)
	if e == nil {
		r.diagnostic("queued", "钉钉问题已持久化，等待独立问答执行")
	}
	return e
}
func (r *Runner) deliver(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		parts, e := r.db.QAPendingDeliveries(16)
		if e != nil {
			r.fail(e)
			return
		}
		for _, part := range parts {
			if ctx.Err() != nil {
				return
			}
			adapter := r.adapters[part.Connection]
			if adapter == nil {
				continue
			}
			claimed, e := r.db.QAClaimDelivery(part.ID)
			if e != nil {
				r.fail(e)
				return
			}
			if !claimed {
				continue
			}
			op, stop := context.WithTimeout(ctx, 25*time.Second)
			id, sendErr := adapter.SendText(op, im.Address{Channel: "dingtalk", Connection: part.Connection, ChatID: part.ChatID}, part.Body, part.ID)
			stop()
			state, detail := "sent", ""
			if sendErr != nil {
				state = "failed"
				detail = "钉钉发送明确失败，请检查权限或发送限额后在本机重试"
				var uncertain *im.DeliveryUncertain
				if errors.As(sendErr, &uncertain) {
					state = "unknown"
					detail = "发送结果未确认，请先核对群消息，再在本机处理"
				}
			}
			if e = r.db.QADeliveryResult(part, state, id, detail); e != nil {
				r.fail(e)
				return
			}
			if state != "sent" {
				r.diagnostic("delivery_failed", detail)
			}
		}
	}
}
func (r *Runner) ResolveDelivery(id, action string, confirmed bool) error {
	connection, e := r.db.QADeliveryConnection(id)
	if e != nil {
		return e
	}
	if r.adapters[connection] == nil {
		return errors.New("该钉钉连接未启用，请恢复对应连接并重启后台后再处理")
	}
	return r.db.QAResolveDelivery(id, action, confirmed)
}

// Retire idle in-memory threads; durable question records remain available for reference replies.
func (r *Runner) retireIdle(ctx context.Context) {
	type candidate struct {
		question, thread string
		used             time.Time
	}
	r.mu.Lock()
	candidates := []candidate{}
	for question, thread := range r.threads {
		if r.active[thread] == nil {
			candidates = append(candidates, candidate{question, thread, r.lastUsed[question]})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used.Before(candidates[j].used) })
	retired := []string{}
	for _, c := range candidates {
		if len(retired) >= 16 {
			break
		}
		if len(r.threads) <= 128 && time.Since(c.used) < 15*time.Minute {
			continue
		}
		delete(r.threads, c.question)
		delete(r.lastUsed, c.question)
		retired = append(retired, c.thread)
	}
	r.mu.Unlock()
	for _, thread := range retired {
		if ctx.Err() != nil {
			return
		}
		op, stop := context.WithTimeout(ctx, 2*time.Second)
		_, _ = r.cx.Call(op, "thread/unsubscribe", map[string]string{"threadId": thread})
		stop()
	}
}
