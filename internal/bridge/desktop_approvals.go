package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	return (a.Kind == "" || a.Kind == "command" || desktopUserInput(a) && validDesktopQuestions(a.Questions) && (a.Kind != "async_input" || a.EntityKey != "")) && a.ThreadID != "" && a.TurnID != "" && a.ItemID != "" && len(a.Fingerprint) == 64 && len(a.RequestID) > 0 && len(a.RequestID) <= 260 && json.Valid(a.RequestID)
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
		if err != nil {
			b.approvalSyncDiagnostic("读取会话绑定失败，尚未核对桌面审批。")
		}
		if err == nil {
			allowed := map[string]store.Session{}
			threads := []string{}
			for _, s := range sessions {
				if s.Status == "active" && s.Managed && s.Address.Channel != "dingtalk" {
					allowed[s.ThreadID] = s
					threads = append(threads, s.ThreadID)
				}
			}
			op, cancel := context.WithTimeout(ctx, 9*time.Second)
			snapshot, readErr := b.codex.ReadDesktopApprovals(op, threads)
			cancel()
			b.diagMu.Lock()
			b.diag.DesktopAdapter = snapshot.Adapter
			b.diag.DesktopCapabilities = snapshot.Capabilities
			b.diag.ApprovalPhase = "running"
			if readErr != nil || snapshot.State != "snapshot" || !snapshot.Complete {
				b.diag.ApprovalPhase = "degraded"
			}
			b.diagMu.Unlock()
			if readErr != nil {
				b.approvalSyncDiagnostic("桌面审批目录读取失败：" + readErr.Error())
				if time.Since(warned) > time.Minute {
					b.log.Warn("桌面交互目录暂不可用，未自动回答、批准或猜测请求状态")
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
					for i := range request.Questions {
						q := &request.Questions[i]
						q.Header = b.presentationFilter().Text(q.Header)
						q.Question = b.presentationFilter().Text(q.Question)
						for n := range q.Options {
							q.Options[n].Label = b.presentationFilter().Text(q.Options[n].Label)
							q.Options[n].Description = b.presentationFilter().Text(q.Options[n].Description)
						}
					}
					_ = b.store.RegisterDesktopApproval(store.DesktopApprovalCard{ID: id, ThreadID: session.ThreadID, AddressKey: store.Hash(session.Address.Key()), Fingerprint: request.Fingerprint, Payload: store.JSON(request), ExpiresAt: 0})
				}
				sent, pending, unknown, responseUnknown := 0, 0, 0, 0
				cards, listErr := b.store.DesktopApprovalCards()
				if listErr == nil {
					for _, card := range cards {
						session, ok := allowed[card.ThreadID]
						if !ok || store.Hash(session.Address.Key()) != card.AddressKey {
							continue
						}
						var original codex.DesktopApproval
						_ = json.Unmarshal([]byte(card.Payload), &original)
						kind := original.Kind
						if kind == "" {
							kind = "command"
						}
						covered := false
						for _, k := range snapshot.CoveredKinds {
							if k == kind {
								covered = true
							}
						}
						if complete && covered && !seen[card.ID] {
							_ = b.store.SetDesktopApprovalState(card.ID, "resolved", "该请求已不再待处理，可能已在桌面处理；此卡不可再次提交")
						}
						fresh, e := b.store.DesktopApproval(card.ID)
						if e == nil {
							b.refreshDesktopApprovalCard(ctx, session, fresh)
							if latest, lookupErr := b.store.DesktopApproval(card.ID); lookupErr == nil && (latest.State == "pending" || latest.State == "responding" || latest.State == "uncertain") {
								switch latest.DeliveryState {
								case "sent":
									sent++
								case "unknown":
									unknown++
								default:
									pending++
								}
								if latest.State == "uncertain" {
									responseUnknown++
								}
							}
						}
					}
				}
				if listErr != nil {
					b.approvalSyncDiagnostic("桌面目录可读，但读取本地审批投递记录失败。")
				} else if complete && len(snapshot.Requests) == 0 && sent+pending+unknown+responseUnknown == 0 {
					b.approvalSyncDiagnostic("桌面审批目录读取正常，当前没有待批准请求。")
				} else {
					message := fmt.Sprintf("待处理请求 %d；卡片已送达 %d，待投递 %d，送达待核对 %d；选择回传待核对 %d。", len(snapshot.Requests), sent, pending, unknown, responseUnknown)
					if !complete {
						message = "目录仅部分可核验；" + message
					}
					b.approvalSyncDiagnostic(message)
				}
			} else {
				b.approvalSyncDiagnostic("桌面审批目录未返回有效快照，尚未确认待批准请求。")
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
	if desktopUserInput(request) {
		return b.desktopInputCard(c, request)
	}
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
	if !b.allowed(in.Address.Connection, in.UserID) {
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
	if desktopUserInput(request) {
		if _, err := desktopInputAnswers(request, in); err != nil {
			return s, card, diagnostics.UserError(err.Error())
		}
	} else if in.Choice != "accept" && in.Choice != "decline" {
		return s, card, diagnostics.UserError("不支持的审批选项")
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
		var request codex.DesktopApproval
		_ = json.Unmarshal([]byte(fresh.Payload), &request)
		var answers map[string]codex.DesktopInputAnswer
		if desktopUserInput(request) {
			answers, authErr = desktopInputAnswers(request, in)
			if authErr != nil {
				return
			}
			// 只登记已提交这一事实，自定义回答不写入审批决策账本。
		}
		claimed, e := b.store.ClaimDesktopApproval(fresh.ID, in.Choice, in.MessageID)
		if e != nil || !claimed {
			return
		}
		if r, e := b.store.DesktopApproval(fresh.ID); e == nil {
			b.refreshDesktopApprovalCard(parent, current, r)
		}
		op, cancel := context.WithTimeout(parent, 9*time.Second)
		var result string
		var replyErr error
		if desktopUserInput(request) {
			result, replyErr = b.codex.ReplyDesktopUserInput(op, request, answers)
		} else {
			result, replyErr = b.codex.ReplyDesktopApproval(op, request, in.Choice)
		}
		cancel()
		state, detail := "uncertain", "本次选择发送结果未确认，请在桌面核对；不会自动重发选择"
		if replyErr == nil {
			switch result {
			case "resolved":
				state = "resolved"
				if desktopUserInput(request) {
					detail = "回答已发送，桌面已确认该提问结束。"
					if in.Choice == "skip" {
						detail = "已跳过这组问题，桌面已确认该提问结束。"
					}
				} else if in.Choice == "accept" {
					detail = "你的选择：仅允许这一次。桌面已确认该审批结束，执行结果以会话回复为准。"
				} else {
					detail = "你的选择：拒绝。桌面已确认该审批结束。"
				}
			case "stale":
				state, detail = "resolved", "该请求已在桌面处理或内容已变化，本次没有再次提交。"
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
