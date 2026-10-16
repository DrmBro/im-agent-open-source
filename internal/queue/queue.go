// Package queue 按会话保持顺序，不同会话共享有界工作池，避免哈希碰撞造成无关任务阻塞。
package queue

import (
	"context"
	"errors"
	"sync"
)

var ErrFull = errors.New("等待队列已满，请稍后重试调度")

type Job func(context.Context)
type queuedJob struct {
	key string
	run Job
}
type Queue struct {
	ctx                      context.Context
	mu                       sync.Mutex
	pending                  map[string][]Job
	active                   map[string]bool
	ready                    []string
	total                    int
	capacity, globalCapacity int
	wake                     chan struct{}
	work                     chan queuedJob
	wg                       sync.WaitGroup
}

func New(ctx context.Context, n, capacity int) *Queue {
	q := &Queue{ctx: ctx, pending: map[string][]Job{}, active: map[string]bool{}, capacity: capacity, globalCapacity: n * capacity, wake: make(chan struct{}, 1), work: make(chan queuedJob)}
	q.wg.Add(n + 1)
	go q.dispatch()
	for i := 0; i < n; i++ {
		go func() {
			defer q.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-q.work:
					if ctx.Err() == nil {
						job.run(ctx)
					}
					q.complete(job.key)
				}
			}
		}()
	}
	return q
}
func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
func (q *Queue) Push(key string, job Job) error {
	if job == nil {
		return errors.New("任务为空")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.ctx.Err() != nil {
		return q.ctx.Err()
	}
	if len(q.pending[key]) >= q.capacity || q.total >= q.globalCapacity {
		return ErrFull
	}
	if len(q.pending[key]) == 0 && !q.active[key] {
		q.ready = append(q.ready, key)
	}
	q.pending[key] = append(q.pending[key], job)
	q.total++
	q.signal()
	return nil
}
func (q *Queue) next() (queuedJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.ready) == 0 {
		return queuedJob{}, false
	}
	key := q.ready[0]
	q.ready = q.ready[1:]
	jobs := q.pending[key]
	job := jobs[0]
	jobs[0] = nil
	q.pending[key] = jobs[1:]
	q.total--
	q.active[key] = true
	return queuedJob{key, job}, true
}
func (q *Queue) dispatch() {
	defer q.wg.Done()
	for {
		if q.ctx.Err() != nil {
			return
		}
		job, ok := q.next()
		if !ok {
			select {
			case <-q.ctx.Done():
				return
			case <-q.wake:
			}
			continue
		}
		select {
		case <-q.ctx.Done():
			return
		case q.work <- job:
		}
	}
}
func (q *Queue) complete(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.active, key)
	if len(q.pending[key]) > 0 {
		q.ready = append(q.ready, key)
	} else {
		delete(q.pending, key)
	}
	q.signal()
}
func (q *Queue) Wait() { q.wg.Wait() }
