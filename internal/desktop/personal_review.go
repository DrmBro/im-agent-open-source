package desktop

import (
	"context"
	"errors"
	"im-agent/internal/daemon"
	"im-agent/internal/im"
	"im-agent/internal/store"
	"time"
)

func (a *API) GetPersonalReview(event string) (*store.PersonalReview, error) {
	ctx, stop, dir, err := a.taskBoardContext()
	if err != nil {
		return nil, err
	}
	defer stop()
	r, err := store.PersonalReviewSnapshot(ctx, dir, event)
	if r != nil {
		wire, e := (im.Address{Channel: "dingtalk_personal", ChatID: r.Target}).DeliveryAddress()
		if e != nil {
			return nil, e
		}
		r.Target = wire.ChatID
	}
	return r, err
}
func (a *API) ReviewPersonalReply(r store.PersonalReviewAction, confirmed bool) error {
	return a.withReviewControl(confirmed, func(ctx context.Context, c *daemon.Client) error { return c.ReviewPersonalReply(ctx, r) })
}
func (a *API) ReviewGroupFollowup(r store.GroupFollowAction, confirmed bool) error {
	return a.withReviewControl(confirmed, func(ctx context.Context, c *daemon.Client) error { return c.ReviewGroupFollowup(ctx, r) })
}
func (a *API) GetGroupFollowups() (store.GroupFollowView, error) {
	ctx, stop, dir, err := a.taskBoardContext()
	if err != nil {
		return store.GroupFollowView{}, err
	}
	defer stop()
	return store.GroupFollowSnapshot(ctx, dir)
}
func (a *API) withReviewControl(confirmed bool, action func(context.Context, *daemon.Client) error) error {
	if !confirmed {
		return errors.New("请先确认审核操作")
	}
	a.app.mu.Lock()
	if err := a.app.ready(); err != nil {
		a.app.mu.Unlock()
		return err
	}
	client := a.app.controller
	parent := a.app.ctx
	a.app.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	return action(ctx, client)
}
