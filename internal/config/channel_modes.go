package config

// UsesDirectCodex 由渠道语义决定，不修改飞书的全局桌面提交设置。
func UsesDirectCodex(channel string) bool {
	return channel == "dingtalk" || channel == "dingtalk_personal"
}

func IsPersonalChannel(channel string) bool { return channel == "dingtalk_personal" }
