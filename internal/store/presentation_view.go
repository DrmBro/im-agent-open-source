package store

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// PresentationSnapshot 仅读取状态，不向前端返回正文，不执行迁移或发送操作。
func PresentationSnapshot(dir string) ([]Presentation, error) {
	path := filepath.Join(dir, "sessions.db")
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return []Presentation{}, nil
	}
	if e != nil || !st.Mode().IsRegular() {
		return nil, errors.New("业务数据库不可读")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, e = db.Exec("PRAGMA busy_timeout=1000"); e != nil {
		return nil, e
	}
	var exists int
	if e = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_presentation'").Scan(&exists); e != nil {
		return nil, e
	}
	if exists == 0 {
		return []Presentation{}, nil
	}
	rows, e := (&Store{db: db}).Presentations(false)
	for i := range rows {
		rows[i].Payload = ""
	}
	return rows, e
}
