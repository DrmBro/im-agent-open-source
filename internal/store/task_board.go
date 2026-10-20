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
	"unicode/utf8"
)

type TaskBoardQuery struct {
	Source   string `json:"source"`
	State    string `json:"state"`
	Search   string `json:"search"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
}
type TaskBoardItem struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	ThreadID   string `json:"thread_id"`
	Connection string `json:"connection"`
	Title      string `json:"title"`
	State      string `json:"state"`
	Stage      string `json:"stage"`
	Execution  string `json:"execution"`
	Delivery   string `json:"delivery"`
	Detail     string `json:"detail"`
	ReceivedAt int64  `json:"received_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

type TaskBoardView struct {
	Items      []TaskBoardItem `json:"items"`
	Counts     map[string]int  `json:"counts"`
	Total      int             `json:"total"`
	Page       int             `json:"page"`
	PageSize   int             `json:"page_size"`
	CapturedAt int64           `json:"captured_at"`
	Warning    string          `json:"warning"`
}
type TaskBoardDetail struct {
	Item    TaskBoardItem  `json:"item"`
	Result  *RequestResult `json:"result,omitempty"`
	Warning string         `json:"warning"`
}

var errTaskBoardRead = errors.New("任务看板暂不可读取，请稍后刷新")

func scanTaskBoardItem(row interface{ Scan(...any) error }) (TaskBoardItem, error) {
	var item TaskBoardItem
	err := row.Scan(&item.Source, &item.ID, &item.ThreadID, &item.Connection, &item.Title, &item.State, &item.Stage, &item.Execution, &item.Delivery, &item.Detail, &item.ReceivedAt, &item.UpdatedAt)
	return item, err
}

// 所有统计和当前页共用只读事务；页面条数不参与全量计数，也不执行迁移。
func taskBoardRead(ctx context.Context, dir string, read func(context.Context, *sql.Tx, map[string]bool) error) error {
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
	tables, err := historyPageTables(ctx, tx)
	if err != nil {
		return errTaskBoardRead
	}
	if err = read(ctx, tx, tables); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("这条任务已清理或不存在，请刷新任务列表")
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

func TaskBoardSnapshot(ctx context.Context, dir string, q TaskBoardQuery) (TaskBoardView, error) {
	if q.Source != "" && q.Source != "session" {
		return TaskBoardView{}, errors.New("任务来源无效")
	}
	switch q.State {
	case "", "active", "waiting", "completed", "attention", "closed":
	default:
		return TaskBoardView{}, errors.New("任务状态无效")
	}
	q.Search = strings.TrimSpace(q.Search)
	if utf8.RuneCountInString(q.Search) > 200 {
		return TaskBoardView{}, errors.New("搜索内容不能超过200字")
	}
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.PageSize > 50 {
		q.PageSize = 50
	}
	if q.Page > 1_000_000 {
		q.Page = 1_000_000
	}
	out := TaskBoardView{Items: []TaskBoardItem{}, Counts: map[string]int{"all": 0, "active": 0, "waiting": 0, "completed": 0, "attention": 0, "closed": 0}, Page: q.Page, PageSize: q.PageSize}
	err := taskBoardRead(ctx, dir, func(ctx context.Context, tx *sql.Tx, tables map[string]bool) error {
		base, warning, err := taskBoardQuery(ctx, tx, tables)
		if err != nil {
			return err
		}
		out.Warning = warning
		if base == "" {
			out.Page = 1
			return nil
		}
		where, args := " WHERE 1=1", []any{}
		if q.Source != "" {
			where += " AND source=?"
			args = append(args, q.Source)
		}
		if q.Search != "" {
			// instr 按字面搜索，用户输入的 % 和 _ 没有 SQL 通配含义。
			where += " AND (instr(lower(search_text||' '||id||' '||thread_id||' '||connection),lower(?))>0"
			args = append(args, q.Search)

			where += ")"
		}
		rows, err := tx.QueryContext(ctx, base+"SELECT state,count(*) FROM board"+where+" GROUP BY state", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var state string
			var count int
			if err = rows.Scan(&state, &count); err != nil {
				rows.Close()
				return err
			}
			out.Counts[state] = count
			out.Counts["all"] += count
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		out.Total = out.Counts["all"]
		if q.State != "" {
			where += " AND state=?"
			args = append(args, q.State)
			out.Total = out.Counts[q.State]
		}
		last := 1
		if out.Total > 0 {
			last = (out.Total-1)/out.PageSize + 1
		}
		if out.Page > last {
			out.Page = last
		}
		args = append(args, out.PageSize, (out.Page-1)*out.PageSize)
		rows, err = tx.QueryContext(ctx, base+"SELECT "+taskBoardFields+" FROM board"+where+" ORDER BY updated_at DESC,received_at DESC,source,id DESC LIMIT ? OFFSET ?", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, e := scanTaskBoardItem(rows)
			if e != nil {
				return e
			}
			out.Items = append(out.Items, item)
		}
		return rows.Err()
	})
	if err != nil {
		return TaskBoardView{}, err
	}
	if out.Total == 0 {
		out.Page = 1
	}
	out.CapturedAt = time.Now().Unix()
	return out, nil
}

func TaskBoardDetailSnapshot(ctx context.Context, dir, source, id string) (TaskBoardDetail, error) {
	if source != "session" || strings.TrimSpace(id) == "" || len(id) > 1024 {
		return TaskBoardDetail{}, errors.New("任务标识无效")
	}
	var out TaskBoardDetail
	err := taskBoardRead(ctx, dir, func(ctx context.Context, tx *sql.Tx, tables map[string]bool) error {
		base, warning, err := taskBoardQuery(ctx, tx, tables)
		if err != nil {
			return err
		}
		if base == "" {
			return sql.ErrNoRows
		}
		out.Warning = warning
		out.Item, err = scanTaskBoardItem(tx.QueryRowContext(ctx, base+"SELECT "+taskBoardFields+" FROM board WHERE source=? AND id=?", source, id))
		if err != nil {
			return err
		}
		if tables["request_results"] {
			out.Result, err = historyPageResult(ctx, tx, id, tables)
		}
		return err
	})
	if err != nil {
		return TaskBoardDetail{}, err
	}
	if out.Item.ID == "" {
		return TaskBoardDetail{}, errors.New("任务不存在或已清理")
	}
	return out, nil
}
