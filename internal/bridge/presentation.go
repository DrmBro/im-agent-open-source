package bridge

import (
	"context"
	"errors"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) syncPresentations(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if e := b.store.EnsurePresentations(); e != nil {
			b.log.Warn("准备群名与历史补齐记录失败")
		} else {
			rows, e := b.store.Presentations(false)
			if e != nil {
				b.log.Warn("读取群名与历史补齐任务失败")
			} else {
				for _, p := range rows {
					if ctx.Err() != nil {
						return
					}
					op, cancel := context.WithTimeout(ctx, 60*time.Second)
					b.presentOne(op, p)
					cancel()
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (b *Bridge) activePresentation(thread string) (store.Session, error) {
	s, e := b.store.ByThread(thread)
	if e != nil {
		return s, e
	}
	if !s.Managed || s.Status != "active" {
		return s, errors.New("会话不是活跃的客户端自建群")
	}
	return s, nil
}
func (b *Bridge) presentOne(ctx context.Context, p store.Presentation) {
	s, e := b.activePresentation(p.ThreadID)
	if e != nil {
		return
	}
	a := b.channels[s.Address.Connection]
	fail := func(field, message string, unknown bool) {
		if e := b.store.PresentationFailure(p.ThreadID, field, message, unknown); e != nil {
			b.log.Warn("保存展示失败状态失败")
		}
	}
	if a == nil {
		field := "name"
		if p.NameApplied {
			field = "history"
		}
		fail(field, "原 IM 连接未配置，等待恢复", false)
		return
	}

	// 每轮独立核对真实标题，既有群和后续改名都能同步，不依赖历史读取成功。
	// 只改变展示任务，绝不创建替代群、改变线程/目录或重置历史发送状态。
	metadata, readErr := b.codex.ReadMetadata(ctx, p.ThreadID)
	if readErr == nil && metadata.ID != p.ThreadID {
		readErr = errors.New("返回的会话ID与目标不一致")
	}
	if readErr != nil {
		fail("name", "读取会话名称失败："+diagnostics.Error(readErr), false)
	} else {
		target := b.threadName(metadata, s.Address.Connection)
		if e = b.store.SetPresentationName(p.ThreadID, target); e != nil {
			fail("name", "保存目标名称失败", false)
		} else if fresh, refreshErr := b.store.Presentation(p.ThreadID); refreshErr != nil {
			fail("name", "读取目标名称状态失败", false)
		} else {
			p = fresh
		}
	}
	if readErr == nil && !p.NameApplied && p.Name != "" {
		if _, e = b.activePresentation(p.ThreadID); e != nil {
			return
		}
		naming, ok := a.(im.NamingAdapter)
		if s.Address.IsTopic() {
			if e = b.updateTopicPresentation(ctx, s, p.Name, "active"); e != nil {
				fail("name", "更新话题入口失败："+diagnostics.Error(e), false)
			} else if e = b.store.PresentationRenamed(p.ThreadID); e != nil {
				fail("name", "话题名称确认保存失败", false)
			}
		} else if !ok || !a.Capabilities().RenameConversation {
			fail("name", "该 IM 尚未实现改名能力", false)
		} else if e = naming.RenameConversation(ctx, s.Address, p.Name); e != nil {
			fail("name", "修改群名失败："+diagnostics.Error(e), false)
		} else if e = b.store.PresentationRenamed(p.ThreadID); e != nil {
			fail("name", "群名已修改，但本地确认保存失败", false)
		}
	}
	if (p.HistoryState == "pending" && p.Payload == "") || p.HistoryState == "sent" || p.HistoryState == "empty" {
		thread, messages, readErr := b.codex.RecentConversation(ctx, p.ThreadID)
		if readErr != nil {
			fail("history", "读取 Codex 历史失败："+diagnostics.Error(readErr), false)
			return
		}
		body := ""
		if len(messages) > 0 {
			body = b.historyText(messages, p.ThreadID)
		}
		source := presentationSourceFromMessages(messages)
		if body != "" {
			pruned, checkErr := b.store.PresentationSourcePruned(p.ThreadID, source)
			if checkErr != nil {
				fail("history", "无法确认本地历史保留状态，未回填正文", false)
				return
			}
			if pruned {
				return
			} // 保留飞书原卡，不把已清理的同一旧摘要重新写回本地。
		}
		title := p.Name
		if title == "" {
			title = b.threadName(thread, s.Address.Connection)
		}
		if p.HistoryState == "pending" || p.HistoryState == "empty" {
			if e = b.store.PreparePresentation(p.ThreadID, title, body, len(messages), source); e != nil {
				fail("history", "保存历史快照失败，尚未发送", false)
				return
			}
		} else if body != "" && p.MessageID != "" {
			if body != p.Payload {
				if e = a.UpdateCard(ctx, s.Address, p.MessageID, im.Card{Title: "最新对话", Text: body}); e != nil {
					fail("history", "更新最新对话失败："+diagnostics.Error(e), false)
					return
				}
			}
			if e = b.store.HistoryUpdated(p.ThreadID, body, len(messages), source); e != nil {
				b.log.Warn("保存最近一轮对话快照失败")
			}
		}
	}
	p, e = b.store.Presentation(p.ThreadID)
	if e != nil {
		return
	}
	if p.HistoryState != "pending" || p.Payload == "" || ctx.Err() != nil {
		return
	}
	claimed, e := b.store.BeginHistorySend(p.ThreadID)
	if e != nil || !claimed {
		return
	}
	key := store.Hash(s.Address.Key() + "\x00" + s.ThreadID + "\x00history-v1")[:32]
	// 历史只作为 IM 文本内容，绝不调用 turn/start 或恢复会话。
	var messageID string
	if a.Capabilities().InteractiveCards {
		messageID, e = a.SendCard(ctx, s.Address, im.Card{Title: "最新对话", Text: p.Payload}, key)
	} else {
		messageID, e = a.SendText(ctx, s.Address, p.Payload, key)
	}
	if e != nil {
		var unknown *im.DeliveryUncertain
		uncertain := errors.As(e, &unknown)
		hint := "发送历史失败：" + diagnostics.Error(e)
		if uncertain {
			hint = "发送结果不确定，可能已送达；不自动重发，请在群内检查"
		}
		fail("history", hint, uncertain)
		return
	}
	if messageID == "" {
		fail("history", "渠道未返回消息 ID，送达结果不确定，不自动重发", true)
		return
	}
	if e = b.store.HistorySent(p.ThreadID, messageID); e != nil {
		fail("history", "历史已发送，但记录未能确认；不自动重复发送", true)
	}
}
func (b *Bridge) RetryPresentation(thread string) error { return b.store.RetryPresentation(thread) }

// 周期同步与任务完成刷新共用同一来源规则，不用正文相同推断消息相同。
func presentationSourceFromMessages(messages []codex.HistoryMessage) store.PresentationSource {
	if len(messages) == 2 && messages[0].Role == "user" && messages[1].Role == "assistant" {
		return store.PresentationSource{UserItemID: messages[0].ID, AssistantItemID: messages[1].ID}
	}
	return store.PresentationSource{}
}
