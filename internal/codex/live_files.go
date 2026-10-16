package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LiveRolloutFiles 只列出本机 Codex 会话目录中元数据身份完全匹配的文件。
// 文件名时间只决定扫描顺序，不能代替 session_meta.id 证明来源。
func (c *Client) LiveRolloutFiles(ctx context.Context, thread string) ([]string, error) {
	home := c.runtimeConfig.Codex.Home
	if home == "" {
		root, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(root, ".codex")
	}
	var files []string
	var firstFailure error
	visit := func(path string, entry os.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") || !strings.Contains(entry.Name(), thread) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("会话日志候选是符号链接，未跟随")
		}
		f, _, err := c.openLiveRollout(thread, path)
		if err != nil {
			if firstFailure == nil {
				firstFailure = err
			}
			return nil // 留下诊断，但不让一个坏文件阻止其它已核验文件。
		}
		f.Close()
		files = append(files, path)
		return nil
	}
	for _, folder := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(home, folder)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return files, err
		}
		if err := filepath.WalkDir(root, visit); err != nil {
			return files, err
		}
	}
	sort.Slice(files, func(i, j int) bool {
		left, right := filepath.Base(files[i]), filepath.Base(files[j])
		if left != right {
			return left < right
		}
		return files[i] < files[j]
	})
	for i := 1; i < len(files); i++ {
		if filepath.Base(files[i]) == filepath.Base(files[i-1]) {
			return nil, errors.New("同一会话日志文件名重复，未猜测读取顺序")
		}
	}
	return files, firstFailure
}

func (c *Client) LiveRolloutInfo(ctx context.Context, thread, path string) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	file, header, err := c.openLiveRollout(thread, path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, 0, err
	}
	return header, info.Size(), nil
}
