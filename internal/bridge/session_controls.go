package bridge

import (
	"context"
	"database/sql"
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

type modelControlForm struct {
	Models     []codex.ModelChoice `json:"models"`
	Preference store.SessionModel  `json:"preference"`
}

func (b *Bridge) controlLock(thread string) *sync.Mutex {
	v, _ := b.controlLocks.LoadOrStore(thread, &sync.Mutex{})
	return v.(*sync.Mutex)
}
func interruptControlID(thread, event, turn string) string {
	return "interrupt:" + store.Hash(thread+"\x00"+event+"\x00"+turn)
}

func (b *Bridge) controlSession(in im.Interaction, event string) (store.Session, error) {
	if !b.allowed(in.Address.Connection, in.UserID) {
		return store.Session{}, diagnostics.UserError("无权操作此会话")
	}
	s, err := b.store.SessionForControlCard(in.Address, in.MessageID, event)
	if err != nil {
		return s, diagnostics.UserError("卡片与会话不匹配或已失效，请刷新状态")
	}
	if in.Choice != "refresh" && in.Choice != "report" && in.UserID != s.Owner && !config.Contains(b.conn(s.Address.Connection).AdminUsers, in.UserID) {
		return s, diagnostics.UserError("只有会话创建者或管理员可以切换模型和中断")
	}
	return s, nil
}

func (b *Bridge) receiveSessionControl(ctx context.Context, in im.Interaction) error {
	if len(in.ActionID) > 512 || len(in.MessageID) > 256 {
		return diagnostics.UserError("卡片操作标识无效")
	}
	var c store.SessionControl
	var err error
	event := strings.TrimPrefix(in.ActionID, "session:")
	form := strings.HasPrefix(in.ActionID, "session-form:")
	if form {
		c, err = b.store.SessionControl(strings.TrimPrefix(in.ActionID, "session-form:"))
		if err != nil || c.Kind != "model" || c.State != "open" || c.ExpiresAt <= time.Now().Unix() {
			return diagnostics.UserError("模型表单已处理或过期，请重新打开")
		}
		event = c.EventID
	} else if in.Choice != "model" && in.Choice != "interrupt" && in.Choice != "refresh" && in.Choice != "report" {
		return diagnostics.UserError("不支持的会话操作")
	}
	s, err := b.controlSession(in, event)
	if err != nil {
		return err
	}
	if form && (c.ThreadID != s.ThreadID || c.AddressKey != store.Hash(s.Address.Key()) || c.MessageID != in.MessageID) {
		return diagnostics.UserError("表单与原卡不匹配")
	}
	if in.Choice == "report" && !form {
		in.ActionID = "report:" + event
		in.Address = s.Address
		return b.receiveTaskReport(ctx, in)
	}
	if form && in.Choice != "create" && in.Choice != "cancel" {
		return diagnostics.UserError("不支持的表单操作")
	}
	if b.controlJobs == nil {
		return diagnostics.UserError("会话控制队列尚未就绪")
	}
	// 同一操作在排队期间去重；中断另外持久化，以防重启后重放。
	key := in.ActionID + ":" + in.Choice
	if _, exists := b.controlScheduled.LoadOrStore(key, true); exists {
		return nil
	}
	err = b.controlJobs.Push(s.ThreadID, func(parent context.Context) {
		defer b.controlScheduled.Delete(key)
		lock := b.controlLock(s.ThreadID)
		lock.Lock()
		defer lock.Unlock()
		work, cancel := context.WithTimeout(parent, 25*time.Second)
		defer cancel()
		current, checkErr := b.controlSession(in, event)
		if checkErr != nil || work.Err() != nil {
			return
		}
		var operationErr error
		if form {
			if in.Choice == "cancel" {
				operationErr = b.store.FinishSessionControl(c.ID, "cancelled", "")
			} else {
				operationErr = b.saveModelForm(current, c, in.Fields)
			}
		} else {
			switch in.Choice {
			case "model":
				operationErr = b.openModelControl(work, current, event, in.MessageID)
			case "interrupt":
				operationErr = b.interruptCardTurn(work, current, event, in.MessageID)
			case "refresh": // 只刷新卡片，不重放模型任务。
			}
		}
		if operationErr != nil {
			// 错误保留在原卡片；不依赖一次性文本提醒是否送达。
			_ = b.store.SetMeta("session-control-notice:"+store.Hash(current.ThreadID+"\x00"+event), b.presentationFilter().Text(diagnostics.Error(operationErr)))
		} else if in.Choice != "refresh" {
			_ = b.store.SetMeta("session-control-notice:"+store.Hash(current.ThreadID+"\x00"+event), "")
		}
		_ = b.enqueueSessionStatus(current.ThreadID, true, event)
	})
	if err != nil {
		b.controlScheduled.Delete(key)
		return diagnostics.UserError("控制队列暂满或后台停止，未执行操作，请稍后再试")
	}
	return nil
}

func (b *Bridge) openModelControl(ctx context.Context, s store.Session, event, message string) error {
	if _, err := b.store.OpenModelControl(s.ThreadID, event); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	models, err := b.codex.Models(ctx)
	if err != nil {
		return diagnostics.UserError("暂时无法读取可用模型，未更改设置")
	}
	pref, err := b.store.SessionModel(s)
	if err != nil {
		return err
	}
	c := store.SessionControl{ID: store.ID(), ThreadID: s.ThreadID, EventID: event, AddressKey: store.Hash(s.Address.Key()), MessageID: message, Kind: "model", Payload: store.JSON(modelControlForm{models, pref}), ExpiresAt: time.Now().Add(15 * time.Minute).Unix()}
	ok, err := b.store.PutSessionControl(c)
	if err != nil {
		return err
	}
	if !ok {
		return diagnostics.UserError("原卡片身份已变化，请刷新后再选择")
	}
	return nil
}
func (b *Bridge) saveModelForm(s store.Session, c store.SessionControl, fields map[string]string) error {
	var form modelControlForm
	if json.Unmarshal([]byte(c.Payload), &form) != nil {
		return diagnostics.UserError("模型表单记录无效")
	}
	if len(fields) != 2 {
		return diagnostics.UserError("请选择模型和思考程度")
	}
	selected, effort := fields["model"], fields["effort"]
	valid := false
	for _, m := range form.Models {
		if m.Model == selected {
			for _, e := range m.Efforts {
				if e == effort {
					valid = true
				}
			}
		}
	}
	if !valid {
		return diagnostics.UserError("模型与思考程度不匹配，请选择该模型支持的组合")
	}
	return b.store.SaveSessionModel(s, c, store.SessionModel{Model: selected, Effort: effort, Revision: form.Preference.Revision})
}
func modelFormCard(c store.SessionControl, card *im.Card) error {
	var form modelControlForm
	if json.Unmarshal([]byte(c.Payload), &form) != nil || len(form.Models) == 0 {
		return errors.New("模型表单记录无效")
	}
	models := []im.Button{}
	efforts := []im.Button{}
	seen := map[string]bool{}
	for _, m := range form.Models {
		label := m.Name
		if label == "" {
			label = m.Model
		}
		models = append(models, im.Button{Label: label, Value: m.Model})
		for _, e := range m.Efforts {
			if !seen[e] {
				seen[e] = true
				efforts = append(efforts, im.Button{Label: e, Value: e})
			}
		}
	}
	model, effort := form.Preference.Model, form.Preference.Effort
	validDefault := false
	for _, m := range form.Models {
		if m.Model == model {
			for _, e := range m.Efforts {
				if e == effort {
					validDefault = true
				}
			}
		}
	}
	if !validDefault {
		model, effort = form.Models[0].Model, form.Models[0].DefaultEffort
		if effort == "" && len(form.Models[0].Efforts) > 0 {
			effort = form.Models[0].Efforts[0]
		}
	}

	card.ActionID = "session-form:" + c.ID
	card.Buttons = nil
	card.Text += "\n\n选择下一轮模型：保存不打断当前轮次，不自动重新发送问题。仅作用于此会话之后从飞书提交的请求。"
	card.Form = &im.Form{SubmitLabel: "保存下一轮模型", Fields: []im.FormField{
		{Name: "model", Label: "模型", Required: true, Default: model, Options: models},
		{Name: "effort", Label: "思考程度（需与模型匹配）", Required: true, Default: effort, Options: efforts},
	}}
	return nil
}

func (b *Bridge) interruptCardTurn(ctx context.Context, s store.Session, event, message string) error {
	view, err := b.store.ReadSessionRequestStatus(s.ThreadID, event)
	if err != nil || view.Telemetry == nil {
		return diagnostics.UserError("尚未确认本卡片的实际轮次，未发送中断")
	}
	snapshot := view.Telemetry.Snapshot
	if snapshot.TurnID == "" || snapshot.Issue == "identity_conflict" || snapshot.EndedAt > 0 || snapshot.Outcome != "" {
		return diagnostics.UserError("本轮已结束或身份尚未确认，请刷新状态")
	}
	// 先核对真实轮次，再持久领取；未知结果只回读，不再发送一次中断。
	turn, err := b.codex.ReadControlTurn(ctx, s.ThreadID, snapshot.TurnID)
	if err != nil {
		return err
	}
	if turn.Status != "inProgress" {
		return diagnostics.UserError("该轮次已经结束，没有发送中断")
	}
	c := store.SessionControl{ID: interruptControlID(s.ThreadID, event, snapshot.TurnID), ThreadID: s.ThreadID, EventID: event, AddressKey: store.Hash(s.Address.Key()), MessageID: message, Kind: "interrupt", Payload: snapshot.TurnID, ExpiresAt: time.Now().Add(time.Minute).Unix()}
	_, err = b.store.PutSessionControl(c)
	if err != nil {
		return err
	}
	claimed, err := b.store.ClaimSessionControl(c.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return diagnostics.UserError("本轮已有中断请求，请刷新核对；不会重复发送")
	}
	_ = b.enqueueSessionStatus(s.ThreadID, true, event)
	ended, err := b.codex.InterruptExactTurn(ctx, s.ThreadID, snapshot.TurnID)
	state, detail := "requested", "中断请求已发送，等待确认轮次结束"
	if err != nil {
		state, detail = "unknown", "中断结果未确认，请刷新核对，未自动重发"
	} else if ended {
		state, detail = "done", "已确认目标轮次结束；已执行操作不会回滚"
	}
	if saveErr := b.store.FinishSessionControl(c.ID, state, detail); saveErr != nil {
		return saveErr
	}
	return err
}

// 原卡上保留报告入口；控制按钮和表单均绑定同一平台回执。
func (b *Bridge) addSessionControls(view store.SessionStatus, card *im.Card) {
	s, event := view.Session, view.Task.ID
	if event == "" {
		return
	}
	card.ActionID = "session:" + event
	if pref, err := b.store.SessionModel(s); err == nil && pref.Model != "" {
		card.Text += "\n下一轮模型：" + pref.Model + " · " + pref.Effort + "（已保存）"
	}
	if view.Telemetry != nil {
		p := view.Telemetry.Snapshot
		if p.Model != "" {
			card.Text += "\n本轮模型：" + p.Model
		}
		switch p.Outcome {
		case "failed":
			card.Text += "\n本轮响应失败，可能已有部分操作完成；未自动重跑。"
		case "interrupted":
			card.Text += "\n本轮已中断，已执行操作不会回滚。"
		}
		if p.Preview != "" && p.Issue != "identity_conflict" {
			label := "临时输出 · 尚未完成"
			if p.EndedAt > 0 || p.Outcome != "" {
				label = "最近公开输出 · 轮次已结束"
			}
			card.Text += fmt.Sprintf("\n\n%s（%s）\n%s", label, telemetryTime(p.PreviewAt), p.Preview)
		} else {
			card.Text += "\n\n临时输出：等待可关联的公开文字。"
		}
		if p.TurnID != "" && p.Issue != "identity_conflict" && p.EndedAt == 0 && p.Outcome == "" {
			if action, err := b.store.SessionControl(interruptControlID(s.ThreadID, event, p.TurnID)); errors.Is(err, sql.ErrNoRows) {
				card.Buttons = append(card.Buttons, im.Button{Label: "中断当前轮次", Value: "interrupt", Destructive: true})
			} else if err == nil {
				detail := action.Detail
				if action.State == "running" {
					detail = "正在请求中断"
				}
				card.Text += "\n" + detail
			}
		}
	}
	card.Buttons = append(card.Buttons, im.Button{Label: "切换模型", Value: "model"}, im.Button{Label: "刷新状态", Value: "refresh"})
	if note, err := b.store.GetMeta("session-control-notice:" + store.Hash(s.ThreadID+"\x00"+event)); err == nil && note != "" {
		card.Text += "\n操作提示：" + note
	}
	if form, err := b.store.OpenModelControl(s.ThreadID, event); err == nil {
		_ = modelFormCard(form, card)
	}
}
