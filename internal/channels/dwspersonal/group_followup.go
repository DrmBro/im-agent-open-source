package dwspersonal

import (
	"context"
	"fmt"
	"im-agent/internal/im"
	"strings"
	"sync"
	"time"
)

const groupEvent = "user_im_message_receive_group"

func (c *Client) SetGroupFollowups(groups []im.GroupFollowup) {
	next := map[string]int64{}
	if c.cfg.PersonalGroupFollowup && c.cfg.PersonalMentions {
		for _, g := range groups {
			a, err := g.Address.DeliveryAddress()
			if err != nil || a.Channel != channel || a.Connection != c.cfg.ID {
				continue
			}
			id := strings.TrimPrefix(a.ChatID, c.accountPrefix()+"group:")
			if id == a.ChatID || !validID(id) || g.Until <= time.Now().Unix() {
				continue
			}
			if g.Until > next[id] {
				next[id] = g.Until
			}
		}
	}
	c.mu.Lock()
	c.followGroups = next
	c.mu.Unlock()
}
func (c *Client) groupPlans() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int64{}
	for group, until := range c.followGroups {
		if until > time.Now().Unix() {
			out[group] = until
		}
	}
	return out
}
func (c *Client) groupReady(group string, ready bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.followReady[group] = ready
}
func (c *Client) followupStatus() string {
	active, ready := 0, 0
	now := time.Now().Unix()
	for group, until := range c.followGroups {
		if until > now {
			active++
			if c.followReady[group] {
				ready++
			}
		}
	}
	return fmt.Sprintf("；群跟进 %d 个（就绪 %d）", active, ready)
}
func (c *Client) manageGroupFollowups(ctx context.Context, receive func(context.Context, im.Message) error) {
	type worker struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	workers := map[string]worker{}
	var wg sync.WaitGroup
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		wg.Wait()
	}()
	for ctx.Err() == nil {
		plans := c.groupPlans()
		for group, w := range workers {
			if _, ok := plans[group]; !ok {
				w.cancel()
				select {
				case <-w.done:
					delete(workers, group)
				default:
				}
			}
		}
		for group := range plans {
			if _, exists := workers[group]; exists {
				continue
			}
			if len(workers) >= 10 {
				break
			}
			run, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			workers[group] = worker{cancel, done}
			wg.Add(1)
			go func(group string) {
				defer wg.Done()
				defer close(done)
				defer c.groupReady(group, false)
				for run.Err() == nil {
					_ = c.listen(run, subscription{event: groupEvent, group: group}, 0, receive)
					c.groupReady(group, false)
					if !pause(run, 10*time.Second) {
						return
					}
				}
			}(group)
		}
		if !pause(ctx, time.Second) {
			return
		}
	}
}
