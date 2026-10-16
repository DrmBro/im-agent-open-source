//go:build !darwin && !linux

package drafts

import (
	"errors"
	"os"
)

// 不支持目录 fd 锚定发布的平台在写入、创建目录或访问钥匙串之前拒绝 Save/Mark。
// List 和 Read 仍可使用；禁止绝对路径回退或明文降级。
func checkPublishSupport() error {
	return errors.New("当前平台不支持安全发布草稿快照，已拒绝暂存或更新；请保留原草稿")
}

func publishAt(_ *os.File, _, _ string, _ bool) error {
	return checkPublishSupport()
}
