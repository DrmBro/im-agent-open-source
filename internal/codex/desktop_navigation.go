package codex

import (
	"context"
	"im-agent/internal/diagnostics"
	"time"
)

// 恢复9月19日的导航流程：精确会话条目优先，必要时打开已确认的会话链接。
func (c *cdpClient) navigateThread(parent context.Context, token, thread string, progress func(string, string), openThread func(context.Context, string) error, draftHandlers ...func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(parent, 120*time.Second)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	var navigationDeadline time.Time
	targetReached := false
	lastReason := ""
	lastReported := ""
	report := func(stage, hint string) {
		// 仅抑制连续相同阶段；草稿→输入框→草稿时必须恢复真实状态。
		if lastReported == stage {
			return
		}
		lastReported = stage
		if progress != nil {
			progress(stage, hint)
		}
	}
	for {
		if parent.Err() != nil {
			return parent.Err()
		}
		if ctx.Err() != nil {
			return desktopNavigationTimeout(lastReason)
		}
		if !navigationDeadline.IsZero() && time.Now().After(navigationDeadline) {
			return desktopNavigationTimeout("navigation_pending")
		}
		action := "navigate"
		if targetReached {
			action = "navigate_target"
		}
		result, err := c.guardValue(ctx, action, token, thread, "")
		if err != nil {
			if parent.Err() != nil {
				return parent.Err()
			}
			if ctx.Err() != nil {
				return desktopNavigationTimeout(lastReason)
			}
			return err
		}
		if result["ok"] == true {
			return nil
		}
		reason, _ := result["reason"].(string)
		if reason == "navigation_target_missing" && openThread != nil {
			reserved, reserveErr := c.guardValue(ctx, "navigate_external", token, thread, "")
			if reserveErr != nil {
				return reserveErr
			}
			if reserved["ok"] == true {
				return nil
			}
			reason, _ = reserved["reason"].(string)
			if reason == "external_navigation_ready" {
				if err = openThread(ctx, thread); err != nil {
					return err
				}
				reason = "navigation_pending"
			}
		}
		lastReason = reason
		switch reason {
		case "navigation_busy":
			navigationDeadline = time.Time{}
			report("waiting_desktop_submit", "等待已有会话切换结束，本条尚未输入；导航占用不代表上一条消息正在发送。")
		case "submission_busy":
			navigationDeadline = time.Time{}
			report("waiting_desktop_submit", "等待已有桌面输入事务释放，本条尚未输入，不会重复提交或强行清空草稿。")
		case "waiting_draft":
			targetReached = true
			if len(draftHandlers) > 0 && draftHandlers[0] != nil {
				handled, draftErr := draftHandlers[0](ctx)
				if draftErr != nil {
					return draftErr
				}
				if handled {
					continue
				}
			}
			navigationDeadline = time.Time{}
			report("waiting_draft", "桌面输入框有未发送内容，可能是你自己的草稿或此前失败的附件说明。正在等待，最多 120 秒；不会清空或覆盖。请先查看并处理草稿，不要重复发送本条消息。")
		case "waiting_composer":
			targetReached = true
			navigationDeadline = time.Time{}
			report("waiting_desktop", "桌面输入框暂不可编辑，本条消息最多等待 120 秒；不会中断已有任务，请不要重复发送。")
		case "navigation_pending":
			if navigationDeadline.IsZero() {
				navigationDeadline = time.Now().Add(15 * time.Second)
			}
			if time.Now().After(navigationDeadline) {
				return diagnostics.UserError("切换桌面会话后 15 秒内未确认目标加载，未发送；请检查桌面，不会重复点击或自动重发")
			}
			report("switching_desktop", "正在按会话 ID 切换桌面，等待目标会话和输入框就绪")
		default:
			return desktopGuardRejection(reason)
		}
		select {
		case <-parent.Done():
			return parent.Err()
		case <-ctx.Done():
			return desktopNavigationTimeout(lastReason)
		case <-ticker.C:
		}
	}
}
func desktopNavigationTimeout(reason string) error {
	switch reason {
	case "navigation_busy":
		return diagnostics.UserError("等待已有会话切换释放超过120秒，本条未发送；未强制解除其它请求的导航状态")
	case "submission_busy":
		return diagnostics.UserError("等待已有桌面输入事务释放超过120秒，本条未发送；不会重复提交或强行清空草稿")
	case "waiting_draft":
		return diagnostics.UserError("等待草稿处理超过 120 秒，本条未发送。桌面输入框仍有未发送内容；若是此前失败留下的附件说明，请先复制备份并手动清除，再发送新消息。不会自动重放旧请求。")
	case "waiting_composer":
		return diagnostics.UserError("等待输入框恢复可编辑超过 120 秒，本条未发送。请检查桌面任务、弹窗或输入框状态；不会中断已有任务或自动重放旧请求。")
	case "navigation_pending":
		return diagnostics.UserError("等待目标会话加载超时，本条未发送。未确认桌面已切到目标会话，不会发送到其他会话。")
	default:
		return diagnostics.UserError("等待桌面状态确认超时，本条未发送；未取得足够信息，未清空草稿或自动重发。")
	}
}
