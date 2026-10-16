// Package attachments 管理非执行型附件。文件内容不构成指令或权限授权。
package attachments

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const MaxBytes int64 = 20 << 20

var ErrTooLarge = errors.New("附件超过20 MiB或配置的上限")
var ErrEmpty = errors.New("附件为空")

func SafeName(name string) error {
	if name == "" || len([]rune(name)) > 180 || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("附件文件名无效")
	}
	return nil
}
func Save(dir, key, name string, reader io.Reader, limit int64) (string, error) {
	if e := SafeName(name); e != nil {
		return "", e
	}
	if limit <= 0 || limit > MaxBytes {
		return "", errors.New("附件大小上限无效")
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("附件目录无效")
	}
	if e = os.Chmod(dir, 0700); e != nil {
		return "", e
	}
	f, e := os.CreateTemp(dir, ".incoming-*")
	if e != nil {
		return "", e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	n, e := io.Copy(f, io.LimitReader(reader, limit+1))
	if e != nil {
		f.Close()
		return "", e
	}
	if n > limit {
		f.Close()
		return "", ErrTooLarge
	}
	if n == 0 {
		f.Close()
		return "", ErrEmpty
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return "", e
	}
	if e = f.Close(); e != nil {
		return "", e
	}
	hash := sha256.Sum256([]byte(key))
	final := filepath.Join(dir, hex.EncodeToString(hash[:16])+"-"+name)
	// 原子硬链接而非覆盖：重复事件不能替换已交给模型的文件。
	if e = os.Link(tmp, final); e != nil {
		return "", errors.New("附件保存失败或同一附件已经存在；未覆盖")
	}
	return final, nil
}
func OpenExport(workDir, path string, limit int64) (*os.File, string, error) {
	if !filepath.IsAbs(workDir) || limit <= 0 || limit > MaxBytes {
		return nil, "", errors.New("会话目录或附件上限无效")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	rel, e := filepath.Rel(workDir, path)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, "", errors.New("只能发送该会话工作目录内的文件")
	}
	// Root.Open 由 Go 的目录能力限制解析；拒绝符号链接逃逸，而非只做字符串前缀检查。
	root, e := os.OpenRoot(workDir)
	if e != nil {
		return nil, "", e
	}
	defer root.Close()
	f, e := root.Open(rel)
	if e != nil {
		return nil, "", errors.New("文件不在允许目录内、符号链接越界或无法读取")
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, "", errors.New("只支持普通文件")
	}
	if info.Size() <= 0 || info.Size() > limit {
		f.Close()
		return nil, "", fmt.Errorf("文件大小必须在 1..%d 字节之间", limit)
	}
	name := filepath.Base(path)
	if e = SafeName(name); e != nil {
		f.Close()
		return nil, "", e
	}
	return f, name, nil
}

// IncomingPath 与Save使用同一稳定路径，不从外部记录接受任意读取路径。
func IncomingPath(dir, key, name string) (string, error) {
	if err := SafeName(name); err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) || key == "" {
		return "", errors.New("入站附件目录或身份无效")
	}
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(hash[:16])+"-"+name), nil
}

// InspectIncoming 只核对本次来源身份的原子落盘普通文件，拒绝链接、超限和内容变化。
func InspectIncoming(dir, key, name string) (path, proof string, size int64, err error) {
	path, err = IncomingPath(dir, key, name)
	if err != nil {
		return
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		err = statErr
		return
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		err = errors.New("附件落盘对象不是普通文件")
		return
	}
	if info.Size() <= 0 || info.Size() > MaxBytes {
		err = errors.New("附件本机文件为空或超过20 MiB")
		return
	}
	root, openErr := os.OpenRoot(dir)
	if openErr != nil {
		err = openErr
		return
	}
	defer root.Close()
	file, openErr := root.Open(filepath.Base(path))
	if openErr != nil {
		err = openErr
		return
	}
	defer file.Close()
	hash := sha256.New()
	size, err = io.Copy(hash, io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return
	}
	if size != info.Size() || size > MaxBytes {
		err = errors.New("附件核对期间内容或大小发生变化")
		return
	}
	proof = hex.EncodeToString(hash.Sum(nil))
	return
}
