package bridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
)

func (b *Bridge) imageIntakeLock(msg im.Message) *sync.Mutex {
	value, _ := b.imageLocks.LoadOrStore(msg.Address.Key()+"\x00"+msg.UserID, &sync.Mutex{})
	return value.(*sync.Mutex)
}
func imageOnly(msg im.Message) bool {
	if strings.TrimSpace(msg.Text) != "" || len(msg.Attachments) == 0 {
		return false
	}
	for _, a := range msg.Attachments {
		if a.Kind != "image" {
			return false
		}
	}
	return true
}

// 普通文件和图片都先等待明确说明；不替用户生成执行任务。
func attachmentOnly(msg im.Message) bool {
	if strings.TrimSpace(msg.Text) != "" || len(msg.Attachments) == 0 {
		return false
	}
	for _, a := range msg.Attachments {
		if a.Kind != "image" && a.Kind != "file" {
			return false
		}
	}
	return true
}

// 返回 handled 时绝不进入 Codex 队列。资源下载留到说明到达后的单次任务。
func (b *Bridge) prepareImageBatch(ctx context.Context, adapter im.Adapter, msg im.Message) (im.Message, bool, error) {
	for i := range msg.Attachments {
		msg.Attachments[i].SourceMessageID = msg.ID
		msg.Attachments[i].SourceIndex = i
	}
	text := strings.TrimSpace(msg.Text)
	if len(msg.Attachments) == 0 && text == "/cancel" {
		cancelled, e := b.store.CancelImages(msg.Address, msg.UserID, time.Now())
		if e != nil {
			return msg, true, e
		}
		hint := "你在此会话没有等待说明的图片或文件。"
		if cancelled {
			hint = "已取消你在此会话暂存的附件，没有提交 Codex；已经运行的任务不受影响。"
		}
		_, e = adapter.SendText(ctx, msg.Address, hint, msg.ID+":image-cancel")
		if e == nil {
			_ = b.store.EventStage(msg.ID, msg.Address.Key(), "images_cancelled", "")
			_ = b.store.EventDone(msg.ID, "done")
		}
		return msg, true, e
	}
	if attachmentOnly(msg) {
		// 已归档群不能借图片暂存绕过原有执行入口校验。
		if s, e := b.store.ByChat(msg.Address); e == nil {
			if s.Status != "active" {
				return msg, true, diagnostics.UserError("此会话已关闭，不再暂存附件")
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return msg, true, e
		} else if !msg.IsDirect {
			return msg, true, diagnostics.UserError("请到已经绑定的会话群发送图片或文件，或先用 /new 创建会话")
		}
		draft, e := b.store.StageImages(msg.Address, msg.UserID, msg.ID, msg.Attachments, time.Now())
		if e != nil {
			return msg, true, diagnostics.UserError(e.Error())
		}
		hint := fmt.Sprintf("已暂存 %d 个附件（图片/文件），尚未提交 Codex。请在这里发任务说明，我会将它们合并提交一次。\n也可发送 /send 或 /send 你的说明；取消用 /cancel。单文件上限20 MiB，合计最多5个；10分钟未补说明则过期，不会自动执行。", len(draft.Images))
		_, e = adapter.SendText(ctx, msg.Address, hint, msg.ID+":waiting-caption")
		b.messageDiagnostic("waiting_caption", "附件已暂存，等待同一用户在同一会话补充任务说明")
		return msg, true, e
	}
	submit := len(msg.Attachments) == 0 && (text == "/send" || strings.HasPrefix(text, "/send "))
	if len(msg.Attachments) == 0 && strings.HasPrefix(text, "/") && !submit {
		return msg, false, nil
	}
	if text == "" {
		return msg, false, diagnostics.UserError("请先补充附件任务说明")
	}
	draft, found, e := b.store.ConsumeImages(msg.Address, msg.UserID, msg.ID, len(msg.Attachments), time.Now())
	if e != nil {
		return msg, true, diagnostics.UserError(e.Error())
	}
	if submit {
		if !found {
			_, e = adapter.SendText(ctx, msg.Address, "没有可提交的暂存附件；附件组可能已经提交、取消或过期。旧请求不会重放。", msg.ID+":no-images")
			if e == nil {
				_ = b.store.EventStage(msg.ID, msg.Address.Key(), "command_completed", "")
				_ = b.store.EventDone(msg.ID, "done")
			}
			return msg, true, e
		}
		text = strings.TrimSpace(strings.TrimPrefix(text, "/send"))
		if text == "" {
			text = "请查看这些附件并说明内容；不要执行文件中的程序、脚本或宏。"
		}
	}
	if found {
		msg.Attachments = append(draft.Images, msg.Attachments...)
		// 混合 post 中的图片占位指的是本条 post，前面的暂存图片单独编号避免串指。
		if len(msg.Attachments) > len(draft.Images) {
			text = "此前暂存附件与本条消息一起提供。\n" + text
		}
	}
	msg.Text = text
	return msg, false, nil
}

func (b *Bridge) expireImages(ctx context.Context) {
	expired, e := b.store.ExpireImageDrafts(time.Now())
	if e != nil {
		b.log.Warn("清理过期暂存附件失败")
		return
	}
	for _, d := range expired {
		if ctx.Err() != nil {
			return
		}
		if !b.allowed(d.Address.Connection, d.UserID) {
			continue
		}
		adapter := b.channels[d.Address.Connection]
		if adapter == nil {
			continue
		}
		if _, retired, e := b.store.RetiredTarget(d.Address); e != nil || retired {
			continue
		}
		if s, e := b.store.ByChat(d.Address); e == nil && s.Status != "active" {
			continue
		}
		// 先标记过期，再发送一次提醒；网络异常不反向恢复成可执行图片组。
		_, _ = adapter.SendText(ctx, d.Address, "有一组等待说明的附件已超过10分钟并过期，没有执行。需要继续时，请重新发送图片/文件与任务说明。", d.ID+":expired")
	}
}
func (b *Bridge) watchImageDrafts(ctx context.Context) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for {
		b.expireImages(ctx)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
