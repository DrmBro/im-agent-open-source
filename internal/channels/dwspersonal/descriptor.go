package dwspersonal

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"im-agent/internal/channels"
	"im-agent/internal/config"
)

const channel = "dingtalk_personal"

var profilePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+:[A-Za-z0-9_-]+$`)

func validID(id string) bool {
	return id != "" && len(id) <= 256 && !strings.HasPrefix(id, "-") && !strings.ContainsAny(id, "@, \t\r\n\x00")
}
func validate(c config.Connection) error {
	if c.PersonalGroupFollowup && !c.PersonalMentions {
		return errors.New("群跟进需要同时开启群@监听")
	}
	if c.PersonalFollowupMinutes != 0 && (c.PersonalFollowupMinutes < 5 || c.PersonalFollowupMinutes > 120) {
		return errors.New("群跟进时间允许5至120分钟，默认30分钟")
	}
	if c.AutoCreateChat || c.SharedChatID != "" || c.ConversationMode == "topic" {
		return errors.New("钉钉个人账号使用已有会话，请关闭自动建群")
	}
	if len(c.AllowedUsers) > 21 {
		return errors.New("个人代回复最多配置本人和 20 位联系人")
	}
	seen := map[string]bool{}
	for _, id := range c.AllowedUsers {
		if seen[id] {
			return errors.New("个人联系人白名单存在重复 ID")
		}
		seen[id] = true
	}
	for _, id := range append(append([]string{}, c.AllowedUsers...), c.AdminUsers...) {
		if !validID(id) {
			return errors.New("请填写完整的 openDingtalkId，不使用昵称或员工 UserID")
		}
	}
	// 草稿保存时凭据尚未解密；完整校验在 Factory 中进行。
	return nil
}
func validateCredentials(c config.Connection) error {
	if err := validate(c); err != nil {
		return err
	}
	profile, _ := c.Credential("profile")
	self, _ := c.Credential("self_open_id")
	binary, _ := c.Credential("dws_binary")
	if !profilePattern.MatchString(profile) {
		return errors.New("DWS 账号必须是 profile list 返回的精确 corpId:userId")
	}
	if !validID(self) || len(c.AdminUsers) != 1 || c.AdminUsers[0] != self || !config.Contains(c.AllowedUsers, self) {
		return errors.New("个人渠道管理员只能是本人的 openDingtalkId，并须同时加入白名单")
	}
	if binary != "" && !filepath.IsAbs(binary) {
		return errors.New("DWS 程序路径必须为绝对路径")
	}
	return nil
}
func Register(r *channels.Registry) error {
	return r.RegisterDescriptor(channels.Descriptor{Type: channel, Name: "钉钉个人账号（DWS）", Description: "本人授权 · 私聊与群@ · 桌面审核",
		UserHint: "此处填写 openDingtalkId（不是机器人 senderStaffId）。管理员只填本人；全部私聊或群@模式无需逐个添加联系人。指定联系人模式最多20位。",
		Credentials: []channels.CredentialField{
			{Key: "profile", Label: "DWS 登录账号（corpId:userId）", Required: true, Help: "先通过 dws auth login 授权，再使用 dws profile list 中的精确 profile。"},
			{Key: "self_open_id", Label: "本人 openDingtalkId", Required: true, Help: "用于过滤本人消息，避免自动回复循环；需与上述登录账号对应。"},
			{Key: "dws_binary", Label: "DWS 程序绝对路径（可选）", Help: "默认使用 ~/.local/bin/dws，兼容已核对的 DWS v1.0.68。OAuth 令牌由 DWS 保存，无需填写应用密钥。"},
		}}, Factory, validate)
}
