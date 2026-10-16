package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"im-agent/internal/codex"
	"im-agent/internal/config"
	"im-agent/internal/diagnostics"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func (b *Bridge) openCreationForm(ctx context.Context, adapter im.Adapter, msg im.Message) error {
	// 表单会展示本机项目目录，仅管理员可发起，回调还要核对原操作者。
	if !config.Contains(b.conn(msg.Address.Connection).AdminUsers, msg.UserID) {
		return diagnostics.UserError("只有该连接的管理员可以打开本机项目创建表单")
	}
	if !adapter.Capabilities().InteractiveCards {
		return diagnostics.UserError("该 IM 不支持创建表单")
	}
	op, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	catalog, e := b.codex.CreationCatalog(op)
	if e != nil {
		return diagnostics.UserError("读取本机 Codex 项目或模型目录失败；未创建群和会话，请稍后重试")
	}
	projectOptions := []im.Button{}
	for _, p := range catalog.Projects {
		label := p.Name
		if p.Path != "" {
			label += " — " + p.Path
		}
		projectOptions = append(projectOptions, im.Button{Label: label, Value: p.Value})
	}
	models := []im.Button{}
	efforts := []im.Button{}
	seen := map[string]bool{}
	for _, m := range catalog.Models {
		label := m.Name
		if label == "" {
			label = m.Model
		}
		models = append(models, im.Button{Label: label, Value: m.Model})
		for _, eff := range m.Efforts {
			if !seen[eff] {
				efforts = append(efforts, im.Button{Label: eff, Value: eff})
				seen[eff] = true
			}
		}
	}
	first := catalog.Models[0]
	effort := first.DefaultEffort
	if !config.Contains(first.Efforts, effort) {
		effort = first.Efforts[0]
	}
	f := store.CreationForm{ID: "new:" + store.ID(), Address: msg.Address, UserID: msg.UserID, Catalog: store.JSON(catalog), ExpiresAt: 0}
	if e = b.store.PutCreationForm(f); e != nil {
		return e
	}
	card := im.Card{Title: "新建 Codex 会话", ActionID: f.ID, Text: "项目与模型来自本机 Codex 目录。可在项目下拉框中搜索；多个根目录分别列出。选项只作用于新会话，不修改全局配置。表单不设等待期限，可稍后填写。填写首问后只需提交一次，后台创建独立群并自动尝试执行；结果将显示在新群。", Form: &im.Form{SubmitLabel: "创建并开始", Fields: []im.FormField{
		{Name: "project", Label: "项目 / 工作目录", Placeholder: "搜索并选择项目", Required: true, Default: "automatic", Options: projectOptions},
		{Name: "model", Label: "模型", Placeholder: "选择模型", Required: true, Default: first.Model, Options: models},
		{Name: "effort", Label: "思考程度（必须受所选模型支持）", Placeholder: "选择思考程度", Required: true, Default: effort, Options: efforts},
		{Name: "prompt", Label: "首条问题", Placeholder: "填写需求，创建后直接执行，不必到新群再发一遍", Required: true, Multiline: true, MaxLength: 1000},
	}}}
	id, e := adapter.SendCard(op, msg.Address, card, f.ID)
	if e != nil {
		_ = b.store.CreationProgress(f.ID, "send_failed", "form_delivery", "", diagnostics.Error(e))
		return e
	}
	if id == "" {
		_ = b.store.CreationProgress(f.ID, "unknown", "form_delivery", "", "未返回卡片 ID")
		return errors.New("未确认表单发送")
	}
	return b.store.CreationMessage(f.ID, id)
}
func validateCreationInteraction(f store.CreationForm, in im.Interaction) error {
	if f.State != "pending" || (f.ExpiresAt != 0 && f.ExpiresAt <= time.Now().Unix()) || f.UserID != in.UserID || f.MessageID == "" || f.MessageID != in.MessageID || f.Address.BaseKey() != in.Address.BaseKey() || (in.Address.TopicID != "" && f.Address.TopicID != in.Address.TopicID) {
		return diagnostics.UserError("表单已提交、失效或不属于当前用户，请重新发送 /new")
	}
	return nil
}
func (b *Bridge) receiveCreationForm(ctx context.Context, in im.Interaction) error {
	f, e := b.store.CreationForm(in.ActionID)
	if e != nil {
		return diagnostics.UserError("创建表单不存在")
	}
	if e = validateCreationInteraction(f, in); e != nil {
		return e
	}
	adapter := b.channels[f.Address.Connection]
	if adapter == nil {
		return errors.New("连接不存在")
	}
	if in.Choice == "cancel" {
		ok, e := b.store.CancelCreation(f.ID)
		if e != nil {
			return e
		}
		if !ok {
			return diagnostics.UserError("表单已经处理")
		}
		// 回调只排队更新，不直接等待网络。
		return b.jobs.Push("create:"+f.ID, func(job context.Context) {
			_ = adapter.UpdateCard(job, f.Address, f.MessageID, im.Card{Title: "已取消创建", Text: "没有创建会话或执行首问。"})
		})
	}
	if in.Choice != "create" {
		return diagnostics.UserError("不支持的表单操作")
	}
	for key := range in.Fields {
		if key != "project" && key != "model" && key != "effort" && key != "prompt" {
			return diagnostics.UserError("表单包含未知字段")
		}
	}
	selected := codex.CreationSelection{Project: in.Fields["project"], Model: in.Fields["model"], Effort: in.Fields["effort"], Prompt: strings.TrimSpace(in.Fields["prompt"])}
	if selected.Prompt == "" || len([]rune(selected.Prompt)) > 1000 || len([]rune(selected.Prompt)) > b.cfg.Limits.MaxInputChars {
		return diagnostics.UserError("请填写 1–1000 字以内的首条问题")
	}
	var catalog codex.CreationCatalog
	if json.Unmarshal([]byte(f.Catalog), &catalog) != nil {
		return errors.New("目录快照无效")
	}
	project, e := catalog.Validate(selected)
	if e != nil {
		return diagnostics.UserError(e.Error())
	}
	claimed, e := b.store.ClaimCreation(f.ID, store.JSON(selected))
	if e != nil {
		return e
	}
	if !claimed {
		return diagnostics.UserError("表单已提交，请勿重复点击")
	}
	e = b.jobs.Push("create:"+f.ID, func(job context.Context) { b.executeCreationForm(job, adapter, f, project, selected) })
	if e != nil {
		_ = b.store.CreationProgress(f.ID, "queue_failed", "queue", "", "队列容量不足，没有创建资源")
		return diagnostics.UserError("当前队列已满，未创建资源；请稍后重新 /new")
	}
	return nil
}
func (b *Bridge) executeCreationForm(parent context.Context, adapter im.Adapter, f store.CreationForm, p codex.ProjectChoice, selected codex.CreationSelection) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(b.cfg.Limits.TurnTimeoutSeconds)*time.Second)
	defer cancel()
	update := func(title, text string) {
		noticeCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer stop()
		if err := adapter.UpdateCard(noticeCtx, f.Address, f.MessageID, im.Card{Title: title, Text: text}); err != nil {
			b.log.Warn("创建阶段卡片更新未确认", "form", f.ID, "error", diagnostics.Error(err))
		}
	}
	fail := func(stage, thread string, e error) {
		hint := diagnostics.Error(e)
		state := "failed"
		var unconfirmed *codex.DesktopCreationUnconfirmed
		if errors.As(e, &unconfirmed) {
			state = "uncertain"
			hint = unconfirmed.Error()
		}
		_ = b.store.CreationProgress(f.ID, state, stage, thread, hint)
		update("创建或首问未完成", "阶段："+stage+"\n原因："+hint+"\n会话 ID："+thread+"\n不会自动重复建群或重放首问；请查看客户端任务记录。")
	}
	update("正在创建会话", "正在应用项目、模型与思考程度，请勿重复提交。")
	// 与自动发现共用锁，避免新 thread 同时被另一个流程建群。
	b.bindMu.Lock()
	if e := b.store.CreationProgress(f.ID, "claimed", "creating_thread", "", ""); e != nil {
		b.bindMu.Unlock()
		fail("记录创建意图", "", e)
		return
	}
	thread, e := b.codex.StartConfiguredThread(ctx, b.cfg, p, selected)
	if thread.ID != "" {
		_ = b.store.CreationProgress(f.ID, "claimed", "creating_group", thread.ID, "")
	}
	if e != nil {
		b.bindMu.Unlock()
		fail("创建并校验会话参数", thread.ID, e)
		return
	}
	origin := im.Message{ID: f.ID, UserID: f.UserID, Address: f.Address, IsDirect: true}
	address, e := b.newSessionAddress(ctx, adapter, origin, thread)
	if e != nil {
		b.bindMu.Unlock()
		fail("创建独立群", thread.ID, e)
		return
	}
	session := store.Session{ThreadID: thread.ID, Address: address, WorkDir: thread.Cwd, Owner: f.UserID, Managed: true, GroupName: b.threadName(thread, address.Connection), Fresh: true}
	if e = b.store.Save(session); e != nil {
		b.bindMu.Unlock()
		fail("保存会话映射", thread.ID, e)
		return
	}
	b.bindMu.Unlock()
	_ = b.store.CreationProgress(f.ID, "claimed", "submitting_first_question", thread.ID, "")
	summary := fmt.Sprintf("项目：%s\n目录：%s\n模型：%s\n思考程度：%s\n新群：%s\n群 ID：%s\nCodex：%s", p.Name, thread.Cwd, selected.Model, selected.Effort, session.GroupName, address.ChatID, thread.ID)
	update("会话和群已创建，正在提交首问", summary+"\n已保存首条问题，无需去新群重新输入。若桌面尚未显示新会话，将明确报告，不会偷换执行通道。")
	e = b.jobs.Push(address.Key(), func(job context.Context) { b.runCreationFirstQuestion(job, adapter, f, session, selected, summary) })
	if e != nil {
		fail("首问入队", thread.ID, e)
	}
}

func (b *Bridge) runCreationFirstQuestion(parent context.Context, adapter im.Adapter, f store.CreationForm, session store.Session, selected codex.CreationSelection, summary string) {
	ctx := parent
	first := im.Message{ID: f.ID + ":first", Address: session.Address, UserID: f.UserID, Text: selected.Prompt}
	failed := func(e error) {
		notice, recorded := b.recordRequestFailure(first.ID, session.Address.Key(), e)
		if !recorded {
			return
		}
		status, hint := notice.Status, notice.Text()
		_ = b.store.CreationProgress(f.ID, status, "first_question", session.ThreadID, hint)
		b.messageDiagnostic(status, "新会话首问："+hint)
		// 首问失败必须通知新群，不能只更新原入口卡片，让新群一直显示“正在处理”。
		// 任务可能已取消；仅用独立的有界上下文投递状态，不重放模型请求。
		noticeCtx, cancelNotice := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancelNotice()
		if _, noticeErr := adapter.SendText(noticeCtx, session.Address, "会话和群已创建。\n"+hint+"\n不会重复建群或自动重放问题。", f.ID+":first-failure"); noticeErr != nil {
			b.log.Warn("新会话首问失败通知未确认送达", "form", f.ID, "error", diagnostics.Error(noticeErr))
		}
		if cardErr := adapter.UpdateCard(noticeCtx, f.Address, f.MessageID, im.Card{Title: "会话已创建 · " + notice.Title, Text: summary + "\n首问状态：" + hint + "\n不会重复建群或自动重放问题。请查看客户端任务记录。"}); cardErr != nil {
			b.log.Warn("新会话首问失败卡片更新未确认", "form", f.ID, "error", diagnostics.Error(cardErr))
		}
	}
	claimed, e := b.store.Claim(first.ID)
	if e != nil || !claimed {
		failed(errors.New("首问记录已存在或不可领取，未重放"))
		return
	}
	_, _ = adapter.SendText(ctx, session.Address, "首条问题（创建表单）：\n"+selected.Prompt, f.ID+":question-copy")
	if e = b.runTurn(ctx, adapter, session, first); e != nil {
		failed(e)
		return
	}
	_ = b.store.EventStage(first.ID, session.Address.Key(), "completed", "")
	_ = b.store.EventDone(first.ID, "done")
	_ = b.store.CreationProgress(f.ID, "done", "completed", session.ThreadID, "")
	_ = adapter.UpdateCard(ctx, f.Address, f.MessageID, im.Card{Title: "新会话首问已完成", Text: summary + "\n首问回复已发送到对应群，可继续在群内交流。"})
}

func (b *Bridge) OpenCreationFormForThread(ctx context.Context, thread string) error {
	s, e := b.store.ByThread(thread)
	if e != nil {
		return e
	}
	if s.Status != "active" {
		return diagnostics.UserError("请选择活跃会话作为表单发送入口")
	}
	adapter := b.channels[s.Address.Connection]
	if adapter == nil {
		return errors.New("连接未配置")
	}
	return b.openCreationForm(ctx, adapter, im.Message{ID: "local-form:" + store.ID(), Address: s.Address, UserID: s.Owner, Text: "/new"})
}
