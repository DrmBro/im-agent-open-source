//go:build darwin || linux

package drafts

import (
	"errors"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func checkPublishSupport() error { return nil }

// publishAt 仅接收随机 ID，在目录 fd 内构造单层文件名，不接受路径。
// Save 用 Linkat 保证目标已存在时失败；Mark 用 Renameat 原子替换完整快照。
func publishAt(dir *os.File, tempID, id string, replace bool) error {
	if dir == nil || !validID(tempID) || !validID(id) {
		return errors.New("草稿快照发布目录或标识无效")
	}
	fd := int(dir.Fd())
	tempName := ".pending-" + tempID + ".tmp"
	name := id + ".json"
	var err error
	if replace {
		err = unix.Renameat(fd, tempName, fd, name)
	} else {
		// flags 为零，不跟随源符号链接；文件身份和非符号链接检查由调用方完成。
		err = unix.Linkat(fd, tempName, fd, name, 0)
	}
	// 系统调用仅持有整数 fd，确保 os.File 在调用完成前不会被终结器关闭。
	runtime.KeepAlive(dir)
	if err != nil {
		if replace {
			return errors.New("草稿快照状态原子更新失败，已保留原记录")
		}
		return errors.New("草稿快照发布失败，未覆盖已有记录；原草稿不得移走")
	}
	return nil
}
