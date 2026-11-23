package im

import (
	"encoding/base64"
	"errors"
	"strings"
)

// 问题地址只用于本机队列和 session 唯一键，不是平台群/话题标识。
const questionPrefix = "personal-question:"

func QuestionAddress(wire Address, id string) (Address, error) {
	if wire.Channel != "dingtalk_personal" || wire.ChatID == "" || wire.IsTopic() || id == "" || strings.ContainsAny(id, ".\x00\r\n") {
		return Address{}, errors.New("问题会话地址无效")
	}
	wire.ChatID = questionPrefix + base64.RawURLEncoding.EncodeToString([]byte(wire.ChatID)) + "." + id
	return wire, nil
}
func (a Address) IsPersonalQuestion() bool {
	return a.Channel == "dingtalk_personal" && strings.HasPrefix(a.ChatID, questionPrefix)
}

func (a Address) DeliveryAddress() (Address, error) {
	if !strings.HasPrefix(a.ChatID, questionPrefix) {
		return a, nil
	}
	if a.Channel != "dingtalk_personal" {
		return Address{}, errors.New("问题地址渠道不匹配")
	}
	encoded, id, ok := strings.Cut(strings.TrimPrefix(a.ChatID, questionPrefix), ".")
	if !ok || id == "" || len(encoded) > 4096 {
		return Address{}, errors.New("问题地址格式无效")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		return Address{}, errors.New("问题地址不可解析")
	}
	a.ChatID = string(raw)
	return a, nil
}
