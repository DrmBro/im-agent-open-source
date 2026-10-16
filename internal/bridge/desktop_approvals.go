package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func desktopApprovalID(a codex.DesktopApproval) string {
	return store.Hash(a.ThreadID + "\x00" + a.TurnID + "\x00" + a.ItemID + "\x00" + string(a.RequestID) + "\x00" + a.Fingerprint)
}
func validDesktopApproval(a codex.DesktopApproval) bool {
	return a.ThreadID != "" && a.TurnID != "" && a.ItemID != "" && len(a.Fingerprint) == 64 && len(a.RequestID) > 0 && len(a.RequestID) <= 260 && json.Valid(a.RequestID)
}
func (b *Bridge) syncDesktopApprovals(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var warned time.Time
	for {
		if ctx.Err() != nil {
			return
		}
		sessions, err := b.store.LiveMirrorSessions()
		if err == nil {
			allowed := map[string]store.Session{}
			threads := []string{}
			for _, s := range sessions {
				if s.Status == "active" && s.Managed {
					allowed[s.ThreadID] = s
					threads = append(threads, s.ThreadID)
				}
			}
			op, cancel := context.WithTimeout(ctx, 9*time.Second)
			snapshot, readErr := b.codex.ReadDesktopApprovals(op, threads)
			cancel()
			if readErr != nil {
				if time.Since(warned) > time.Minute {
					b.log.Warn("桌面审批目录暂不可用，未自动批准或猜测审批状态")
					warned = time.Now()
				}
			} else if snapshot.State == "snapshot" {
				seen := map[string]bool{}
				complete := snapshot.Complete
				for _, request := range snapshot.Requests {
					session, ok := allowed[request.ThreadID]
					if !ok || !validDesktopApproval(request) {
						complete = false
						continue
					}
					id := desktopApprovalID(request)
					seen[id] = true
					// 只持久化脱敏展示，原请求精确指纹用于点击时核验。
					request.Command = b.presentationFilter().Text(request.Command)
					request.Reason = b.presentationFilter().Text(request.Reason)
					_ = b.store.RegisterDesktopApproval(store.DesktopApprovalCard{ID: id, ThreadID: session.ThreadID, AddressKey: store.Hash(session.Address.Key()), Fingerprint: request.Fingerprint, Payload: store.JSON(request), ExpiresAt: 0})
				}
				cards, listErr := b.store.DesktopApprovalCards()
				if listErr == nil {
					for _, card := range cards {
						session, ok := allowed[card.ThreadID]
						if !ok || store.Hash(session.Address.Key()) != card.AddressKey {
							continue
						}
						if complete && !seen[card.ID] {
							_ = b.store.SetDesktopApprovalState(card.ID, "resolved", "该审批已不再待处理，可能已在桌面处理；此卡不可再次授权")
						}
						fresh, e := b.store.DesktopApproval(card.ID)
						if e == nil {
							b.refreshDesktopApprovalCard(ctx, session, fresh)
						}
					}
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
func (b *Bridge) desktopApprovalCard(c store.DesktopApprovalCard) im.Card {
	var request codex.DesktopApproval
	_ = json.Unmarshal([]byte(c.Payload), &request)
	text := "Codex 正在等待命令执行审批。\n" + request.Reason + "\n\n命令：\n" + request.Command
	card := im.Card{Title: "需要你的审批", Text: text, ActionID: "desktop-approval:" + c.ID}
	if c.State == "pending" && (c.ExpiresAt == 0 || c.ExpiresAt > time.Now().Unix()) {
		if request.AllowOnce {
			card.Buttons = append(card.Buttons, im.Button{Label: "仅允许这一次", Value: "accept"})
		} else {
			card.Text += "\n命令未完整展示，请在桌面查看后决定；飞书不提供允许按钮。"
		}
		card.Buttons = append(card.Buttons, im.Button{Label: "拒绝", Value: "decline", Destructive: true})
		card.Text += "\n\n可稍后处理，不会因等待自动拒绝。只作用于本次请求，不设置永久规则或本会话自动允许。"
	} else {
		card.Text += "\n\n" + c.Detail
	}
	return card
}
func (b *Bridge) refreshDesktopApprovalCard(parent context.Context, s store.Session, c store.DesktopApprovalCard) {
	if c.NextAttempt > time.Now().Unix() || parent.Err() != nil {
		return
	}
	lock := b.approvalCardLock(c.ID)
	lock.Lock()
	defer lock.Unlock()
	current, err := b.store.ByThread(s.ThreadID)
	if err != nil || current.Status != "active" || current.Address.Key() != s.Address.Key() {
		return
	}
	adapter := b.channels[s.Address.Connection]
	if adapter == nil || !adapter.Capabilities().InteractiveCards {
		return
	}
	c, err = b.store.DesktopApproval(c.ID)
	if err != nil {
		return
	}
	card := b.desktopApprovalCard(c)
	digest := store.Hash(store.JSON(card))
	if digest == c.Digest {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 7*time.Second)
	defer cancel()
	if c.MessageID != "" {
		if err = adapter.UpdateCard(ctx, s.Address, c.MessageID, card); err != nil {
			_ = b.store.DesktopApprovalCardFailed(c.ID, false)
		} else {
			_ = b.store.DesktopApprovalCardUpdated(c, digest)
		}
		return
	}
	if c.State != "pending" {
		return
	}
	claimed, err := b.store.ClaimDesktopApprovalCard(c.ID)
	if err != nil || !claimed {
		return
	}
	message, sendErr := adapter.SendCard(ctx, s.Address, card, "approval:"+c.ID)
	if sendErr != nil || message == "" {
		var uncertain *im.DeliveryUncertain
		unknown := message == "" && sendErr == nil || errors.As(sendErr, &uncertain) || errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded)
		_ = b.store.DesktopApprovalCardFailed(c.ID, unknown)
		return
	}
	_ = b.store.DesktopApprovalCardSent(c.ID, message, digest)
}
func (b *Bridge) authorizeDesktopApproval(in im.Interaction) (store.Session, store.DesktopApprovalCard, error) {
	if !b.allowed(in.Address.Connection, in.UserID) || (in.Choice != "accept" && in.Choice != "decline") {
		return store.Session{}, store.DesktopApprovalCard{}, diagnostics.UserError("无权处理此审批")
	}
	id := strings.TrimPrefix(in.ActionID, "desktop-approval:")
	if len(id) != 64 {
		return store.Session{}, store.DesktopApprovalCard{}, diagnostics.UserError("审批标识无效")
	}
	card, err := b.store.DesktopApproval(id)
	if err != nil {
		return store.Session{}, card, diagnostics.UserError("审批记录不可用")
	}
	s, err := b.store.ByThread(card.ThreadID)
	if err != nil || s.Status != "active" || card.AddressKey != store.Hash(s.Address.Key()) || s.Address.BaseKey() != in.Address.BaseKey() || in.Address.TopicID != "" && in.Address.TopicID != s.Address.TopicID || card.MessageID == "" || card.MessageID != in.MessageID || card.DeliveryState != "sent" || card.State != "pending" || (card.ExpiresAt != 0 && card.ExpiresAt <= time.Now().Unix()) {
		return s, card, diagnostics.UserError("审批已处理、失效或与当前卡片不匹配")
	}
	if in.UserID != s.Owner && !config.Contains(b.conn(s.Address.Connection).AdminUsers, in.UserID) {
		return s, card, diagnostics.UserError("只有会话创建者或管理员可以审批")
	}
	var request codex.DesktopApproval
	if json.Unmarshal([]byte(card.Payload), &request) != nil || !validDesktopApproval(request) || request.ThreadID != s.ThreadID || request.Fingerprint != card.Fingerprint || desktopApprovalID(request) != card.ID || in.Choice == "accept" && !request.AllowOnce {
		return s, card, diagnostics.UserError("审批内容不完整，请在桌面处理")
	}
	return s, card, nil
}
func (b *Bridge) receiveDesktopApproval(ctx context.Context, in im.Interaction) error {
	s, card, err := b.authorizeDesktopApproval(in)
	if err != nil {
		return err
	}
	if b.controlJobs == nil {
		return diagnostics.UserError("审批处理队列未就绪")
	}
	key := "approval:" + card.ID
	if _, loaded := b.controlScheduled.LoadOrStore(key, true); loaded {
		return diagnostics.UserError("此审批正在处理，请勿重复点击")
	}
	err = b.controlJobs.Push(s.ThreadID, func(parent context.Context) {
		defer b.controlScheduled.Delete(key)
		current, fresh, authErr := b.authorizeDesktopApproval(in)
		if authErr != nil || parent.Err() != nil {
			return
		}
		claimed, e := b.store.ClaimDesktopApproval(fresh.ID, in.Choice, in.MessageID)
		if e != nil || !claimed {
			return
		}
		if r, e := b.store.DesktopApproval(fresh.ID); e == nil {
			b.refreshDesktopApprovalCard(parent, current, r)
		}
		var request codex.DesktopApproval
		_ = json.Unmarshal([]byte(fresh.Payload), &request)
		op, cancel := context.WithTimeout(parent, 9*time.Second)
		result, replyErr := b.codex.ReplyDesktopApproval(op, request, in.Choice)
		cancel()
		state, detail := "uncertain", "本次选择发送结果未确认，请在桌面核对；不会自动重发选择"
		if replyErr == nil {
			switch result {
			case "resolved":
				state = "resolved"
				if in.Choice == "accept" {
					detail = "你的选择：仅允许这一次。桌面已确认该审批结束，执行结果以会话回复为准。"
				} else {
					detail = "你的选择：拒绝。桌面已确认该审批结束。"
				}
			case "stale":
				state, detail = "resolved", "该请求已在桌面处理或内容已变化，本次没有再次授权。"
			case "sent":
				detail = "本次选择已发送到桌面，等待最终确认；不会重复发送。"
			}
		}
		_ = b.store.SetDesktopApprovalState(fresh.ID, state, detail)
		if r, e := b.store.DesktopApproval(fresh.ID); e == nil {
			b.refreshDesktopApprovalCard(parent, current, r)
		}
	})
	if err != nil {
		b.controlScheduled.Delete(key)
		return diagnostics.UserError("审批队列暂满，未授予权限，请稍后处理")
	}
	return nil
}

func (b *Bridge) approvalCardLock(id string) *sync.Mutex {
	value, _ := b.approvalCardLocks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}
