package daemon

import (
	"context"
	"errors"
	"im-agent/internal/bridge"
	"im-agent/internal/service"
	"im-agent/internal/store"
	"net/http"
)

func registerPersonalInbox(mux *http.ServeMux, c *service.Controller) {
	mux.HandleFunc("GET /personal-inbox", func(w http.ResponseWriter, r *http.Request) { v, e := c.PersonalInbox(r.Context()); reply(w, v, e) })
	mux.HandleFunc("POST /personal-inbox", func(w http.ResponseWriter, r *http.Request) {
		var a bridge.PersonalInboxAction
		if decode(r, &a) != nil {
			reply(w, nil, errors.New("收件操作无效"))
			return
		}
		reply(w, struct{}{}, c.ReviewPersonalInbox(r.Context(), a))
	})
}
func (c *Client) PersonalInbox(ctx context.Context) ([]store.PersonalInboxItem, error) {
	var out []store.PersonalInboxItem
	err := c.call(ctx, "GET", "/personal-inbox", nil, &out)
	return out, err
}
func (c *Client) ReviewPersonalInbox(ctx context.Context, a bridge.PersonalInboxAction) error {
	return c.call(ctx, "POST", "/personal-inbox", a, nil)
}
