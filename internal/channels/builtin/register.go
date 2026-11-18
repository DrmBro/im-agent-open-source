// Package builtin 统一桌面、后台和命令行入口的渠道注册。
package builtin

import (
	"im-agent/internal/channels"
	"im-agent/internal/channels/dingtalk"
	"im-agent/internal/channels/dwspersonal"
	"im-agent/internal/channels/feishu"
)

func Register(r *channels.Registry) error {
	if err := feishu.Register(r); err != nil {
		return err
	}
	if err := dingtalk.Register(r); err != nil {
		return err
	}
	return dwspersonal.Register(r)
}
