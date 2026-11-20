package daemon

import (
	"context"
	"errors"
	"net/http"

	"im-agent/internal/bridge"
	"im-agent/internal/service"
)

func registerAssistant(mux *http.ServeMux, c *service.Controller) {
	mux.HandleFunc("POST /assistant", func(w http.ResponseWriter, r *http.Request) {
		var req bridge.AssistantRequest
		if decode(r, &req) != nil {
			reply(w, nil, errors.New("消息助理请求无效"))
			return
		}
		out, err := c.Assistant(r.Context(), req)
		reply(w, out, err)
	})
}

func (c *Client) Assistant(ctx context.Context, r bridge.AssistantRequest) (bridge.AssistantResponse, error) {
	var out bridge.AssistantResponse
	err := c.call(ctx, "POST", "/assistant", r, &out)
	return out, err
}
