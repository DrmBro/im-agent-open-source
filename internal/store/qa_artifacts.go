package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// QAArtifact 仅保存本机文档索引，不保存或读取文档正文。
type QAArtifact struct {
	ID           string `json:"id"`
	QuestionID   string `json:"question_id"`
	EventID      string `json:"event_id"`
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	RelativePath string `json:"relative_path"`
	SHA256       string `json:"sha256"`
	CreatedAt    int64  `json:"created_at"`
}

func (s *Store) migrateQAArtifacts() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS qa_artifacts(
 id TEXT PRIMARY KEY,question_id TEXT NOT NULL REFERENCES qa_questions(id),
 event_id TEXT NOT NULL REFERENCES qa_turns(id),kind TEXT NOT NULL,title TEXT NOT NULL,
 relative_path TEXT NOT NULL,sha256 TEXT NOT NULL,created_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS qa_artifacts_recent ON qa_artifacts(created_at,id);
 CREATE INDEX IF NOT EXISTS qa_artifacts_question ON qa_artifacts(question_id,created_at);`)
	return err
}

const qaArtifactColumns = "id,question_id,event_id,kind,title,relative_path,sha256,created_at"

func scanQAArtifact(row interface{ Scan(...any) error }) (QAArtifact, error) {
	var a QAArtifact
	err := row.Scan(&a.ID, &a.QuestionID, &a.EventID, &a.Kind, &a.Title, &a.RelativePath, &a.SHA256, &a.CreatedAt)
	return a, err
}

// QAPutArtifact 由调用者先落盘，再保存不可变索引；相同标识只接受同一内容。
func (s *Store) QAPutArtifact(a QAArtifact) error {
	if strings.TrimSpace(a.ID) == "" || a.QuestionID == "" || a.EventID == "" || strings.TrimSpace(a.Kind) == "" || strings.TrimSpace(a.Title) == "" {
		return errors.New("问答文档索引信息不完整")
	}
	if a.RelativePath == "" || strings.ContainsAny(a.RelativePath, "\\\x00") || path.IsAbs(a.RelativePath) || filepath.IsAbs(a.RelativePath) ||
		path.Clean(a.RelativePath) != a.RelativePath || a.RelativePath == "." || a.RelativePath == ".." || strings.HasPrefix(a.RelativePath, "../") {
		return errors.New("问答文档索引必须使用规范的相对路径")
	}
	digest, err := hex.DecodeString(a.SHA256)
	if err != nil || len(digest) != 32 {
		return errors.New("问答文档 SHA256 无效")
	}
	a.SHA256 = strings.ToLower(a.SHA256)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var qid string
	if err = tx.QueryRow("SELECT question_id FROM qa_turns WHERE id=?", a.EventID).Scan(&qid); err != nil {
		return err
	}
	if qid != a.QuestionID {
		return errors.New("文档所属问答与轮次不匹配")
	}
	existing, err := scanQAArtifact(tx.QueryRow("SELECT "+qaArtifactColumns+" FROM qa_artifacts WHERE id=?", a.ID))
	if err == nil {
		if existing.QuestionID != a.QuestionID || existing.EventID != a.EventID || existing.Kind != a.Kind ||
			existing.Title != a.Title || existing.RelativePath != a.RelativePath || existing.SHA256 != a.SHA256 ||
			(a.CreatedAt != 0 && existing.CreatedAt != a.CreatedAt) {
			return errors.New("文档标识已指向不同内容，不能覆盖索引")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	if a.CreatedAt < 0 {
		return errors.New("文档创建时间无效")
	}
	if _, err = tx.Exec("INSERT INTO qa_artifacts("+qaArtifactColumns+") VALUES(?,?,?,?,?,?,?,?)", a.ID, a.QuestionID, a.EventID, a.Kind, a.Title, a.RelativePath, a.SHA256, a.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// qaOpenSnapshot 以只读模式打开既有状态库，不建库、不迁移，也不读取文档文件。
func qaOpenSnapshot(dir string) (*sql.DB, error) {
	filename := filepath.Join(dir, "sessions.db")
	st, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("问答数据库不可读取")
	}
	u := url.URL{Scheme: "file", Path: filename, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func qaHasTable(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var exists int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&exists)
	return exists != 0, err
}

// QAArtifactSnapshot 只查询索引；找不到数据库、表或指定索引时返回 sql.ErrNoRows。
func QAArtifactSnapshot(ctx context.Context, dir, id string) (QAArtifact, error) {
	db, err := qaOpenSnapshot(dir)
	if os.IsNotExist(err) {
		return QAArtifact{}, sql.ErrNoRows
	}
	if err != nil {
		return QAArtifact{}, err
	}
	defer db.Close()
	exists, err := qaHasTable(ctx, db, "qa_artifacts")
	if err != nil {
		return QAArtifact{}, err
	}
	if !exists {
		return QAArtifact{}, sql.ErrNoRows
	}
	return scanQAArtifact(db.QueryRowContext(ctx, "SELECT "+qaArtifactColumns+" FROM qa_artifacts WHERE id=?", id))
}
