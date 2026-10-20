//go:build !darwin && !linux

package qa

import (
	"errors"
	"os"
)

func openBusinessText(string) (*os.File, error) {
	return nil, errors.New("当前平台尚未支持业务文件的安全读取")
}
func openDocumentDirectory(string, string, bool) (*os.File, error) {
	return nil, errors.New("当前平台尚未支持业务文档存储")
}
func openSecureFileAt(*os.File, string) (*os.File, error) {
	return nil, errors.New("当前平台尚未支持业务文档读取")
}
func publishDocumentAt(*os.File, string, []byte) error {
	return errors.New("当前平台尚未支持业务文档发布")
}
