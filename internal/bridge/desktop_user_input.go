package bridge

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"im-agent/internal/codex"
	"im-agent/internal/im"
	"im-agent/internal/store"
)

func desktopUserInput(request codex.DesktopApproval) bool {
	return request.Kind == "input" || request.Kind == "async_input"
}

func validDesktopQuestions(questions []codex.DesktopInputQuestion) bool {
	if len(questions) == 0 || len(questions) > 10 || len(store.JSON(questions)) > 24000 {
		return false
	}
	ids := map[string]bool{}
	for _, q := range questions {
		if q.ID == "" || len(q.ID) > 1024 || ids[q.ID] || strings.TrimSpace(q.Question) == "" || q.IsSecret || len(q.Options) > 100 {
			return false
		}
		ids[q.ID] = true
		for _, option := range q.Options {
			if strings.TrimSpace(option.Label) == "" {
				return false
			}
		}
	}
	return true
}

func (b *Bridge) desktopInputCard(c store.DesktopApprovalCard, request codex.DesktopApproval) im.Card {
	card := im.Card{Title: "Question · 需要你的回答", ActionID: "desktop-approval:" + c.ID}
	parts := []string{}
	form := &im.Form{SubmitLabel: "发送回答", SubmitChoice: "answer", CancelLabel: "跳过", CancelChoice: "skip"}
	for i, q := range request.Questions {
		label := fmt.Sprintf("问题 %d", i+1)
		if q.Header != "" {
			label += " · " + q.Header
		}
		text := label + "\n" + q.Question
		options := []im.Button{}
		for n, option := range q.Options {
			options = append(options, im.Button{Label: option.Label, Value: strconv.Itoa(n)})
			text += fmt.Sprintf("\n%d. %s", n+1, option.Label)
			if option.Description != "" {
				text += "：" + option.Description
			}
		}
		parts = append(parts, text)
		if len(options) > 0 {
			form.Fields = append(form.Fields, im.FormField{Name: fmt.Sprintf("option_%d", i), Label: fmt.Sprintf("问题 %d：选择一项", i+1), Placeholder: "请选择，或在下方填写自定义回答", Options: options})
		}
		form.Fields = append(form.Fields, im.FormField{Name: fmt.Sprintf("text_%d", i), Label: fmt.Sprintf("问题 %d：自定义回答", i+1), Placeholder: "填写后优先使用此回答", Multiline: true, MaxLength: 1000})
	}
	card.Text = strings.Join(parts, "\n\n")
	if c.State == "pending" && c.ExpiresAt == 0 {
		card.Form = form
		card.Text += "\n\n每题选择一项或填写自定义回答，然后一次发送；自定义回答优先。跳过会跳过整组问题。没有等待有效期。"
	} else {
		card.Text += "\n\n" + c.Detail
	}
	return card
}

// 只接受当前问题对应的字段；选项保存索引，由桌面按原始请求还原标签。
func desktopInputAnswers(request codex.DesktopApproval, in im.Interaction) (map[string]codex.DesktopInputAnswer, error) {
	answers := map[string]codex.DesktopInputAnswer{}
	if in.Choice == "skip" {
		return answers, nil
	}
	if in.Choice != "answer" {
		return nil, errors.New("请选择发送回答或跳过")
	}
	fields := map[string]bool{}
	for i, q := range request.Questions {
		optionKey, textKey := fmt.Sprintf("option_%d", i), fmt.Sprintf("text_%d", i)
		fields[textKey] = true
		if len(q.Options) > 0 {
			fields[optionKey] = true
		}
		text := strings.TrimSpace(in.Fields[textKey])
		if utf8.RuneCountInString(text) > 1000 || len(text) > 4000 {
			return nil, errors.New("自定义回答最多 1000 字")
		}
		if text != "" {
			answers[q.ID] = codex.DesktopInputAnswer{Text: text}
			continue
		}
		key := in.Fields[optionKey]
		n, err := strconv.Atoi(key)
		if err != nil || n < 0 || n >= len(q.Options) || key != strconv.Itoa(n) {
			return nil, fmt.Errorf("请回答问题 %d：选择一项或填写自定义回答", i+1)
		}
		answers[q.ID] = codex.DesktopInputAnswer{Option: &n}
	}
	for key := range in.Fields {
		if !fields[key] {
			return nil, errors.New("回答包含不属于当前问题的字段")
		}
	}
	return answers, nil
}
