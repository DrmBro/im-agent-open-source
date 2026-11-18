package codex

import (
	"encoding/json"
	"errors"
)

// 用户明确要求：所有由 IM 创建的新会话使用 Full Access。
// 仅修改创建请求，辅助进程自身仍使用原后台配置。
func fullAccessCreation(params map[string]any) {
	params["sandbox"] = "danger-full-access"
	params["approvalPolicy"] = "never"
}

// 以服务器确认的实际权限为准，未确认时不提交首问或重复创建。
func verifyFullAccessCreation(raw json.RawMessage) error {
	var response struct {
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        struct {
			Type string `json:"type"`
		} `json:"sandbox"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return errors.New("新会话权限回执格式无效，未提交首问")
	}
	if response.ApprovalPolicy != "never" || response.Sandbox.Type != "dangerFullAccess" {
		return errors.New("新会话未确认 Full Access 权限，未提交首问；请核对已创建会话，不要重复创建")
	}
	return nil
}
