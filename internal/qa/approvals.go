package qa

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"im-agent/internal/store"
)

type Resolution struct {
	ID       string              `json:"id"`
	Decision string              `json:"decision"`
	Answers  map[string][]string `json:"answers,omitempty"`
}
type approval struct {
	request   store.QARequest
	execution *execution
	result    chan any
	resolved  bool
}

func (r *Runner) handleRequest(_ context.Context, method string, raw json.RawMessage) (any, error) {
	if len(raw) > 64<<10 {
		return nil, errors.New("问答交互请求过大，未自动批准")
	}
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Command  string `json:"command"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal(raw, &p) != nil || p.ThreadID == "" || p.TurnID == "" {
		return nil, errors.New("问答交互请求缺少真实会话或轮次")
	}
	r.mu.Lock()
	running := r.active[p.ThreadID]
	r.mu.Unlock()
	if running == nil {
		return nil, errors.New("该 CLI 请求没有问答执行上下文")
	}
	select {
	case <-running.ready:
	case <-running.ctx.Done():
		return nil, running.ctx.Err()
	}
	if running.turn == "" || running.turn != p.TurnID {
		return nil, errors.New("问答交互请求轮次不匹配")
	}
	if running.restricted {
		// Business replies are generated from approved text. Host commands and writes
		// are denied uniformly, never escalated into an administrator-only bypass.
		switch method {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
			return map[string]string{"decision": "decline"}, nil
		case "item/permissions/requestApproval":
			return map[string]any{"permissions": map[string]any{}, "scope": "turn"}, nil
		default:
			return nil, errors.New("业务模式不使用本机交互工具，请在群内正文澄清")
		}
	}
	detail := ""
	switch method {
	case "item/commandExecution/requestApproval":
		detail = "Codex 请求执行命令：\n" + p.Command
	case "item/fileChange/requestApproval":
		detail = "Codex 请求修改文件。请核对问答与权限范围。"
	case "item/permissions/requestApproval":
		detail = "Codex 请求扩大本轮权限：\n" + p.Reason
	case "item/tool/requestUserInput":
		detail = "Codex 需要你补充信息。"
	default:
		return nil, errors.New("问答未实现该类交互请求，未自动批准")
	}
	a := &approval{request: store.QARequest{ID: store.ID(), QuestionID: running.event.QuestionID, EventID: running.event.ID, Method: method, Detail: detail, Payload: append(json.RawMessage(nil), raw...)}, execution: running, result: make(chan any, 1)}
	if e := r.db.QAPutRequest(a.request); e != nil {
		return nil, e
	}
	r.mu.Lock()
	r.approvals[a.request.ID] = a
	r.mu.Unlock()
	running.waiting.Add(1)
	defer running.waiting.Add(-1)
	defer func() {
		r.mu.Lock()
		delete(r.approvals, a.request.ID)
		r.mu.Unlock()
		if e := r.db.QARequestState(a.request.ID, "expired"); e != nil {
			r.fail(e)
		}
	}()
	stage, waiting := "waiting_approval", "钉钉问答正在等待本机确认，请在“任务与附件 → 钉钉问答”处理"
	if method == "item/tool/requestUserInput" {
		stage = "waiting_input"
		waiting = "钉钉问答正在等待补充信息，请在本机问答面板查看"
	}
	if e := r.db.QASetProgress(running.event.ID, stage, waiting, true); e != nil {
		return nil, e
	}
	r.diagnostic(stage, waiting)
	select {
	case <-running.ctx.Done():
		return nil, running.ctx.Err()
	case result := <-a.result:
		if running.ctx.Err() != nil {
			return nil, running.ctx.Err()
		}
		return result, nil
	}
}
func (r *Runner) ResolveRequest(res Resolution) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.approvals[res.ID]
	if a == nil || a.resolved || a.execution.ctx.Err() != nil || r.active[a.execution.thread] != a.execution {
		return errors.New("该问答请求已失效或已处理")
	}
	if res.Decision != "accept" && res.Decision != "decline" {
		return errors.New("审批决定无效")
	}
	var result any
	switch a.request.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]string{"decision": res.Decision}
	case "item/permissions/requestApproval":
		permissions := map[string]any{}
		if res.Decision == "accept" {
			var p struct {
				Permissions map[string]any `json:"permissions"`
			}
			if json.Unmarshal(a.request.Payload, &p) != nil || p.Permissions == nil {
				return errors.New("无法识别请求的权限，未放行")
			}
			permissions = p.Permissions
		}
		result = map[string]any{"permissions": permissions, "scope": "turn"}
	case "item/tool/requestUserInput":
		var p struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		}
		if json.Unmarshal(a.request.Payload, &p) != nil || len(p.Questions) == 0 {
			return errors.New("补充问题格式无效")
		}
		answers := map[string]any{}
		for _, q := range p.Questions {
			values := res.Answers[q.ID]
			if res.Decision == "decline" {
				values = []string{"用户取消补充，请停止需要该信息的操作。"}
			}
			if len(values) == 0 || len(values) > 16 {
				return errors.New("请填写每项补充信息")
			}
			for _, v := range values {
				if strings.TrimSpace(v) == "" || len(v) > 8192 {
					return errors.New("补充信息为空或过长")
				}
			}
			answers[q.ID] = map[string]any{"answers": values}
		}
		result = map[string]any{"answers": answers}
	default:
		return errors.New("不支持的问答交互类型")
	}
	if e := r.db.QARequestState(res.ID, res.Decision); e != nil {
		return e
	}
	a.resolved = true
	a.result <- result
	return nil
}
