package bridge

import (
	"im-agent/internal/codex"
	"im-agent/internal/store"
	"strings"
)

// 只有持久请求与随机上下文令牌同时匹配，才把桥接附件说明替换为用户原文/文件名。
// 普通用户引用相似文本不伪装成已验证的IM来源，来源确认仍由原始日志账本负责。
func (b *Bridge) userMessageForDisplay(thread string, message codex.HistoryMessage) codex.HistoryMessage {
	if message.Role != "user" {
		return message
	}
	context, ok := codex.ParseAttachmentInput(message.Text)
	if !ok {
		if !strings.Contains(message.Text, "用户通过 IM 提供了附件。以下是本机附件路径") {
			return message
		}
		original, _ := codex.DesktopInputForms(message.Text)
		display, authorized, err := b.store.LegacyAttachmentDisplay(thread, store.Hash(original))
		if err == nil && authorized {
			message.Text = display
		}
		return message
	}
	display, authorized, err := b.store.AttachmentContextDisplay(thread, context.RequestID, context.Token)
	if err != nil {
		b.log.Warn("附件展示元数据暂不可读，不展示本机路径")
		message.Text = "（附件任务说明暂无法核对，请查看原始会话；未展示本机路径）"
		return message
	}
	if authorized {
		message.Text = display
	}
	return message
}
func (b *Bridge) mirrorEntryForThread(thread string, message codex.HistoryMessage, offset int64, markdown bool) store.MirrorEntry {
	shown := b.userMessageForDisplay(thread, message)
	entry := b.mirrorEntry(shown, offset, markdown)
	// 展示摘要不是身份，来源指纹必须仍是完整原始Codex输入。
	original, _ := codex.DesktopInputForms(message.Text)
	entry.Fingerprint = store.Hash(original)
	return entry
}
