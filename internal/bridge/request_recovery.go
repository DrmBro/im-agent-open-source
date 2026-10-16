package bridge

import (
	"context"
	"encoding/json"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/diagnostics"
	"im-agent/internal/store"
)

// captureRequestReplies先持久化明确完成回执和完整回复清单，再允许网络投递。
func (b *Bridge) captureRequestReplies(ctx context.Context, s store.Session, event, turn string, replies []codex.HistoryMessage) error {
	entries := make([]store.MirrorEntry, 0, len(replies))
	for _, reply := range replies {
		entries = append(entries, b.mirrorEntry(reply, 0, b.markdownEnabled(s)))
	}
	lock := b.liveLock(s.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.store.SealRequestReplies(s, event, turn, entries)
}
func (b *Bridge) reconcileRequestReplies(s store.Session) {
	completed, e := b.store.ReconcileRequestReplies(s)
	if e != nil {
		b.log.Warn("核对请求回复回执失败", "thread", s.ThreadID, "error", diagnostics.Error(e))
		return
	}
	for _, event := range completed {
		// 更新原请求的状态卡，不创建新模型任务；展示队列未就绪时由原定时扫描补齐。
		_ = b.enqueueSessionStatus(s.ThreadID, false, event)
	}
}

func (b *Bridge) syncRequestRecovery(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		probes, e := b.store.PendingRequestProbes()
		if e != nil {
			b.log.Warn("读取恢复观察任务失败", "error", diagnostics.Error(e))
		} else {
			for _, p := range probes {
				if ctx.Err() != nil {
					return
				}
				b.observeRequestRecovery(ctx, p)
			}
		}
		// 与实时消息是否能继续读取解耦。已封存的回复也可能由人工核对确认送达。
		sessions, e := b.store.LiveMirrorSessions()
		if e == nil {
			for _, s := range sessions {
				if ctx.Err() != nil {
					return
				}
				lock := b.liveLock(s.ThreadID)
				lock.Lock()
				b.reconcileRequestReplies(s)
				lock.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (b *Bridge) observeRequestRecovery(parent context.Context, p store.RequestProbe) {
	s, e := b.store.ByThread(p.ThreadID)
	if e != nil || s.Status != "active" || store.Hash(s.Address.Key()) != p.AddressKey {
		return
	}
	var state codex.RecoveryObservation
	if p.State != "" && json.Unmarshal([]byte(p.State), &state) != nil {
		_, _ = b.store.SaveRequestProbe(p, p.Offset, p.State, "恢复观察记录损坏，未重新扫描或执行")
		return
	}
	if state.Outcome == "completed" && len(state.ReplyIDs) == 0 && state.ReplyIssue == "" {
		recheck, cancel := context.WithTimeout(parent, 3*time.Second)
		repaired, changed, readErr := b.codex.RecoverUnphasedCompletion(recheck, p.ThreadID, p.Path, p.UserItemID, p.OriginOffset, p.Offset, state)
		cancel()
		if readErr == nil && changed {
			if raw, err := json.Marshal(repaired); err == nil {
				saved, saveErr := b.store.SaveUnphasedReplyRecovery(p, string(raw))
				if saveErr != nil || !saved {
					return
				}
				p.State, state = string(raw), repaired
			}
		}
	}
	ctx, stop := context.WithTimeout(parent, 3*time.Second)
	updated, next, e := b.codex.ReadRecoveryObservation(ctx, p.ThreadID, p.Path, p.Offset, p.UserItemID, state)
	stop()
	if e != nil {
		_, _ = b.store.SaveRequestProbe(p, p.Offset, p.State, diagnostics.Error(e))
		return
	}
	raw, e := json.Marshal(updated)
	if e != nil {
		return
	}
	hint := ""
	if updated.Outcome == "completed" {
		hint = codex.ReplyRecoveryHint(updated)
	}
	saved, e := b.store.SaveRequestProbe(p, next, string(raw), hint)
	if e != nil || !saved {
		return
	}
	p.Offset, p.State = next, string(raw)
	if updated.Outcome == "" {
		return
	}
	lock := b.liveLock(p.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	if updated.Outcome == "completed" {
		// 完成证据先独立持久化；封存回复失败不再回滚执行结果。
		if e = b.store.ConfirmObservedExecution(s, p, updated.MatchedTurn); e != nil {
			_, _ = b.store.SaveRequestProbe(p, p.Offset, p.State, "执行完成证据与当前请求关联未通过复核；未重跑")
			return
		}
		if hint != "" {
			_, _ = b.store.SaveRequestProbe(p, p.Offset, p.State, hint)
			_ = b.enqueueSessionStatus(s.ThreadID, false, p.EventID)
			return
		}
		var entries []store.MirrorEntry
		if len(updated.ReplyLocations) == len(updated.ReplyIDs) && len(updated.ReplyIDs) > 0 {
			readCtx, cancel := context.WithTimeout(parent, 3*time.Second)
			replies, readErr := b.codex.ReadObservedReplies(readCtx, p.ThreadID, p.Path, updated.ReplyLocations)
			cancel()
			if readErr != nil {
				_, _ = b.store.SaveRequestProbe(p, p.Offset, p.State, "原回复位置或正文指纹未通过复核，未使用替代消息；稍后再核对")
				return
			}
			for index, reply := range replies {
				entries = append(entries, b.mirrorEntry(reply, 0, b.markdownEnabled(s)))
				location := updated.ReplyLocations[index]
				if location.TurnID != "" {
					_ = b.store.RecoverReplyPreview(p, updated.MatchedTurn, reply.ID, reply.Text, b.presentationFilter().Text(reply.Text), location.Timestamp)
				}
			}
		}
		e = b.store.SealObservedRequest(s, p, updated.ReplyIDs, updated.MatchedTurn, entries...)
	} else {
		e = b.store.FailObservedRequest(s, p, updated.MatchedTurn)
	}
	if e != nil {
		_, _ = b.store.SaveRequestProbe(p, p.Offset, p.State, "恢复证据尚未完整关联："+diagnostics.Error(e))
		_ = b.enqueueSessionStatus(s.ThreadID, false, p.EventID)
		return
	}
	b.reconcileRequestReplies(s)
	_ = b.enqueueSessionStatus(s.ThreadID, false, p.EventID)
}
