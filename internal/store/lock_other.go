//go:build !darwin && !linux

package store

import "errors"

func lockDirectory(string) (func(), error) {
	return nil, errors.New("当前运行锁实现支持 macOS/Linux；其他平台暂不启动桥接，以免多个实例冲突")
}
