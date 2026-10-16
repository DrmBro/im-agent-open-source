// Package diagnostics 只提取错误类别和数字代码，绝不将原始错误体直接送到界面。
package diagnostics

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// UserError 仅用于源码内固定的操作提示，不得包裹平台原始响应、密钥或用户正文。
type UserError string

func (e UserError) Error() string { return string(e) }

var codes = regexp.MustCompile(`(?:HTTP [1-5][0-9]{2}|status [1-5][0-9]{2}|code=-?[0-9]{1,12})`)

func Error(err error) string {
	if err == nil {
		return ""
	}
	var userError UserError
	if errors.As(err, &userError) {
		return string(userError)
	}
	if errors.Is(err, context.Canceled) {
		return "操作已取消"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "请求超时"
	}
	message := err.Error()
	if strings.Contains(message, "model_not_found") || strings.Contains(message, "模型") && strings.Contains(message, "不存在") {
		return "中转返回 model_not_found：请确认当前模型与真实客户端的授权，重试或改客户端标识不能代替授权"
	}
	// 投递未确认等包装错误不能吞掉内层的安全状态码。
	for cause, depth := err, 0; cause != nil && depth < 8; cause, depth = errors.Unwrap(cause), depth+1 {
		values := codes.FindAllString(cause.Error(), 3)
		if len(values) > 0 {
			return strings.Join(values, "；")
		}
	}
	return "请求失败（原始错误已隐藏）"
}
