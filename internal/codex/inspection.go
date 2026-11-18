package codex

import (
	"context"
	"errors"

	"im-agent/internal/config"
)

type LogInspection struct {
	ThreadID           string `json:"thread_id"`
	UserItemID         string `json:"user_item_id"`
	AssistantItemID    string `json:"assistant_item_id"`
	TurnID             string `json:"turn_id"`
	Outcome            string `json:"outcome"`
	CompletionVerified bool   `json:"completion_verified"`
	ReplyIssue         string `json:"reply_issue"`
	StatusIssue        string `json:"status_issue"`
	MessageCount       int    `json:"message_count"`
}

// 独立只读诊断入口：不启动 App Server、不访问飞书、不提交或恢复模型轮次。
func InspectSessionLog(ctx context.Context, home, thread string) (LogInspection, error) {
	client := &Client{runtimeConfig: config.Config{Codex: config.Codex{Home: home}}}
	view, found, err := client.observeRollout(ctx, thread)
	out := LogInspection{ThreadID: thread}
	if err != nil {
		return out, err
	}
	if !found {
		return out, errors.New("未找到可核验会话日志")
	}
	out.UserItemID = view.UserItemID
	out.TurnID = view.Snapshot.TurnID
	out.Outcome = view.Snapshot.Outcome
	out.CompletionVerified = view.Snapshot.CompletionOutputVerified
	out.ReplyIssue = view.ReplyIssue
	out.StatusIssue = view.Snapshot.Issue
	out.MessageCount = len(view.Messages)
	for _, message := range view.Messages {
		if message.Role == "assistant" {
			out.AssistantItemID = message.ID
		}
	}
	return out, nil
}
