package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

type replyAssessment struct {
	Assessment string `json:"assessment"`
	Reason     string `json:"reason"`
	Reply      string `json:"reply"`
}

func parseReplyAssessment(raw string) replyAssessment {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```json\n") && strings.HasSuffix(text, "\n```") {
		text = strings.TrimSuffix(strings.TrimPrefix(text, "```json\n"), "\n```")
	}
	var r replyAssessment
	if json.Unmarshal([]byte(text), &r) != nil || r.Reason == "" || (r.Assessment != "ready" && r.Assessment != "needs_info" && r.Assessment != "manual") || len(r.Reply) > 24000 {
		if len(raw) > 24000 {
			raw = ""
		}
		return replyAssessment{Assessment: "manual", Reason: "模型没有返回可确认的处理判断，请人工检查；不会自动发送。", Reply: raw}
	}
	r.Reply = strings.TrimSpace(r.Reply)
	if r.Assessment == "ready" && r.Reply == "" {
		r.Assessment = "manual"
		r.Reason = "模型判断可以处理，但没有生成回复草稿。"
	}
	return r
}

// 调用方持有 liveLock。默认仅落待审记录，绝不把模型文本直接当作发送授权。
func (b *Bridge) preparePersonalReviews(ctx context.Context, s store.Session) error {
	if err := b.store.HoldDisabledAutomatic(s.ThreadID, b.personalAutoPeers(s)); err != nil {
		return err
	}
	candidates, err := b.store.PersonalReviewCandidates(s.ThreadID)
	if err != nil {
		return err
	}
	for event, candidate := range candidates {
		autoAfter := int64(0)
		if config.Contains(b.personalAutoPeers(s), s.Owner) {
			autoAfter = b.personalStartedAt
		}
		var r replyAssessment
		if candidate.Natural {
			r = b.reviewNaturalDraft(ctx, s, candidate, autoAfter > 0)
		} else {
			r = parseReplyAssessment(candidate.Body)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = b.store.PutPersonalReview(s.ThreadID, event, r.Assessment, r.Reason, r.Reply, autoAfter); err != nil {
			return err
		}
	}
	rows, err := b.store.PendingPersonalReviews(s.ThreadID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.AutoEligible && r.Assessment == "ready" && config.Contains(b.personalAutoPeers(s), r.Peer) {
			parts := b.personalParts(r.Draft)
			_ = b.store.DecidePersonalReview(s, store.PersonalReviewAction{EventID: r.EventID, Version: r.Version, Action: "send", Text: r.Draft}, parts, true)
		}
		if notify, err := b.store.ClaimPersonalNotice(r.EventID); err == nil && notify {
			b.notifyPersonalReview(ctx)
		}
	}
	return nil
}

// 已启用自动回复的联系人仍需后台判断草稿是否可以直接发送。
func (b *Bridge) reviewNaturalDraft(ctx context.Context, s store.Session, c store.ReviewCandidate, assess bool) replyAssessment {
	draft := strings.TrimSpace(c.Body)
	if draft == "" || draft == "\x00" || len(draft) > 24000 {
		return replyAssessment{Assessment: "manual", Reason: "请在原会话核对完整输出并编辑回复。"}
	}
	r := replyAssessment{Assessment: "draft", Reason: "请核对回复草稿。", Reply: draft}
	if !assess {
		return r
	}
	cfg := b.configForConnection(s.Address.Connection)
	cfg.Codex.AssessDraft = true
	input, _ := json.Marshal(map[string]string{"request": c.Request, "draft": draft})
	data, err := codex.ClassifyPersonal(ctx, cfg, string(input))
	var decision replyAssessment
	if err == nil && json.Unmarshal([]byte(data), &decision) == nil && strings.TrimSpace(decision.Reason) != "" {
		switch decision.Assessment {
		case "ready", "manual", "needs_info":
			r.Assessment = decision.Assessment
			r.Reason = decision.Reason
		}
	}
	return r
}
func (b *Bridge) personalParts(text string) []string {
	max := b.cfg.Limits.MaxMessageChars - 80
	if max < 120 {
		max = 120
	}
	chars := []rune(strings.TrimSpace(text))
	var parts []string
	for start := 0; start < len(chars); start += max {
		end := start + max
		if end > len(chars) {
			end = len(chars)
		}
		parts = append(parts, string(chars[start:end]))
	}
	return parts
}
func (b *Bridge) notifyPersonalReview(parent context.Context) {
	if runtime.GOOS != "darwin" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	// 沿用本机巡检通知的参数化调用方式；锁屏通知不包含联系人和消息正文。
	_ = exec.CommandContext(ctx, "/usr/bin/osascript", "-e", notifyScript, "个人消息处理结果已更新，请打开 IM Agent 消息助理查看。默认等待你审核后再回复。", "IM Agent 消息审核").Run()
}

const notifyScript = "on run argv\ndisplay notification (item 1 of argv) with title (item 2 of argv)\nend run"

func notifyLocal(parent context.Context, title, text string) {
	if runtime.GOOS != "darwin" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "/usr/bin/osascript", "-e", notifyScript, text, title).Run()
}

// 只由本机控制 API 调用。将来飞书审核先认证操作者，再复用本方法；不开放给联系人。
func (b *Bridge) ReviewPersonalReply(ctx context.Context, r store.PersonalReviewAction) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	old, err := b.store.PersonalReview(r.EventID)
	if err != nil {
		return errors.New("待审记录不存在")
	}
	lock := b.liveLock(old.ThreadID)
	lock.Lock()
	defer lock.Unlock()
	s, err := b.store.ByThread(old.ThreadID)
	if err != nil {
		return err
	}
	a := b.channels[s.Address.Connection]
	localClose := r.Action == "manual" || r.Action == "ignored"
	if !localClose && (a == nil || !a.Capabilities().PersonalInbox || !b.messageAllowed(a, im.Message{Address: s.Address, UserID: old.Peer, IsDirect: personalDirect(s.Address)})) {
		return errors.New("原个人账号连接或联系人授权已失效")
	}
	if err = b.store.DecidePersonalReview(s, r, b.personalParts(r.Text), false); err != nil {
		return err
	}
	// 由原投递扫描领取已批准片段；API 超时不会重复提交发送。
	return nil
}

// 群@只人工审核；联系人自动回复规则仅适用于单聊。
func (b *Bridge) personalAutoPeers(s store.Session) []string {
	if !personalDirect(s.Address) {
		return nil
	}
	return b.conn(s.Address.Connection).PersonalAutoReplyUsers
}

func personalDirect(a im.Address) bool {
	wire, err := a.DeliveryAddress()
	return err == nil && !strings.Contains(wire.ChatID, ":group:")
}
