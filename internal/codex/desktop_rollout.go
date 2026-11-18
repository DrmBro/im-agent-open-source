package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 桌面协议提交后的只读轮次观察与通知；不负责提交或操作输入框。
func (c *Client) watchDesktopTurn(ctx context.Context, threadID, turnID, input string, rolloutPath string, rolloutOffset int64, actualTurn ...string) {
	defer func() { c.desktopTurnsMu.Lock(); delete(c.desktopTurns, threadID); c.desktopTurnsMu.Unlock() }()
	if len(actualTurn) == 1 && actualTurn[0] != "" && c.observeProtocolTurn(ctx, threadID, turnID, actualTurn[0]) {
		return
	}
	if rolloutPath == "" {
		c.sendDesktopObservationStopped(threadID, turnID)
		return
	}
	if !readDesktopRolloutUntil(ctx, rolloutPath, rolloutOffset, turnID, input, c, threadID, actualTurn...) && ctx.Err() != context.Canceled {
		c.sendDesktopObservationStopped(threadID, turnID)
	}
}

func findDesktopRollout(home, threadID string) (string, int64) {
	if home == "" {
		if h, e := os.UserHomeDir(); e == nil {
			home = filepath.Join(h, ".codex")
		}
	}
	root := filepath.Join(home, "sessions")
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") || !strings.Contains(filepath.Base(path), threadID) {
			return nil
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		raw, readErr := readRolloutLine(bufio.NewReader(f))
		f.Close()
		if readErr != nil {
			return nil
		}
		var meta struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(raw, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != threadID {
			return nil
		}
		if found == "" || filepath.Base(path) > filepath.Base(found) {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", 0
	}
	info, e := os.Stat(found)
	if e != nil {
		return "", 0
	}
	return found, info.Size()
}

// 只接受提交后真实用户条目所属轮次的完成事件，不能拿提交前正在执行的轮次作为回执。
func readDesktopRolloutUntil(ctx context.Context, path string, offset int64, turnID, expected string, c *Client, threadID string, actualTurn ...string) bool {
	currentTurn, matchedTurn, final, finalID := "", "", "", ""
	unphasedCandidate := false
	unphasedFingerprints := []string{}
	observedTurn := false
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		f, err := os.Open(path)
		if err != nil {
			return false
		}
		info, statErr := f.Stat()
		if statErr != nil || info.Size() < offset {
			// 桌面日志回退后旧观察身份不可再安全关联，结束本地等待但不重发。
			f.Close()
			return false
		}
		if _, err = f.Seek(offset, 0); err != nil {
			f.Close()
			return false
		}
		reader := bufio.NewReaderSize(f, 64<<10)
		for {
			raw, err := readRolloutLine(reader)
			if err == io.EOF {
				break // 尾部尚未写完，等待完整记录。
			}
			if err != nil {
				// 确定的读取错误不能在同一位置无限重试；不猜测轮次，也不重发。
				f.Close()
				c.sendDesktopObservationStopped(threadID, turnID)
				return true
			}
			offset += int64(len(raw))
			var r struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if json.Unmarshal(raw, &r) != nil {
				continue
			}
			if r.Type == "event_msg" {
				var ev struct {
					Type   string          `json:"type"`
					TurnID string          `json:"turn_id"`
					Last   string          `json:"last_agent_message"`
					Error  json.RawMessage `json:"error"`
				}
				if json.Unmarshal(r.Payload, &ev) != nil {
					continue
				}
				if ev.Type == "task_started" {
					currentTurn = ev.TurnID
					observedTurn = ev.TurnID != ""
				}
				if matchedTurn == "" && observedTurn && ev.TurnID == currentTurn &&
					(ev.Type == "task_complete" || ev.Type == "turn_aborted") {
					// 桌面轮次已结束却无法关联本次输入：结果未知而非继续占住本地队列。
					// 不猜测回复归属，也不重放模型输入。
					f.Close()
					c.sendDesktopObservationStopped(threadID, turnID)
					return true
				}
				if matchedTurn != "" && ev.TurnID == matchedTurn {
					if ev.Type == "turn_aborted" {
						f.Close()
						c.sendDesktopTurnFailure(threadID, turnID, "桌面本轮已中断")
						return true
					}
					if ev.Type == "task_complete" {
						matches := 0
						if unphasedCandidate {
							for _, fingerprint := range unphasedFingerprints {
								if fingerprint == storeHash(ev.Last) {
									matches++
								}
							}
						}
						if unphasedCandidate && (matches != 1 || ev.Last == "" || ev.Last != final) {
							f.Close()
							c.sendDesktopObservationStopped(threadID, turnID)
							return true
						}
						if hasCompletionError(ev.Error) {
							f.Close()
							c.sendDesktopTurnFailure(threadID, turnID, "桌面本轮响应中断或返回错误，可能已有部分操作完成")
							return true
						}
						if ev.Last != "" {
							if finalID != "" && final != ev.Last {
								f.Close()
								c.sendDesktopObservationStopped(threadID, turnID)
								return true
							}
							final = ev.Last
						}
						f.Close()
						if strings.TrimSpace(final) == "" {
							c.sendDesktopTurnFailure(threadID, turnID, "匹配的桌面轮次没有最终文本")
							return true
						}
						if finalID == "" {
							c.sendDesktopObservationStopped(threadID, turnID)
							return true
						}
						c.subMu.Lock()
						ch := c.subs[threadID]
						if ch != nil {
							item := map[string]any{"threadId": threadID, "turnId": turnID, "item": map[string]any{"id": finalID, "type": "agentMessage", "phase": "final_answer", "text": final}}
							complete := map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "completed"}}
							select {
							case ch <- Notice{"item/completed", mustJSON(item)}:
							default:
							}
							select {
							case ch <- Notice{"turn/completed", mustJSON(complete)}:
							default:
							}
						}
						c.subMu.Unlock()
						return true
					}
				}
			}
			if r.Type == "response_item" {
				var item struct {
					ID      string         `json:"id"`
					Type    string         `json:"type"`
					Role    string         `json:"role"`
					Phase   string         `json:"phase"`
					Status  string         `json:"status"`
					Content []InputContent `json:"content"`
					Meta    struct {
						TurnID string `json:"turn_id"`
					} `json:"internal_chat_message_metadata_passthrough"`
				}
				if json.Unmarshal(r.Payload, &item) != nil || item.Type != "message" {
					continue
				}
				parts := []string{}
				for _, p := range item.Content {
					if p.Type == "input_text" || p.Type == "output_text" || p.Type == "text" {
						parts = append(parts, p.Text)
					}
				}
				text := strings.Join(parts, "\n")
				if matchedTurn == "" && item.Role == "user" && desktopInputMatches(text, expected) {
					actual := item.Meta.TurnID
					if actual == "" {
						actual = currentTurn
					}
					if actual != "" && (len(actualTurn) == 0 || actualTurn[0] == actual) {
						matchedTurn = actual
					}
				}
				if matchedTurn != "" && currentTurn == matchedTurn && item.Role == "assistant" && completedPublicReply(item.Phase, item.Status) && (item.Phase != "" || item.Meta.TurnID == matchedTurn) {
					unphasedCandidate = item.Phase == ""
					if unphasedCandidate {
						if len(unphasedFingerprints) >= 512 {
							f.Close()
							c.sendDesktopObservationStopped(threadID, turnID)
							return true
						}
						unphasedFingerprints = append(unphasedFingerprints, storeHash(text))
					}
					finalID = item.ID
					final = text
				}
			}
		}
		f.Close()
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func storeHash(s string) string      { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func mustJSON(v any) json.RawMessage { raw, _ := json.Marshal(v); return raw }

func (c *Client) sendDesktopTurnFailure(threadID, turnID, message string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	ch := c.subs[threadID]
	if ch == nil {
		return
	}
	params := mustJSON(map[string]any{"threadId": threadID, "turnId": turnID, "message": message})
	select {
	case ch <- Notice{"turn/failed", params}:
	default:
	}
}

// 观察结束不同于真实 turn/failed，不向上层伪造桌面执行失败。
func (c *Client) sendDesktopObservationStopped(threadID, turnID string) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	if ch := c.subs[threadID]; ch != nil {
		params := mustJSON(map[string]any{"threadId": threadID, "turnId": turnID})
		select {
		case ch <- Notice{"bridge/desktopObservationStopped", params}:
		default:
		}
	}
}
