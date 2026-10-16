package daemon

import (
	"context"
	"errors"
	"net/http"

	"im-agent/internal/bridge"
	"im-agent/internal/service"
)

func registerTaskDelivery(mux *http.ServeMux, control *service.Controller) {
	mux.HandleFunc("POST /task-report", func(w http.ResponseWriter, r *http.Request) {
		var request bridge.SendTaskReportRequest
		if err := decode(r, &request); err != nil {
			reply(w, nil, errors.New("任务报告请求格式无效"))
			return
		}
		reply(w, response{}, control.SendTaskReport(r.Context(), request))
	})
}

func (c *Client) SendTaskReport(ctx context.Context, request bridge.SendTaskReportRequest) error {
	return c.call(ctx, "POST", "/task-report", request, nil)
}
