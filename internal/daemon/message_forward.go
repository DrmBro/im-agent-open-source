package daemon

import (
	"context"
	"errors"
	"im-agent/internal/bridge"
	"im-agent/internal/service"
	"net/http"
)

func registerMessageForward(mux *http.ServeMux, c *service.Controller) {
	mux.HandleFunc("POST /message-forward", func(w http.ResponseWriter, r *http.Request) {
		var req bridge.MessageForwardRequest
		if decode(r, &req) != nil {
			reply(w, nil, errors.New("消息投递请求无效"))
			return
		}
		result, err := c.MessageForward(r.Context(), req)
		reply(w, result, err)
	})
}
func (c *Client) MessageForward(ctx context.Context, r bridge.MessageForwardRequest) (bridge.MessageForwardView, error) {
	var out bridge.MessageForwardView
	err := c.call(ctx, "POST", "/message-forward", r, &out)
	return out, err
}
