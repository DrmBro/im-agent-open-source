//go:build darwin || linux

package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// 内核锁在异常退出时自动释放，不通过 PID 文件猜测运行状态。
func lockDirectory(dir string) (func(), error) {
	path := filepath.Join(dir, "bridge.lock")
	fd, e := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, errors.New("无法打开桥接锁文件")
	}
	f := os.NewFile(uintptr(fd), path)
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		_ = f.Close()
		return nil, errors.New("该状态目录已有桥接实例运行，请先停止旧实例")
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
}
