package qa

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"im-agent/internal/store"
)

func validDocumentComponent(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func validDocumentFilename(name string) bool {
	return strings.HasSuffix(name, ".md") && validDocumentComponent(strings.TrimSuffix(name, ".md"))
}
func (r *Runner) saveDocument(t store.QATurn, kind, title, document string) (store.QAArtifact, error) {
	if e := validateDocument(kind, document); e != nil {
		return store.QAArtifact{}, e
	}
	if !validDocumentComponent(t.ID) || !validDocumentComponent(t.QuestionID) {
		return store.QAArtifact{}, errors.New("文档任务标识无效")
	}
	directory, e := openDocumentDirectory(r.cfg.StateDir, t.QuestionID, true)
	if e != nil {
		return store.QAArtifact{}, e
	}
	defer directory.Close()
	content := []byte(strings.TrimSpace(document) + "\n")
	digest := sha256.Sum256(content)
	if e = publishDocumentAt(directory, t.ID+".md", content); e != nil {
		return store.QAArtifact{}, e
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "业务文档"
	}
	if runes := []rune(title); len(runes) > 120 {
		title = string(runes[:120])
	}
	relative := filepath.ToSlash(filepath.Join("qa-documents", t.QuestionID, t.ID+".md"))
	artifact := store.QAArtifact{ID: store.Hash(t.ID + ":document"), QuestionID: t.QuestionID, EventID: t.ID, Kind: kind, Title: title, RelativePath: relative, SHA256: hex.EncodeToString(digest[:]), CreatedAt: time.Now().Unix()}
	if e = r.db.QAPutArtifact(artifact); e != nil {
		return store.QAArtifact{}, errors.New("文档已写入，但索引保存失败，请在本机核对")
	}
	return artifact, nil
}
func ReadArtifact(stateDir string, artifact store.QAArtifact) (string, error) {
	if !validDocumentComponent(artifact.QuestionID) || !validDocumentComponent(artifact.EventID) {
		return "", errors.New("文档索引标识无效")
	}
	expected := filepath.ToSlash(filepath.Join("qa-documents", artifact.QuestionID, artifact.EventID+".md"))
	if artifact.RelativePath != expected {
		return "", errors.New("文档索引不在预期存储范围")
	}
	directory, e := openDocumentDirectory(stateDir, artifact.QuestionID, false)
	if e != nil {
		return "", e
	}
	defer directory.Close()
	file, e := openSecureFileAt(directory, artifact.EventID+".md")
	if e != nil {
		return "", e
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil || info.Size() > 256<<10 {
		return "", errors.New("文档大小无效")
	}
	content, e := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if e != nil || len(content) > 256<<10 {
		return "", errors.New("业务文档读取失败")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return "", errors.New("业务文档内容与索引不一致，未返回正文")
	}
	return string(content), nil
}
