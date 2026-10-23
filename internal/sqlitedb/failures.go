package sqlitedb

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"
)

const failureLogLimit = 256 << 10

type failureLog struct {
	mu   sync.Mutex
	path string
}

// 不保存 SQL、参数、业务标识、文件路径或原始错误；调用位置只取项目函数名。
// 最多保留当前和上一份各 256 KiB 的日志，数据库故障不会导致日志无限增长。
func (l *failureLog) record(operation string, err, rollbackErr error) {
	entry := struct {
		At                string   `json:"at"`
		Operation         string   `json:"operation"`
		Code              int      `json:"sqlite_code"`
		RollbackCode      int      `json:"rollback_sqlite_code"`
		RollbackFailed    bool     `json:"rollback_failed"`
		DiscardConnection bool     `json:"discard_connection"`
		Callers           []string `json:"callers"`
	}{time.Now().UTC().Format(time.RFC3339Nano), operation, sqliteCode(err), sqliteCode(rollbackErr), rollbackErr != nil, true, projectCallers()}
	data, marshalErr := json.Marshal(entry)
	if marshalErr != nil {
		return
	}
	data = append(data, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	info, statErr := os.Lstat(l.path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return
	}
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return
		}
		if err := os.Chmod(l.path, 0600); err != nil {
			return
		}
		if info.Size()+int64(len(data)) > failureLogLimit {
			if err := os.Rename(l.path, l.path+".1"); err != nil {
				return
			}
		}
	}
	f, openErr := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if openErr != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(data)
}

// 0 表示无错误，-1 表示非 SQLite 错误，避免将错误字符串作为日志内容。
func sqliteCode(err error) int {
	if err == nil {
		return 0
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()
	}
	return -1
}

func projectCallers() []string {
	var pcs [24]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	var names []string
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, "im-agent/internal/") && !strings.HasPrefix(frame.Function, "im-agent/internal/sqlitedb.") {
			names = append(names, frame.Function)
			if len(names) == 8 {
				break
			}
		}
		if !more {
			break
		}
	}
	return names
}
