// Package sqlitedb 在 SQLite 事务失败时清理并淘汰连接，避免锁随连接池复用长期残留。
package sqlitedb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"path/filepath"

	"modernc.org/sqlite"
)

// Open 的连接参数在每次重连时执行；只在初始化时执行 PRAGMA 会丢失这些约束。
// path 必须是本机数据库文件路径，目录与文件权限由调用方管理。
func Open(path string, pragmas ...string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: url.Values{"_pragma": pragmas}.Encode()}
	d := &guardDriver{Driver: &sqlite.Driver{}, failures: &failureLog{path: abs + ".errors.jsonl"}}
	return sql.OpenDB(&connector{driver: d, dsn: u.String()}), nil
}

type connector struct {
	driver *guardDriver
	dsn    string
}

func (c *connector) Driver() driver.Driver { return c.driver }
func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// 保留当前驱动的上下文取消、查询、预编译、健康检查和连接复用接口。
type sqliteConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type guardDriver struct {
	driver.Driver
	failures *failureLog
}

func (d *guardDriver) Open(name string) (driver.Conn, error) {
	c, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	native, ok := c.(sqliteConn)
	if !ok {
		_ = c.Close()
		return nil, errors.New("SQLite 驱动缺少事务清理所需接口")
	}
	return &guardConn{sqliteConn: native, failures: d.failures}, nil
}

type guardConn struct {
	sqliteConn
	failures *failureLog
	unusable bool // database/sql 串行使用同一驱动连接。
}

var (
	_ driver.Connector = (*connector)(nil)
	_ driver.Driver    = (*guardDriver)(nil)
	_ sqliteConn       = (*guardConn)(nil)
	_ driver.Tx        = (*guardTx)(nil)
)

func (c *guardConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *guardConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.unusable {
		return nil, driver.ErrBadConn
	}
	tx, err := c.sqliteConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &guardTx{Tx: tx, conn: c}, nil
}

func (c *guardConn) IsValid() bool {
	return !c.unusable && c.sqliteConn.IsValid()
}

func (c *guardConn) ResetSession(ctx context.Context) error {
	if c.unusable {
		return driver.ErrBadConn
	}
	return c.sqliteConn.ResetSession(ctx)
}

type guardTx struct {
	driver.Tx
	conn *guardConn
}

func (t *guardTx) Commit() error {
	err := t.Tx.Commit()
	if err != nil {
		// database/sql 已把 sql.Tx 标为结束，调用方 defer tx.Rollback() 不再到达驱动。
		// SQLite 的 COMMIT 遇到 BUSY 却可能保留事务，必须在归还连接之前直接回滚。
		rollbackErr := t.Tx.Rollback()
		t.conn.unusable = true
		t.conn.failures.record("commit", err, rollbackErr)
	}
	// 保留原错误，不返回 ErrBadConn 触发业务重试；IsValid 负责让池关闭此连接。
	return err
}

func (t *guardTx) Rollback() error {
	err := t.Tx.Rollback()
	if err != nil {
		t.conn.unusable = true
		t.conn.failures.record("rollback", err, err)
	}
	return err
}
