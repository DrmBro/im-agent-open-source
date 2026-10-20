package daemon

import (
	"context"
	"errors"
	"net/http"
	"time"

	"im-agent/internal/qa"
	"im-agent/internal/service"
)

func registerQAOperations(mux *http.ServeMux, control *service.Controller) {
	mux.HandleFunc("POST /qa/resolve-request", func(w http.ResponseWriter, r *http.Request) {
		var body qa.Resolution
		if decode(r, &body) != nil {
			reply(w, nil, errors.New("问答审批请求格式无效"))
			return
		}
		reply(w, response{}, control.ResolveQARequest(body))
	})
	mux.HandleFunc("POST /qa/resolve-delivery", func(w http.ResponseWriter, r *http.Request) {
		var body service.QADeliveryResolution
		if decode(r, &body) != nil {
			reply(w, nil, errors.New("问答投递请求格式无效"))
			return
		}
		reply(w, response{}, control.ResolveQADelivery(body))
	})
}
func (c *Client) ResolveQARequest(request qa.Resolution) error {
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	return c.call(ctx, "POST", "/qa/resolve-request", request, nil)
}
func (c *Client) ResolveQADelivery(request service.QADeliveryResolution) error {
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	return c.call(ctx, "POST", "/qa/resolve-delivery", request, nil)
}
