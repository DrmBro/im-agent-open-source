//go:build darwin || linux

package daemon

import (
	"errors"
	"os"
	"syscall"
)

func lockDaemon(path string) (func(), error) {
	fd, e := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("后台服务已在运行")
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
}
func detachAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
