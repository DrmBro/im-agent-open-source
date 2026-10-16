package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

type TaskTarget struct {
	ThreadID string `json:"thread_id"`
	Name     string `json:"name"`
}

// 功能选择器与分页展示分离，返回全部活跃会话的轻量目标，不带正文。
func TaskTargetsSnapshot(ctx context.Context, dir string) ([]TaskTarget, error) {
	out := []TaskTarget{}
	path := filepath.Join(dir, "sessions.db")
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return out, errors.New("会话目标数据库不可读取")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return out, errors.New("会话目标数据库不可读取")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var has int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_presentation'").Scan(&has); err != nil {
		return out, errors.New("会话目标读取失败")
	}
	query := `SELECT s.thread_id, ''
FROM sessions s
WHERE s.status = 'active'
ORDER BY s.created_at DESC, s.thread_id`
	if has > 0 {
		query = `SELECT s.thread_id, coalesce(p.target_name, '')
FROM sessions s
         LEFT JOIN session_presentation p ON p.thread_id = s.thread_id
WHERE s.status = 'active'
ORDER BY s.created_at DESC, s.thread_id`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return out, errors.New("会话目标读取失败")
	}
	defer rows.Close()
	for rows.Next() {
		var t TaskTarget
		if err = rows.Scan(&t.ThreadID, &t.Name); err != nil {
			return nil, errors.New("会话目标读取失败")
		}
		out = append(out, t)
	}
	if rows.Err() != nil {
		return nil, errors.New("会话目标读取中断")
	}
	return out, nil
}
