package store

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// ClosureSnapshot 独立只读连接，桌面查看状态不调用 New，不重置审批或事件状态。
func ClosureSnapshot(dir string) ([]Closure, error) {
	path := filepath.Join(dir, "sessions.db")
	info, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return []Closure{}, nil
	}
	if e != nil || !info.Mode().IsRegular() {
		return nil, errors.New("无法读取业务状态数据库")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, errors.New("无法打开只读业务数据库")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec("PRAGMA busy_timeout=1000"); e != nil {
		return nil, e
	}
	var exists int
	e = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_closures'").Scan(&exists)
	if e != nil {
		return nil, errors.New("读取关闭状态失败")
	}
	if exists == 0 {
		return []Closure{}, nil
	}
	return (&Store{db: db}).ClosureRows(false)
}
