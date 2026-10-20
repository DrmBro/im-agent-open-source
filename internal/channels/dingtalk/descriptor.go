// Package dingtalk implements the DingTalk Stream group question-and-answer channel.
package dingtalk

import (
	"errors"
	"strings"

	"im-agent/internal/channels"
	"im-agent/internal/config"
	"im-agent/internal/im"
)

func Register(r *channels.Registry) error {
	return r.RegisterDescriptor(channels.Descriptor{
		Type: "dingtalk", Name: "钉钉", Description: "Stream 群问答 · 直接 @新开，引用回答续聊",
		UserHint: "填写企业 UserID，每行一个；管理员须在白名单中。不知道 ID 时，白名单和管理员同时留空进入识别模式，仅响应群内 @机器人的 /whoami，不执行问答。",
		Credentials: []channels.CredentialField{
			{Key: "app_id", Label: "Client ID / AppKey", Required: true, Help: "企业内部应用的 Client ID；加密保存。"},
			{Key: "app_secret", Label: "Client Secret / AppSecret", Required: true, Help: "应用密钥；留空保留已有值。"},
			{Key: "robot_code", Label: "RobotCode", Required: true, Help: "机器人编码，用于群消息发送接口。"},
		},
	}, func(c config.Connection) (im.Adapter, error) { return New(c) }, func(c config.Connection) error {
		if c.ConversationMode == "topic" || c.SharedChatID != "" {
			return errors.New("钉钉使用已有群内的独立问答，不配置共享话题群")
		}
		for _, u := range append(append([]string{}, c.AllowedUsers...), c.AdminUsers...) {
			if strings.HasPrefix(u, "ou_") {
				return errors.New("钉钉白名单使用企业用户 ID，不能使用飞书 Open ID")
			}
		}
		return nil
	})
}
