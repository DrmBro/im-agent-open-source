package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"im-agent/internal/diagnostics"
)

type LiveMessage struct {
	HistoryMessage
	Offset int64
	Issue  *LiveReadIssue
}

// LiveReadIssue 只记录不可解析原始记录的位置和摘要，不把可能敏感的正文写入诊断。
type LiveReadIssue struct {
	Kind, Fingerprint string
	Length            int64
}

func liveIssue(kind string, raw []byte, offset int64) LiveMessage {
	sum := sha256.Sum256(raw)
	return LiveMessage{Offset: offset, Issue: &LiveReadIssue{Kind: kind, Fingerprint: hex.EncodeToString(sum[:]), Length: int64(len(raw))}}
}

// 超限完整行仅记录长度和摘要，不保留正文；若仍在写入则等待换行，不跳过半行。
func oversizedLiveIssue(ctx context.Context, file *os.File, offset int64, tailOut ...*[]byte) (LiveMessage, int64, error) {
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return LiveMessage{}, offset, err
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	hash := sha256.New()
	var length int64
	var tail []byte
	for {
		if err := ctx.Err(); err != nil {
			return LiveMessage{}, offset, err
		}
		chunk, err := reader.ReadSlice('\n')
		length += int64(len(chunk))
		tail = appendCheckpointTail(tail, chunk)
		if length > 512<<20 {
			return LiveMessage{}, offset, errors.New("单条日志超过隔离上限，需人工核对原始日志")
		}
		if _, writeErr := hash.Write(chunk); writeErr != nil {
			return LiveMessage{}, offset, writeErr
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return LiveMessage{}, offset, err
		}
		if len(tailOut) > 0 {
			*tailOut[0] = tail
		}
		return LiveMessage{Offset: offset, Issue: &LiveReadIssue{Kind: "oversized_record",
			Fingerprint: hex.EncodeToString(hash.Sum(nil)), Length: length}}, offset + length, nil
	}
}

// 定位游标所在完整记录的真实起点；增量投递要求它与原游标一致，
// 边界变化时保留进度待核对，不回放首次启用前未入账的历史。
func alignLiveOffset(f *os.File, header, offset int64) (int64, error) {
	if offset == header {
		return offset, nil
	}
	var previous [1]byte
	if _, err := f.ReadAt(previous[:], offset-1); err != nil {
		return offset, err
	}
	if previous[0] == '\n' {
		return offset, nil
	}
	start := offset - (32 << 20)
	if start < header {
		start = header
	}
	buf := make([]byte, offset-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return offset, err
	}
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		start += int64(i) + 1
	} else if start != header {
		return offset, errors.New("日志游标所在记录超过单条上限，未猜测边界")
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return offset, err
	}
	raw, err := readRolloutLine(bufio.NewReaderSize(f, 64<<10))
	if err == io.EOF {
		return offset, nil // 日志仍在写入；等待完整记录，不重置游标。
	}
	if err != nil {
		return offset, err
	}
	if len(raw) == 0 {
		return offset, errors.New("日志游标所在记录为空")
	}
	return start, nil
}

func (c *Client) openLiveRollout(thread, path string) (*os.File, int64, error) {
	home := c.runtimeConfig.Codex.Home
	if home == "" {
		root, e := os.UserHomeDir()
		if e != nil {
			return nil, 0, e
		}
		home = filepath.Join(root, ".codex")
	}
	allowed := false
	for _, folder := range []string{"sessions", "archived_sessions"} {
		rel, relErr := filepath.Rel(filepath.Join(home, folder), path)
		if relErr == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, 0, errors.New("实时日志路径不在会话目录内")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	raw, err := readRolloutLine(bufio.NewReader(f))
	if err != nil {
		f.Close()
		return nil, 0, errors.New("实时日志元数据尚未完整写入")
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != thread {
		f.Close()
		return nil, 0, errors.New("实时日志与目标会话标识不一致")
	}
	return f, int64(len(raw)), nil
}

// 首次启用只从最后完整行开始，不把旧历史作为新消息刷到 IM。
func (c *Client) LiveBaseline(ctx context.Context, thread string) (string, int64, error) {
	if ctx.Err() != nil {
		return "", 0, ctx.Err()
	}
	path, _ := findDesktopRollout(c.runtimeConfig.Codex.Home, thread)
	if path == "" {
		return "", 0, errors.New("尚未找到该会话的实时日志")
	}
	f, header, err := c.openLiveRollout(thread, path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	end := info.Size()
	// 最多读取一个受支持的最大记录；尾部半行不能成为下一次起始位置。
	for end > header {
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		start := end - 65536
		if start < header {
			start = header
		}
		buf := make([]byte, end-start)
		if _, err = f.ReadAt(buf, start); err != nil && err != io.EOF {
			return "", 0, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return path, start + int64(i) + 1, nil
		}
		end = start
		if info.Size()-end > 32<<20 {
			return "", 0, errors.New("实时日志尾部记录过大，未初始化游标")
		}
	}
	return path, header, nil
}

// 持久化偏移由上层在消息入队的同一事务中推进；这里只读，不写 Codex 文件。
func (c *Client) ReadLiveMessages(ctx context.Context, thread, path string, offset int64) ([]LiveMessage, int64, error) {
	return c.readLiveMessages(ctx, thread, path, offset, "", nil)
}

// 检查点来自本次实际消费的原始字节；在同一个文件句柄内核对读取前后边界。
func (c *Client) ReadLiveMessagesChecked(ctx context.Context, thread, path string, offset int64, expected string) ([]LiveMessage, int64, string, error) {
	var checkpoint string
	messages, next, err := c.readLiveMessages(ctx, thread, path, offset, expected, &checkpoint)
	return messages, next, checkpoint, err
}

func (c *Client) readLiveMessages(ctx context.Context, thread, path string, offset int64, expected string, checkpoint *string) ([]LiveMessage, int64, error) {
	f, header, err := c.openLiveRollout(thread, path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if offset < header {
		return nil, offset, errors.New("实时日志游标早于会话元数据，未猜测位置")
	}
	if offset > info.Size() {
		return nil, offset, diagnostics.UserError("日志已截短，原同步边界需核对；保留游标，未回放启用前历史")
	}
	aligned, err := alignLiveOffset(f, header, offset)
	if err != nil {
		return nil, offset, err
	}
	if aligned != offset {
		return nil, offset, diagnostics.UserError("日志消费边界已改写，保留游标等待核对")
	}
	initialTail, err := readCheckpointTail(f, offset)
	if err != nil {
		return nil, offset, err
	}
	if expected != "" && storeHash(string(initialTail)) != expected {
		return nil, offset, diagnostics.UserError("已读取日志内容发生变化，保留游标等待核对；已有入账消息继续投递")
	}
	tail := append([]byte(nil), initialTail...)
	if _, err = f.Seek(aligned, io.SeekStart); err != nil {
		return nil, offset, err
	}
	reader := bufio.NewReaderSize(f, 64<<10)
	next := aligned
	var out []LiveMessage
	for lines := 0; lines < 1000 && next-aligned < 8<<20 && len(out) < 50; lines++ {
		if ctx.Err() != nil {
			if next == aligned {
				return nil, offset, ctx.Err()
			}
			// 完整记录已读取时保留本批进度；仍经下方文件身份和双边界核验。
			// 完成事件核验超时已记为待补齐，不能每轮丢弃同一批而永久卡住。
			break
		}
		start := next
		raw, err := readRolloutLine(reader)
		if err == io.EOF {
			break
		}
		if errors.Is(err, errRolloutLineTooLarge) {
			issue, end, scanErr := oversizedLiveIssue(ctx, f, start, &tail)
			if scanErr == io.EOF {
				break // 仍在写入的超限半行不能先隔离。
			}
			if scanErr != nil {
				return nil, offset, scanErr
			}
			out = append(out, issue)
			next = end
			if _, seekErr := f.Seek(next, io.SeekStart); seekErr != nil {
				return nil, offset, seekErr
			}
			reader = bufio.NewReaderSize(f, 64<<10)
			continue
		}
		if err != nil {
			return nil, offset, err
		}
		next += int64(len(raw))
		tail = appendCheckpointTail(tail, raw)
		var rec struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &rec) != nil {
			out = append(out, liveIssue("invalid_json", raw, start))
			continue
		}
		if rec.Type == "event_msg" {
			var event struct {
				Type   string          `json:"type"`
				TurnID string          `json:"turn_id"`
				Last   string          `json:"last_agent_message"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(rec.Payload, &event) == nil && event.Type == "task_complete" && !hasCompletionError(event.Error) {
				replies, checkErr := finalRepliesAtCompletion(ctx, f, header, start, event.TurnID, event.Last)
				if checkErr != nil {
					out = append(out, liveIssue("final_reply_unconfirmed", raw, start))
				} else {
					out = append(out, replies...)
				}
			}
			continue
		}
		if rec.Type != "response_item" {
			continue
		}
		message, visible, parseErr := publicUserMessage(rec.Payload)
		if parseErr != nil {
			kind := "invalid_item"
			if errors.Is(parseErr, errMissingPublicItemID) {
				kind = "missing_item_id"
			}
			out = append(out, liveIssue(kind, raw, start))
			continue
		}
		if visible {
			out = append(out, LiveMessage{HistoryMessage: message, Offset: start})
		}
	}
	currentTail, err := readCheckpointTail(f, next)
	if err != nil {
		return nil, offset, err
	}
	oldBoundary, err := readCheckpointTail(f, offset)
	if err != nil {
		return nil, offset, err
	}
	currentInfo, err := os.Stat(path)
	if err != nil {
		return nil, offset, err
	}
	if !os.SameFile(info, currentInfo) || !bytes.Equal(currentTail, tail) || !bytes.Equal(oldBoundary, initialTail) {
		return nil, offset, diagnostics.UserError("日志在读取期间发生改写，本批未入账，等待重新核对")
	}
	if checkpoint != nil {
		*checkpoint = storeHash(string(tail))
	}
	return out, next, nil
}

// 来源匹配只接受原文及已实测的完整下划线转义形式，不做相似度匹配。
func DesktopInputForms(text string) (string, string) {
	original := strings.TrimSpace(text)
	rendered := original
	if !strings.ContainsAny(original, "`\\") {
		rendered = strings.ReplaceAll(original, "_", "\\_")
	}
	return original, rendered
}

func appendCheckpointTail(tail, raw []byte) []byte {
	if len(raw) >= 256 {
		return append(tail[:0], raw[len(raw)-256:]...)
	}
	tail = append(tail, raw...)
	if len(tail) > 256 {
		tail = append(tail[:0], tail[len(tail)-256:]...)
	}
	return tail
}
func readCheckpointTail(file *os.File, offset int64) ([]byte, error) {
	start := offset - 256
	if start < 0 {
		start = 0
	}
	if offset < 0 {
		return nil, errors.New("检查点位置无效")
	}
	raw := make([]byte, offset-start)
	_, err := file.ReadAt(raw, start)
	return raw, err
}
