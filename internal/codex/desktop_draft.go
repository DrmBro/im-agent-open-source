package codex

import (
	"context"
	"time"

	"im-agent/internal/diagnostics"
	"im-agent/internal/drafts"
)

// draftPreservation 仅属于一次受全局桌面锁保护的提交，不恢复其它请求的历史快照。
type draftPreservation struct {
	client  *cdpClient
	options desktopSendOptions
	token   string
	thread  string
	record  *drafts.Record
}

func (d *draftPreservation) suspend(ctx context.Context) (bool, error) {
	if d.record != nil {
		return false, diagnostics.UserError("草稿暂存后输入框再次出现内容，已停止提交；新输入不会被覆盖，原草稿可在本机草稿恢复中查看")
	}
	// 只重取草稿快照，不重试发送；候选提交改变文本时保留变化前后的备份。
	for attempt := 0; attempt < 3; attempt++ {
		result, err := d.client.guardValue(ctx, "draft_capture", d.token, d.thread, "")
		if err != nil {
			return false, diagnostics.UserError("无法确认桌面草稿，未尝试移走或发送；请检查桌面")
		}
		if result["ok"] != true {
			reason, _ := result["reason"].(string)
			if reason == "draft_other_thread" {
				// 第一版不跨会话搬运草稿，继续原有等待/目标校验。
				return false, nil
			}
			return false, draftRejection(reason)
		}
		if result["empty"] == true {
			return false, nil
		}
		text, ok := result["text"].(string)
		if !ok || len(text) == 0 || len(text) > 65536 || d.options.Drafts == nil || d.options.RequestID == "" {
			return false, diagnostics.UserError("草稿快照或请求身份不可确认，原草稿未移走，本条未发送")
		}
		if d.options.Progress != nil {
			d.options.Progress("preserving_draft", "正在保存本机加密草稿快照；保存及复核成功前不会移走草稿。")
		}
		record, err := d.options.Drafts.Save(d.options.RequestID, d.thread, text)
		if err != nil {
			return false, diagnostics.UserError("本机加密草稿备份未成功，原草稿未移走，本条未发送；请检查钥匙串授权与本机存储")
		}
		d.record = &record
		if ctx.Err() != nil {
			return false, diagnostics.UserError("草稿备份后请求已取消，未继续提交；快照保留在本机")
		}
		result, err = d.client.guardValue(ctx, "draft_settle", d.token, d.thread, "")
		if err != nil || result["ok"] != true {
			return false, diagnostics.UserError("草稿已备份，但编辑器状态发生变化，未移走或发送；快照保留在本机")
		}
		if result["recapture"] == true {
			if err = d.options.Drafts.Mark(record.ID, "retained", "input_settled_recapture"); err != nil {
				return false, diagnostics.UserError("草稿快照状态保存未确认，原文备份保留，未继续提交")
			}
			if d.options.OnDraftRetained != nil {
				d.options.OnDraftRetained()
			}
			d.record = nil
			continue
		}
		result, err = d.client.guardValue(ctx, "draft_take", d.token, d.thread, "")
		if err != nil || result["ok"] != true {
			return false, diagnostics.UserError("草稿移走前复核冲突或清空结果未确认，已停止提交；不覆盖后续输入，原草稿快照保留在本机")
		}
		if err = d.options.Drafts.Mark(record.ID, "suspended", "draft_suspended"); err != nil {
			return false, diagnostics.UserError("草稿已有加密备份，但暂存状态保存未确认，已暂停本条提交；请查看本机草稿恢复记录")
		}
		return true, nil
	}
	return false, diagnostics.UserError("草稿在自动处理期间持续变化，已保留备份且未清空新输入；本条未发送")
}

// 导航可能加载目标会话原先保存的草稿（包括纯空白），必须在prepare前重新检查。
func (d *draftPreservation) ready(ctx context.Context) error {
	if d.record == nil {
		_, err := d.suspend(ctx)
		return err
	}
	result, err := d.client.guardValue(ctx, "draft_send_ready", d.token, d.thread, "")
	if err != nil || result["ok"] != true {
		return diagnostics.UserError("草稿暂存后桌面状态发生变化，已停止提交；不会再次清空新输入，原草稿快照保留在本机")
	}
	return nil
}

// 在发送事务清理之后、桌面锁释放之前调用；恢复错误绝不变成消息重提依据。
func restoreFailureReason(result map[string]any, err error, phase string) string {
	if err != nil {
		return "restore_" + phase + "_rpc_failed"
	}
	reason, _ := result["reason"].(string)
	switch reason {
	case "draft_restore_blocked", "draft_restore_target_changed", "draft_restore_structure_changed",
		"draft_restore_not_empty", "draft_restore_focus_changed", "draft_restore_not_armed",
		"draft_restore_selection_unavailable", "draft_restore_command_failed", "draft_restore_input_unobserved",
		"draft_restore_content_mismatch":
		return reason
	default:
		return "restore_" + phase + "_rejected"
	}
}

func (d *draftPreservation) finish(parent context.Context, mayRestore bool) {
	defer func() {
		cleanupCtx, endCleanup := context.WithTimeout(context.Background(), time.Second)
		defer endCleanup()
		_, _ = d.client.guardValue(cleanupCtx, "draft_cleanup", d.token, d.thread, "")
	}()
	if d.record == nil {
		return
	}
	retained := func(reason string) {
		_ = d.options.Drafts.Mark(d.record.ID, "retained", reason)
		if d.options.OnDraftRetained != nil {
			d.options.OnDraftRetained()
		}
	}
	if !mayRestore || parent.Err() != nil {
		retained("submission_unconfirmed")
		return
	}
	if !d.options.AutoRestoreDraft {
		retained("auto_restore_disabled")
		return
	}
	if d.options.Progress != nil {
		d.options.Progress("restoring_draft", "正在确认能否安全恢复桌面草稿；草稿恢复与飞书消息提交结果分别确认。")
	}
	ctx, stop := context.WithTimeout(parent, 3*time.Second)
	defer stop()
	// Enter回执只证明按键已交付，必须另等输入框确实空白，绝不能覆盖残留输入。
	for {
		result, err := d.client.guardValue(ctx, "draft_restore_prepare", d.token, d.thread, "")
		if err != nil {
			retained(restoreFailureReason(result, err, "prepare"))
			return
		}
		if result["ok"] == true {
			if result["already"] != true {
				applied, applyErr := d.client.guardValue(ctx, "draft_restore_apply", d.token, d.thread, "")
				if applyErr != nil || applied["ok"] != true {
					retained(restoreFailureReason(applied, applyErr, "apply"))
					return
				}
				confirmed, confirmErr := d.client.guardValue(ctx, "draft_restore_confirm", d.token, d.thread, "")
				if confirmErr != nil || confirmed["ok"] != true {
					retained(restoreFailureReason(confirmed, confirmErr, "confirm"))
					return
				}
			}
			if err = d.options.Drafts.Mark(d.record.ID, "restored", "auto_restored"); err != nil {
				retained("snapshot_state_unconfirmed")
			}
			return
		}
		if result["reason"] != "draft_wait_empty" {
			retained(restoreFailureReason(result, nil, "prepare"))
			return
		}
		select {
		case <-ctx.Done():
			retained("restore_wait_timeout")
			return
		case <-time.After(75 * time.Millisecond):
		}
	}
}

func draftRejection(reason string) error {
	switch reason {
	case "draft_unsupported":
		return diagnostics.UserError("当前草稿不是可确认的无附件纯文本，或编辑器结构不受支持；原草稿保持不动，本条未发送")
	case "draft_structure_budget":
		return diagnostics.UserError("草稿换行结构超过安全恢复上限，原草稿未移走，本条未发送；请保留原文并手动处理")
	case "draft_too_large":
		return diagnostics.UserError("桌面草稿超过64KiB暂存上限，原草稿保持不动，本条未发送")
	case "draft_busy":
		return diagnostics.UserError("桌面仍有未结束的草稿或发送事务，未覆盖草稿或重复提交")
	default:
		return diagnostics.UserError("无法安全暂存当前桌面草稿，原草稿未移走，本条未发送")
	}
}
