//go:build !darwin && !linux

package daemon

import (
	"errors"
	"syscall"
)

func lockDaemon(string) (func(), error)      { return nil, errors.New("本平台暂不支持后台服务") }
func detachAttributes() *syscall.SysProcAttr { return nil }
