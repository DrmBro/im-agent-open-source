package feishu

import (
	"im-agent/internal/im"
	"im-agent/internal/redact"
)

func formElements(c im.Card) []any {
	submitChoice, cancelLabel, cancelChoice := c.Form.SubmitChoice, c.Form.CancelLabel, c.Form.CancelChoice
	if submitChoice == "" {
		submitChoice = "create"
	}
	if cancelLabel == "" {
		cancelLabel = "取消"
	}
	if cancelChoice == "" {
		cancelChoice = "cancel"
	}
	fields := []any{}
	for _, f := range c.Form.Fields {
		// JSON 1.0 的 form 不允许直接嵌套 div，否则客户端会丢弃整个表单。
		fields = append(fields, map[string]any{"tag": "markdown", "content": f.Label})
		if len(f.Options) > 0 {
			options := []any{}
			for _, o := range f.Options {
				options = append(options, map[string]any{"text": map[string]string{"tag": "plain_text", "content": redact.Clip(o.Label, 100)}, "value": o.Value})
			}
			v := map[string]any{"tag": "select_static", "name": f.Name, "required": f.Required, "placeholder": map[string]string{"tag": "plain_text", "content": f.Placeholder}, "options": options}
			if f.Default != "" {
				v["initial_option"] = f.Default
			}
			fields = append(fields, v)
		} else {
			kind := "text"
			if f.Multiline {
				kind = "multiline_text"
			}
			v := map[string]any{"tag": "input", "name": f.Name, "required": f.Required, "input_type": kind, "placeholder": map[string]string{"tag": "plain_text", "content": f.Placeholder}, "max_length": f.MaxLength}
			if f.Default != "" {
				v["default_value"] = f.Default
			}
			fields = append(fields, v)
		}
	}
	fields = append(fields, map[string]any{"tag": "button", "name": "create", "action_type": "form_submit", "text": map[string]string{"tag": "plain_text", "content": c.Form.SubmitLabel}, "type": "primary", "value": map[string]string{"action_id": c.ActionID, "choice": submitChoice}})
	actions := []any{}
	for _, button := range c.Form.Navigation {
		actions = append(actions, map[string]any{"tag": "button", "text": map[string]string{"tag": "plain_text", "content": button.Label}, "type": "default", "value": map[string]string{"action_id": c.ActionID, "choice": button.Value}})
	}
	actions = append(actions, map[string]any{"tag": "button", "text": map[string]string{"tag": "plain_text", "content": cancelLabel}, "type": "default", "value": map[string]string{"action_id": c.ActionID, "choice": cancelChoice}})
	return []any{map[string]any{"tag": "form", "name": "new_conversation", "elements": fields}, map[string]any{"tag": "action", "actions": actions}}
}
