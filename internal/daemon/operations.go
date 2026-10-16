package daemon

import (
	"context"
	"errors"
	"im-agent/internal/bridge"
	"im-agent/internal/service"
	"im-agent/internal/store"
	"net/http"
)

func registerOperations(mux *http.ServeMux, control *service.Controller, dir string) {
	registerTaskDelivery(mux, control)
	mux.HandleFunc("POST /creation-form", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Thread string `json:"thread"`
		}
		if e := decode(r, &body); e != nil {
			reply(w, nil, errors.New("表单请求无效"))
			return
		}
		reply(w, response{}, control.OpenCreationForm(r.Context(), body.Thread))
	})
	mux.HandleFunc("POST /session-status", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Thread string `json:"thread"`
		}
		if e := decode(r, &body); e != nil {
			reply(w, nil, errors.New("状态卡请求无效"))
			return
		}
		reply(w, response{}, control.OpenSessionStatus(r.Context(), body.Thread))
	})
	mux.HandleFunc("POST /send-file", func(w http.ResponseWriter, r *http.Request) {
		var req bridge.SendFileRequest
		if e := decode(r, &req); e != nil {
			reply(w, nil, errors.New("附件请求格式无效"))
			return
		}
		reply(w, response{}, control.SendFile(r.Context(), req))
	})
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) { v, e := store.TaskCenterSnapshot(dir); reply(w, v, e) })
	mux.HandleFunc("POST /delivery", func(w http.ResponseWriter, r *http.Request) {
		var req store.DeliveryResolution
		if e := decode(r, &req); e != nil {
			reply(w, nil, errors.New("投递操作格式无效"))
			return
		}
		reply(w, response{}, control.ResolveDelivery(req))
	})
}
func (c *Client) ResolveDelivery(r store.DeliveryResolution) error {
	return c.call(context.Background(), "POST", "/delivery", r, nil)
}

func (c *Client) SendFile(ctx context.Context, r bridge.SendFileRequest) error {
	return c.call(ctx, "POST", "/send-file", r, nil)
}

func (c *Client) OpenCreationForm(ctx context.Context, thread string) error {
	return c.call(ctx, "POST", "/creation-form", map[string]string{"thread": thread}, nil)
}

func (c *Client) OpenSessionStatus(ctx context.Context, thread string) error {
	return c.call(ctx, "POST", "/session-status", map[string]string{"thread": thread}, nil)
}
