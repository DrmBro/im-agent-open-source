package feishu

import (
	"errors"
	"im-agent/internal/channels"
	"im-agent/internal/config"
	"strings"
)

func Register(r *channels.Registry) error {
	return r.RegisterDescriptor(channels.Descriptor{Type: "feishu", Name: "飞书", SupportsTopics: true, Description: "官方长连接 · 消息与交互卡片", UserHint: "使用 ou_ 开头的飞书 Open ID，每行一个。管理员必须在白名单内。", Credentials: []channels.CredentialField{
		{Key: "app_id", Label: "App ID", Required: true, Help: "飞书开发者后台的应用 ID；加密保存，不回显。"},
		{Key: "app_secret", Label: "App Secret", Required: true, Help: "应用密钥；留空保留已有值。"},
		{Key: "verification_token", Label: "Verification Token", Help: "可选，长连接通常不需要。"},
		{Key: "encrypt_key", Label: "Encrypt Key", Help: "可选，按飞书后台配置填写。"},
	}}, Factory, func(c config.Connection) error {
		if c.SharedChatID != "" && !strings.HasPrefix(c.SharedChatID, "oc_") {
			return errors.New("飞书共享群 ID 必须以 oc_ 开头")
		}
		for _, u := range append(append([]string{}, c.AllowedUsers...), c.AdminUsers...) {
			if !strings.HasPrefix(u, "ou_") {
				return errors.New("飞书白名单与管理员必须使用 ou_ 开头的 Open ID")
			}
		}
		return nil
	})
}
