package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// 从原异常记录的精确位置核验完成事件，不改变实时消费游标。
func (c *Client) RecoverLiveCompletion(ctx context.Context, thread, path string, offset, length int64, fingerprint string) ([]LiveMessage, error) {
	if offset <= 0 || length <= 0 || length > 32<<20 || fingerprint == "" {
		return nil, &CompletionEvidenceError{"补齐完成记录的位置无效"}
	}
	file, header, err := c.openLiveRollout(thread, path)
	if os.IsNotExist(err) {
		// 原日志移动到归档目录时，以同一文件名、会话元数据和原始行指纹核验位置。
		files, scanErr := c.LiveRolloutFiles(ctx, thread)
		if scanErr != nil {
			return nil, scanErr
		}
		for _, candidate := range files {
			if filepath.Base(candidate) == filepath.Base(path) {
				file, header, err = c.openLiveRollout(thread, candidate)
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if offset < header {
		return nil, &CompletionEvidenceError{"补齐完成记录早于会话元数据"}
	}
	raw := make([]byte, length)
	if _, err = file.ReadAt(raw, offset); err != nil {
		return nil, err
	}
	if storeHash(string(raw)) != fingerprint {
		return nil, &CompletionEvidenceError{"原完成记录已变化，未猜测补发"}
	}
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Type   string          `json:"type"`
			TurnID string          `json:"turn_id"`
			Last   string          `json:"last_agent_message"`
			Error  json.RawMessage `json:"error"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &record) != nil || record.Type != "event_msg" || record.Payload.Type != "task_complete" || hasCompletionError(record.Payload.Error) {
		return nil, &CompletionEvidenceError{"原记录不是成功完成事件"}
	}
	replies, checkErr := finalRepliesAtCompletion(ctx, file, header, offset, record.Payload.TurnID, record.Payload.Last)
	if checkErr != nil {
		var pathErr *os.PathError
		if ctx.Err() != nil || errors.Is(checkErr, io.EOF) || errors.Is(checkErr, io.ErrUnexpectedEOF) || errors.As(checkErr, &pathErr) {
			return nil, checkErr
		}
		return nil, &CompletionEvidenceError{checkErr.Error()}
	}
	return replies, nil
}

// 证据不满足条件与临时读取失败分开记录；临时失败留待下一轮重新读取。
type CompletionEvidenceError struct{ Detail string }

func (e *CompletionEvidenceError) Error() string { return e.Detail }
func CompletionEvidenceRejected(err error) bool {
	var evidence *CompletionEvidenceError
	return errors.As(err, &evidence)
}
