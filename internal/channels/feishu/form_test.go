package feishu

import (
	"encoding/json"
	"im-agent/internal/im"
	"strings"
	"testing"
)

func TestCreationCardContainsOneSubmitAndNamedFields(t *testing.T) {
	raw := cardPayload(im.Card{Title: "创建会话", ActionID: "new:abc", Form: &im.Form{SubmitLabel: "创建并开始", Fields: []im.FormField{{Name: "project", Label: "项目", Required: true, Options: []im.Button{{Label: "示例项目", Value: "p:0"}}}, {Name: "prompt", Label: "首条问题", Required: true, Multiline: true, MaxLength: 4000}}}})
	if !json.Valid([]byte(raw)) || !strings.Contains(raw, "form_submit") || !strings.Contains(raw, "multiline_text") || !strings.Contains(raw, `"name":"project"`) {
		t.Fatal("form missing required components", raw)
	}
}
