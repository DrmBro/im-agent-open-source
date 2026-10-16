package store

import (
	"database/sql"
	"errors"
	"time"
)

type Transfer struct {
	ID         string `json:"id"`
	ThreadID   string `json:"thread_id"`
	Direction  string `json:"direction"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Path       string `json:"-"`
	State      string `json:"state"`
	Error      string `json:"error"`
	PlatformID string `json:"platform_id"`
	ContentSHA string `json:"-"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	UpdatedAt  int64  `json:"updated_at"`
}

func (s *Store) migrateAttachments() error {
	_, e := s.db.Exec(`CREATE TABLE IF NOT EXISTS attachment_transfers (
 id TEXT PRIMARY KEY,thread_id TEXT NOT NULL REFERENCES sessions(thread_id),direction TEXT NOT NULL,kind TEXT NOT NULL,
 name TEXT NOT NULL,path TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,error TEXT NOT NULL DEFAULT '',platform_id TEXT NOT NULL DEFAULT '',updated_at INTEGER NOT NULL
 )`)
	if e != nil {
		return e
	}
	rows, e := s.db.Query("PRAGMA table_info(attachment_transfers)")
	if e != nil {
		return e
	}
	columns := map[string]bool{}
	for rows.Next() {
		var id, required, pk int
		var name, kind string
		var def any
		if e = rows.Scan(&id, &name, &kind, &required, &def, &pk); e != nil {
			rows.Close()
			return e
		}
		columns[name] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, c := range []struct{ name, definition string }{{"content_sha256", "TEXT NOT NULL DEFAULT ''"}, {"size_bytes", "INTEGER NOT NULL DEFAULT 0"}} {
		if !columns[c.name] {
			if _, e = s.db.Exec("ALTER TABLE attachment_transfers ADD COLUMN " + c.name + " " + c.definition); e != nil {
				return e
			}
		}
	}
	// 入站下载是只读操作，中断可核对原子落盘结果后安全恢复；出站仍不可重复发送。
	if _, e = s.db.Exec("UPDATE attachment_transfers SET state='interrupted',error='入站下载中断，按原资源身份核对后恢复，不重复提交模型' WHERE direction='in' AND state='downloading'"); e != nil {
		return e
	}
	_, e = s.db.Exec("UPDATE attachment_transfers SET state='unknown',error='进程在文件发送期间退出，需人工核对；未自动重发' WHERE direction='out' AND state='sending'")
	return e
}
func (s *Store) BeginTransfer(t Transfer) (bool, error) {
	if t.ID == "" || len(t.ID) > 256 || (t.Direction != "in" && t.Direction != "out") {
		return false, errors.New("附件任务无效")
	}
	state := "sending"
	if t.Direction == "in" {
		state = "downloading"
	}
	r, e := s.db.Exec("INSERT INTO attachment_transfers(id,thread_id,direction,kind,name,state,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", t.ID, t.ThreadID, t.Direction, t.Kind, t.Name, state, time.Now().Unix())
	if e != nil {
		return false, e
	}
	n, e := r.RowsAffected()
	return n == 1, e
}
func (s *Store) FinishTransfer(id, state, path, platform, hint string) error {
	if state != "received" && state != "sent" && state != "failed" && state != "unknown" {
		return errors.New("附件状态无效")
	}
	_, e := s.db.Exec("UPDATE attachment_transfers SET state=?,path=?,platform_id=?,error=?,updated_at=? WHERE id=? AND state IN ('sending','downloading')", state, path, platform, hint, time.Now().Unix(), id)
	return e
}
func transfersSnapshot(db *sql.DB) ([]Transfer, error) {
	var n int
	if e := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='attachment_transfers'").Scan(&n); e != nil {
		return nil, e
	}
	if n == 0 {
		return []Transfer{}, nil
	}
	rows, e := db.Query("SELECT id,thread_id,direction,kind,name,state,error,platform_id,updated_at FROM attachment_transfers ORDER BY updated_at DESC,id LIMIT 100")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Transfer{}
	for rows.Next() {
		var t Transfer
		if e = rows.Scan(&t.ID, &t.ThreadID, &t.Direction, &t.Kind, &t.Name, &t.State, &t.Error, &t.PlatformID, &t.UpdatedAt); e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// 入站下载的幂等领取；完整收到的文件只返回原记录，失败只读下载可重新尝试。
func (s *Store) BeginInboundTransfer(t Transfer) (Transfer, bool, error) {
	if t.Direction != "in" || t.ID == "" || t.ThreadID == "" || (t.Kind != "image" && t.Kind != "file") {
		return t, false, errors.New("入站附件身份无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return t, false, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO attachment_transfers(id,thread_id,direction,kind,name,state,updated_at)
VALUES (?,?,'in',?,?,'downloading',?) ON CONFLICT(id) DO NOTHING`, t.ID, t.ThreadID, t.Kind, t.Name, time.Now().Unix())
	if err != nil {
		return t, false, err
	}
	var old Transfer
	err = tx.QueryRow(`SELECT id,thread_id,direction,kind,name,path,state,content_sha256,size_bytes FROM attachment_transfers WHERE id=?`, t.ID).Scan(&old.ID, &old.ThreadID, &old.Direction, &old.Kind, &old.Name, &old.Path, &old.State, &old.ContentSHA, &old.SizeBytes)
	if err != nil {
		return t, false, err
	}
	if old.ThreadID != t.ThreadID || old.Direction != "in" || old.Kind != t.Kind || old.Name != t.Name {
		return t, false, errors.New("同一附件ID已有不同来源元数据，拒绝复用")
	}
	if old.State == "received" {
		return old, false, tx.Commit()
	}
	if old.State != "downloading" && old.State != "failed" && old.State != "interrupted" && old.State != "unknown" {
		return t, false, errors.New("入站附件状态不允许恢复")
	}
	if old.State != "downloading" {
		if _, err = tx.Exec("UPDATE attachment_transfers SET state='downloading',error='',updated_at=? WHERE id=? AND direction='in' AND state=?", time.Now().Unix(), t.ID, old.State); err != nil {
			return t, false, err
		}
	}
	return old, true, tx.Commit()
}

func (s *Store) FinishInboundTransfer(t Transfer, path, hash string, size int64) error {
	if path == "" || len(hash) != 64 || size <= 0 {
		return errors.New("附件落盘完整性证明无效")
	}
	result, err := s.db.Exec(`UPDATE attachment_transfers SET state='received',path=?,content_sha256=?,size_bytes=?,error='',updated_at=?
WHERE id=? AND thread_id=? AND direction='in' AND kind=? AND name=? AND state IN ('downloading','received')`, path, hash, size, time.Now().Unix(), t.ID, t.ThreadID, t.Kind, t.Name)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("入站附件落盘账本未确认，未提交模型")
	}
	return nil
}
