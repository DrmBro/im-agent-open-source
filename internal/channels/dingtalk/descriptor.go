package dingtalk

import (
	"errors"
	"im-agent/internal/channels"
	"im-agent/internal/config"
)

func Register(r *channels.Registry) error {
	return r.RegisterDescriptor(channels.Descriptor{
		Type: "dingtalk", Name: "钉钉", Description: "Stream 接收 · 独立 Codex CLI · 私聊/已有群文本会话",
		UserHint: "填写企业内部应用回调中的 senderStaffId（企业员工 userId），每行一个；不使用 unionId 或昵称。",
		Credentials: []channels.CredentialField{
			{Key: "app_id", Label: "Client ID / AppKey", Required: true, Help: "企业内部应用的 Client ID。"},
			{Key: "app_secret", Label: "Client Secret / AppSecret", Required: true, Help: "企业内部应用密钥；加密保存。"},
			{Key: "robot_code", Label: "Robot Code", Required: true, Help: "应用机器人的 robotCode，用于私聊与群主动发送。"},
			{Key: "corp_id", Label: "Corp ID", Required: true, Help: "仅接受本企业员工消息，拒绝跨企业身份。"},
		},
	}, Factory, func(c config.Connection) error {
		if c.AutoCreateChat || c.SharedChatID != "" || c.ConversationMode == "topic" {
			return errors.New("钉钉目前仅绑定私聊或已有群，请关闭自动建群并清空共享群配置")
		}
		return nil
	})
}
