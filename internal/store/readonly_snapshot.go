package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errTaskBoardRead = errors.New("本机记录暂不可读取，请稍后刷新")

// 只读事务读取本机业务库，不执行迁移。
func readOnlySnapshot(ctx context.Context, dir string, read func(context.Context, *sql.Tx, map[string]bool) error) error {
	if ctx == nil || strings.TrimSpace(dir) == "" {
		return errTaskBoardRead
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	path, err := filepath.Abs(filepath.Join(dir, "sessions.db"))
	if err != nil {
		return errTaskBoardRead
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return errTaskBoardRead
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1000)"}}.Encode()}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return errTaskBoardRead
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return errTaskBoardRead
	}
	defer tx.Rollback()
	tables, err := snapshotTables(ctx, tx)
	if err != nil {
		return errTaskBoardRead
	}
	if err = read(ctx, tx, tables); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("记录已清理或不存在，请刷新")
		}
		return errTaskBoardRead
	}
	if err = tx.Commit(); err != nil {
		return errTaskBoardRead
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return errTaskBoardRead
	}
	return nil
}

func snapshotTables(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}
