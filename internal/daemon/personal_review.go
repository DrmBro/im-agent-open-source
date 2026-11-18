package daemon

import (
	"context"
	"errors"
	"im-agent/internal/service"
	"im-agent/internal/store"
	"net/http"
)

func registerPersonalReview(mux *http.ServeMux, c *service.Controller) {
	mux.HandleFunc("POST /group-followup", func(w http.ResponseWriter, r *http.Request) {
		var body store.GroupFollowAction
		if decode(r, &body) != nil {
			reply(w, nil, errors.New("群跟进操作格式无效"))
			return
		}
		reply(w, response{}, c.ReviewGroupFollowup(r.Context(), body))
	})
	mux.HandleFunc("POST /personal-review", func(w http.ResponseWriter, r *http.Request) {
		var body store.PersonalReviewAction
		if decode(r, &body) != nil {
			reply(w, nil, errors.New("个人审核请求格式无效"))
			return
		}
		reply(w, response{}, c.ReviewPersonalReply(r.Context(), body))
	})
}
func (c *Client) ReviewPersonalReply(ctx context.Context, r store.PersonalReviewAction) error {
	return c.call(ctx, "POST", "/personal-review", r, nil)
}

func (c *Client) ReviewGroupFollowup(ctx context.Context, r store.GroupFollowAction) error {
	return c.call(ctx, "POST", "/group-followup", r, nil)
}
