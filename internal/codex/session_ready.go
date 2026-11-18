package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"im-agent/internal/diagnostics"
)

// 就绪检查只加载、查询同一会话；此循环中绝不发送 turn/start。
func (c *Client) waitSessionReady(parent context.Context, thread string, hooks TurnHooks) error {
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	attempts, readFailures := 0, 0
	last := "等待目标会话空闲超时"
	progress := ""
	var nextResume time.Time
	report := func(text string) {
		if hooks.Progress != nil && progress != text {
			hooks.Progress("waiting_desktop", text)
			progress = text
		}
	}
	for {
		if ctx.Err() != nil {
			return diagnostics.UserError(last + "；本条尚未提交")
		}
		raw, err := c.sessionControlCall(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": false})
		if err != nil {
			readFailures++
			if !retryableSessionRead(err) || readFailures >= 3 {
				return sessionReadyError("查询目标会话", err)
			}
			last = "查询目标会话连续超时或连接中断"
			report("正在重新核对目标会话状态，本条尚未提交。")
		} else {
			readFailures = 0
			state, decodeErr := controlSessionState(raw, thread)
			if decodeErr != nil {
				return decodeErr
			}
			switch state {
			case "idle":
				return nil
			case "active":
				last = "等待目标会话已有任务结束超时"
				report("目标会话仍在执行，等待其结束后再提交本条问题。")
			case "notLoaded":
				last = "目标会话在有限次加载后仍未就绪"
				if attempts >= 3 {
					return diagnostics.UserError(last + "；本条尚未提交")
				}
				if !time.Now().Before(nextResume) {
					attempts++
					report(fmt.Sprintf("正在加载目标会话（第 %d/3 次），本条尚未提交。", attempts))
					// 不请求全量历史，也不覆盖模型、目录或会话权限。
					resumed, resumeErr := c.sessionControlCall(ctx, "thread/resume", map[string]any{"threadId": thread, "excludeTurns": true})
					if resumeErr != nil {
						if !retryableSessionRead(resumeErr) {
							return sessionReadyError("加载目标会话", resumeErr)
						}
						last = "加载回执超时，重新查询后仍未能确认会话就绪"
					} else {
						// 成功响应的身份冲突必须停止；格式缺失则通过独立只读查询核实。
						var identity struct {
							Thread struct {
								ID string `json:"id"`
							} `json:"thread"`
						}
						if json.Unmarshal(resumed, &identity) == nil && identity.Thread.ID != "" && identity.Thread.ID != thread {
							return diagnostics.UserError("加载回执的会话身份不一致，本条尚未提交")
						}
					}
					nextResume = time.Now().Add(3 * time.Second)
					report("加载请求已返回或回执未确认，正在重新查询同一会话的实际状态。")
					// 无论加载回执是否完整，都先重新 thread/read；不能直接重发加载或提交消息。
					continue
				}
			case "systemError":
				return diagnostics.UserError("目标会话处于执行服务异常状态，本条尚未提交")
			default:
				return diagnostics.UserError("目标会话返回了不支持的状态，本条尚未提交")
			}
		}
		select {
		case <-ctx.Done():
			return diagnostics.UserError(last + "；本条尚未提交")
		case <-time.After(time.Second):
		}
	}
}

func controlSessionState(raw json.RawMessage, thread string) (string, error) {
	var response struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID == "" || response.Thread.Status.Type == "" {
		return "", diagnostics.UserError("目标会话状态回执格式不完整，本条尚未提交")
	}
	if response.Thread.ID != thread {
		return "", diagnostics.UserError("目标会话查询回执身份不一致，本条尚未提交")
	}
	return response.Thread.Status.Type, nil
}
func retryableSessionRead(err error) bool {
	var uncertain *SessionControlUnconfirmed
	return errors.As(err, &uncertain) || errors.Is(err, context.DeadlineExceeded)
}
func sessionReadyError(stage string, err error) error {
	return diagnostics.UserError(stage + "失败：" + diagnostics.Error(err) + "；本条尚未提交")
}
