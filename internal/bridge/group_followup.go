package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) syncGroupFollowups(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		plans, err := b.store.ActiveGroupFollowups("")
		if err == nil {
			for id, a := range b.channels {
				if follow, ok := a.(im.GroupFollowupAdapter); ok {
					var groups []im.GroupFollowup
					for _, t := range plans {
						if t.Connection == id && b.conn(id).PersonalGroupFollowup {
							groups = append(groups, im.GroupFollowup{Address: im.Address{Channel: "dingtalk_personal", Connection: id, ChatID: t.Chat}, Until: t.Until})
						}
					}
					follow.SetGroupFollowups(groups)
				}
			}
		}
		pending, err := b.store.PendingGroupFollowInputs()
		if err == nil {
			for _, input := range pending {
				input := input
				if _, busy := b.followScheduled.LoadOrStore(input.ID, true); busy {
					continue
				}
				err := b.followJobs.Push(input.ID, func(job context.Context) { defer b.followScheduled.Delete(input.ID); b.handleGroupFollowup(job, input) })
				if err != nil {
					b.followScheduled.Delete(input.ID)
					break
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
func (b *Bridge) handleGroupFollowup(ctx context.Context, input store.GroupFollowInput) {
	current, err := b.store.GroupFollowInput(input.ID)
	if err != nil {
		return
	}
	input = current
	if input.State != "queued" && input.State != "matched" && input.State != "accepted" {
		return
	}
	if done, target, e := b.store.GroupFollowupReceived(input.ID); e != nil {
		return
	} else if done {
		kind := "duplicate"
		if target != "" && target == input.Target {
			kind = "routed"
		} else {
			target = ""
		}
		_ = b.store.FinishGroupFollowup(input.ID, target, kind)
		return
	}
	cc := b.conn(input.Message.Address.Connection)
	if !cc.PersonalGroupFollowup || !cc.PersonalMentions {
		_ = b.store.FinishGroupFollowup(input.ID, "", "ignored")
		return
	}
	if input.State == "matched" || input.State == "accepted" {
		if err = b.routeGroupFollowup(ctx, input); err != nil {
			b.log.Warn("群补充关联暂未完成，保留待重试")
		}
		return
	}
	tasks, err := b.store.ActiveGroupFollowups(input.WireKey)
	if err != nil {
		return
	}
	if len(tasks) == 0 {
		_ = b.store.FinishGroupFollowup(input.ID, "", "expired")
		return
	}
	// 不可确认的关联一律进入本机候选，不污染业务 thread，也不自动回复。
	target, reason, related := "", "暂时无法确认归属，请在本机选择相关任务。", true
	if len(tasks) <= 8 && len(input.Message.Text) <= b.cfg.Limits.MaxInputChars {
		type taskSummary struct {
			ID      string `json:"id"`
			Summary string `json:"summary"`
		}
		summaries := []taskSummary{}
		for _, t := range tasks {
			summaries = append(summaries, taskSummary{t.ID, t.Summary})
		}
		payload, _ := json.Marshal(map[string]any{"tasks": summaries, "message": input.Message.Text, "self_message": input.Message.SelfMessage})
		cfg := b.configForConnection(input.Message.Address.Connection)
		cfg.WorkDir = ""
		answer, e := codex.ClassifyPersonal(ctx, cfg, string(payload))
		var choice struct {
			Related *bool  `json:"related"`
			Target  string `json:"target_event_id"`
			Reason  string `json:"reason"`
		}
		if e == nil && json.Unmarshal([]byte(strings.TrimSpace(answer)), &choice) == nil && choice.Related != nil && len(choice.Reason) <= 1500 {
			if !*choice.Related {
				related = false
				reason = "与当前跟进任务无关，未加入业务会话。"
			} else {
				for _, t := range tasks {
					if t.ID == choice.Target {
						target = choice.Target
						reason = choice.Reason
						break
					}
				}
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	if done, _, e := b.store.GroupFollowupReceived(input.ID); e != nil {
		return
	} else if done {
		_ = b.store.FinishGroupFollowup(input.ID, "", "duplicate")
		return
	}
	if b.store.ClassifyGroupFollowup(input.ID, target, reason, related) != nil {
		return
	}
	if related {
		if notify, err := b.store.ClaimGroupFollowNotice(input.ID); err == nil && notify {
			b.notifyPersonalReview(ctx)
		}
	}
}
func (b *Bridge) routeGroupFollowup(ctx context.Context, input store.GroupFollowInput) error {
	task, err := b.store.GroupFollowTask(input.Target)
	if err != nil {
		_ = b.store.FinishGroupFollowup(input.ID, "", "expired")
		return nil
	}
	if task.WireKey != input.WireKey {
		return errors.New("群补充与目标任务不在同一会话")
	}
	s, err := b.store.ByThread(task.ThreadID)
	if err != nil {
		return err
	}
	if s.Status != "active" {
		_ = b.store.FinishGroupFollowup(input.ID, "", "expired")
		return nil
	}
	if input.Message.SelfMessage {
		// 本人消息只登记引用关系，绝不再次提交给 Codex 让它回复自己。
		if err = b.store.FinishGroupFollowup(input.ID, task.ID, "self_recorded"); err != nil {
			return err
		}
		return nil
	}
	msg := input.Message
	msg.Address = s.Address
	msg.FollowupTarget = task.ID
	if err = b.receiveMessage(ctx, msg); err != nil {
		return err
	}
	return b.store.FinishGroupFollowup(input.ID, task.ID, "routed")
}

func (b *Bridge) ReviewGroupFollowup(ctx context.Context, a store.GroupFollowAction) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if a.Action == "open" {
		s, msg, err := b.store.GroupFollowSource(a.Target)
		if err != nil {
			return err
		}
		cc := b.conn(s.Address.Connection)
		if s.Status != "active" || msg.IsDirect || !cc.PersonalGroupFollowup || !cc.PersonalMentions || !config.IsPersonalChannel(cc.Type) {
			return errors.New("此任务不支持开启群跟进")
		}
		if review, e := b.store.PersonalReview(msg.ID); e == nil && review.State == "ignored" {
			return errors.New("该任务已忽略，请以新@消息开启新的跟进")
		}
		return b.store.OpenGroupFollowup(s, msg, cc.PersonalFollowupMinutes, true)
	}
	if a.Action == "close" {
		t, err := b.store.GroupFollowTask(a.Target)
		if err != nil {
			return err
		}
		if !config.IsPersonalChannel(b.conn(t.Connection).Type) {
			return errors.New("群跟进连接已失效")
		}
		return b.store.CloseGroupFollowup(a.Target)
	}
	input, err := b.store.GroupFollowInput(a.ID)
	if err != nil {
		return err
	}
	cc := b.conn(input.Message.Address.Connection)
	if !cc.PersonalGroupFollowup {
		return errors.New("群跟进功能已关闭")
	}
	return b.store.DecideGroupFollowup(a)
}
