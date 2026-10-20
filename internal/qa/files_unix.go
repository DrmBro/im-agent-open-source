//go:build darwin || linux

package qa

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openSecureDirectory fixes each ancestor by descriptor. Every component is
// opened with NOFOLLOW, so a directory swap cannot redirect a later path lookup.
func openSecureDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("目录必须是绝对路径")
	}
	clean := filepath.Clean(path)
	fd, e := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, errors.New("目录含符号链接或不可读取")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), "业务目录"), nil
}
func openSecureFileAt(directory *os.File, name string) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return nil, errors.New("文件名无效")
	}
	fd, e := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if e != nil {
		return nil, errors.New("文件含符号链接或不可读取")
	}
	file := os.NewFile(uintptr(fd), "业务文档")
	st, e := file.Stat()
	if e != nil || !st.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("目标不是普通文件")
	}
	return file, nil
}
func openBusinessText(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("业务资料必须是绝对路径")
	}
	directory, e := openSecureDirectory(filepath.Dir(path))
	if e != nil {
		return nil, e
	}
	defer directory.Close()
	return openSecureFileAt(directory, filepath.Base(path))
}
func openDocumentDirectory(stateDir, question string, create bool) (*os.File, error) {
	if !validDocumentComponent(question) {
		return nil, errors.New("文档问答标识无效")
	}
	root, e := openSecureDirectory(stateDir)
	if e != nil {
		return nil, e
	}
	current := root
	for _, name := range []string{"qa-documents", question} {
		fd, err := unix.Openat(int(current.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.ENOENT && create {
			if err = unix.Mkdirat(int(current.Fd()), name, 0700); err != nil && err != unix.EEXIST {
				current.Close()
				return nil, errors.New("文档目录创建失败")
			}
			fd, err = unix.Openat(int(current.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		current.Close()
		if err != nil {
			return nil, errors.New("文档目录不可读取或为符号链接")
		}
		current = os.NewFile(uintptr(fd), "问答文档目录")
	}
	return current, nil
}
func publishDocumentAt(directory *os.File, name string, content []byte) error {
	if !validDocumentFilename(name) {
		return errors.New("文档文件名无效")
	}
	nonce := make([]byte, 16)
	if _, e := rand.Read(nonce); e != nil {
		return e
	}
	temp := ".document-" + hex.EncodeToString(nonce)
	fd, e := unix.Openat(int(directory.Fd()), temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return errors.New("无法创建文档临时文件")
	}
	defer unix.Unlinkat(int(directory.Fd()), temp, 0)
	file := os.NewFile(uintptr(fd), "文档临时文件")
	_, e = file.Write(content)
	if e == nil {
		e = file.Sync()
	}
	closeErr := file.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return errors.New("业务文档写入失败")
	}
	if e = unix.Linkat(int(directory.Fd()), temp, int(directory.Fd()), name, 0); e != nil {
		return errors.New("文档发布失败，未覆盖已有文档")
	}
	_ = directory.Sync()
	return nil
}
