package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// RecoveryObservation只保存关联标识与读取进度，不保存输入正文或模型思考内容。
// 当前提交层的临时turn引用不能冒充日志实际turn_id，必须通过已匹配用户条目重新关联。
type RecoveryObservation struct {
	UnphasedCandidates []ReplyLocation `json:"unphased_candidates,omitempty"`
	UserItemID         string          `json:"user_item_id"`
	CurrentTurn        string          `json:"current_turn"`
	MatchedTurn        string          `json:"matched_turn"`
	Outcome            string          `json:"outcome"`
	ReplyIDs           []string        `json:"reply_ids"`
	ReplyIssue         string          `json:"reply_issue,omitempty"`
	ReplyLocations     []ReplyLocation `json:"reply_locations,omitempty"`
	FinalCandidate     ReplyLocation   `json:"final_candidate,omitempty"`
	FinalCount         int             `json:"final_count,omitempty"`
	MatchedUserCount   int             `json:"matched_user_count,omitempty"`
	ReplyEvidence      string          `json:"reply_evidence,omitempty"`
}

// ReadRecoveryObservation只读已授权会话日志；没有turn/start、Resume、Interrupt或UI操作。
func (c *Client) ReadRecoveryObservation(ctx context.Context, thread, path string, offset int64, userItemID string, state RecoveryObservation) (RecoveryObservation, int64, error) {
	if userItemID == "" || state.UserItemID != "" && state.UserItemID != userItemID {
		return state, offset, errors.New("恢复观察的用户消息身份无效")
	}
	if state.Outcome != "" && state.Outcome != "completed" && state.Outcome != "failed" || len(state.ReplyIDs) > 512 || len(state.UnphasedCandidates) > 512 {
		return state, offset, errors.New("恢复观察状态无效")
	}
	if state.Outcome != "" && state.MatchedTurn == "" {
		return state, offset, errors.New("恢复观察终态缺少关联轮次")
	}
	state.UserItemID = userItemID
	if state.Outcome != "" {
		return state, offset, nil
	}
	f, header, e := c.openLiveRollout(thread, path)
	if e != nil {
		return state, offset, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return state, offset, e
	}
	if offset < header || offset > info.Size() {
		return state, offset, errors.New("恢复观察日志位置失效，未回退重扫")
	}
	if _, e = f.Seek(offset, io.SeekStart); e != nil {
		return state, offset, e
	}
	reader := bufio.NewReaderSize(f, 64<<10)
	next := offset
	for lines := 0; lines < 1000 && next-offset < 8<<20; lines++ {
		if e = ctx.Err(); e != nil {
			return state, offset, e
		}
		raw, err := readRolloutLine(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			return state, offset, err
		}
		var r struct {
			Timestamp string          `json:"timestamp"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return state, offset, errors.New("恢复观察日志包含无效记录")
		}
		start := next
		next += int64(len(raw))
		switch r.Type {
		case "event_msg":
			var ev struct {
				Type   string          `json:"type"`
				TurnID string          `json:"turn_id"`
				Last   string          `json:"last_agent_message"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(r.Payload, &ev) != nil {
				return state, offset, errors.New("恢复观察轮次事件无效")
			}
			if ev.Type == "task_started" {
				state.CurrentTurn = ev.TurnID
			}
			if ev.TurnID != "" && ev.TurnID == state.MatchedTurn {
				switch ev.Type {
				case "task_complete":
					if hasCompletionError(ev.Error) {
						state.Outcome = "failed"
						return state, next, nil
					}
					if state.ReplyIssue == "turn_conflict" && state.CurrentTurn == state.MatchedTurn &&
						state.MatchedUserCount == 1 && state.FinalCount == 1 && state.FinalCandidate.ItemID != "" &&
						ev.Last != "" && storeHash(ev.Last) == state.FinalCandidate.Fingerprint {
						state.ReplyIDs = []string{state.FinalCandidate.ItemID}
						state.ReplyLocations = []ReplyLocation{state.FinalCandidate}
						state.ReplyIssue = ""
						state.ReplyEvidence = "unique_final_and_completion_text"
					}
					if len(state.ReplyIDs) == 0 && state.ReplyIssue == "" && ev.Last != "" {
						matches := []ReplyLocation{}
						for _, candidate := range state.UnphasedCandidates {
							if candidate.Fingerprint == storeHash(ev.Last) {
								matches = append(matches, candidate)
							}
						}
						if len(matches) == 1 {
							location := matches[0]
							location.CompletionOffset = start
							state.ReplyIDs = []string{location.ItemID}
							state.ReplyLocations = []ReplyLocation{location}
							state.ReplyEvidence = "explicit_turn_and_completion_text_unphased"
						} else if len(matches) > 1 {
							state.ReplyIssue = "ambiguous_unphased"
						}
					}
					state.UnphasedCandidates = nil
					state.Outcome = "completed"
					return state, next, nil
				case "turn_aborted":
					state.Outcome = "failed"
					return state, next, nil
				}
			}
			if (ev.Type == "task_complete" || ev.Type == "turn_aborted") && ev.TurnID == state.CurrentTurn {
				state.CurrentTurn = ""
			}
		case "response_item":
			var item struct {
				ID, Type, Role, Phase, Status string
				Content                       []InputContent
				Meta                          struct {
					TurnID string `json:"turn_id"`
				} `json:"internal_chat_message_metadata_passthrough"`
			}
			if json.Unmarshal(r.Payload, &item) != nil {
				return state, offset, errors.New("恢复观察消息结构无效")
			}
			if item.Type != "message" {
				continue
			}
			actual := item.Meta.TurnID
			if actual == "" {
				actual = state.CurrentTurn
			}
			if item.Role == "user" && state.MatchedTurn != "" && (actual == state.MatchedTurn || state.CurrentTurn == state.MatchedTurn) && item.ID != userItemID {
				return state, offset, errors.New("同一轮次包含其他用户消息，未猜测回复归属")
			}
			if item.Role == "user" && item.ID == userItemID {
				state.MatchedUserCount++
				if actual == "" {
					return state, offset, errors.New("已匹配用户消息缺少可确认的执行轮次")
				}
				if state.MatchedTurn != "" && state.MatchedTurn != actual {
					return state, offset, errors.New("用户消息关联轮次冲突")
				}
				state.MatchedTurn = actual
			}
			if item.Role == "assistant" && item.Phase == "" && item.Status == "completed" && item.ID != "" && state.MatchedTurn != "" && state.CurrentTurn == state.MatchedTurn && item.Meta.TurnID == state.MatchedTurn {
				if text, ok := exactReplyText(item.Content); ok {
					if len(state.UnphasedCandidates) >= 512 {
						return state, offset, errors.New("缺省阶段回复候选超出上限")
					}
					stamp, parseErr := time.Parse(time.RFC3339Nano, r.Timestamp)
					if parseErr == nil && stamp.UnixMilli() > 0 {
						state.UnphasedCandidates = append(state.UnphasedCandidates, ReplyLocation{ItemID: item.ID, Offset: start, Fingerprint: storeHash(text), TurnID: state.MatchedTurn, Timestamp: stamp.UnixMilli()})
					}
				}
			}
			if item.Role == "assistant" && item.Phase == "final_answer" && state.MatchedTurn != "" && state.CurrentTurn == state.MatchedTurn {
				state.FinalCount++
				if text, supported := exactReplyText(item.Content); supported && item.ID != "" {
					state.FinalCandidate = ReplyLocation{ItemID: item.ID, Offset: start, Fingerprint: storeHash(text)}
				}
			}
			if item.Role == "assistant" && item.Phase == "final_answer" && state.MatchedTurn != "" &&
				state.CurrentTurn == state.MatchedTurn && item.Meta.TurnID != "" && item.Meta.TurnID != state.MatchedTurn {
				// 完成事件仍可确认执行，但显式冲突身份不能被时序覆盖，也不能封存部分回复。
				state.ReplyIssue = "turn_conflict"
			}
			if item.Role == "assistant" && item.Phase == "final_answer" && state.MatchedTurn != "" && actual == state.MatchedTurn {
				if item.ID == "" {
					state.ReplyIssue = "missing_id"
					continue
				}
				for _, id := range state.ReplyIDs {
					if id == item.ID {
						state.ReplyIssue = "duplicate_id"
					}
				}
				if len(state.ReplyIDs) >= 512 {
					return state, offset, errors.New("恢复观察回复清单超出安全上限")
				}
				state.ReplyIDs = append(state.ReplyIDs, item.ID)
				if text, supported := exactReplyText(item.Content); supported {
					state.ReplyLocations = append(state.ReplyLocations, ReplyLocation{ItemID: item.ID, Offset: start, Fingerprint: storeHash(text)})
				}
			}
		}
	}
	return state, next, nil
}

// ReplyRecoveryHint 只返回固定提示，绝不把日志原文、内部标识或路径送往 IM。
func ReplyRecoveryHint(state RecoveryObservation) string {
	switch state.ReplyIssue {
	case "turn_conflict":
		return "执行已完成，但最终回复的轮次标识与执行轮次冲突；未猜测归属，请核对原会话，不要重跑"
	case "missing_id":
		return "执行已完成，但最终回复缺少消息标识，暂不能确认完整投递；不会重跑任务"
	case "ambiguous_unphased":
		return "完成正文对应多个缺省阶段的回复条目，未猜测归属；请核对原会话，不要重跑"
	case "duplicate_id":
		return "执行已完成，但回复清单出现重复消息标识，未擅自合并；不会重跑任务"
	case "":
		if len(state.ReplyIDs) == 0 {
			return "执行已完成，但尚无可确切关联的最终回复清单；回复同步待核对，不要重跑"
		}
		return ""
	default:
		return "执行已完成，但回复身份存在未识别的异常；回复同步待核对，不要重跑"
	}
}

// 专用于旧解析器已经确认完成、却漏掉缺省 phase 回复的记录。
// 独立重读原请求的有界字节范围；不得回退已有游标或拿后续轮次替代。
func (c *Client) RecoverUnphasedCompletion(ctx context.Context, thread, path, user string, start, end int64, previous RecoveryObservation) (RecoveryObservation, bool, error) {
	if previous.Outcome != "completed" || len(previous.ReplyIDs) != 0 || previous.ReplyIssue != "" || previous.MatchedTurn == "" || previous.UserItemID != user || end < start || end-start > 8<<20 {
		return previous, false, nil
	}
	updated, next, err := c.ReadRecoveryObservation(ctx, thread, path, start, user, RecoveryObservation{})
	if err != nil {
		return previous, false, err
	}
	if next != end || updated.MatchedTurn != previous.MatchedTurn || updated.Outcome != "completed" || updated.ReplyIssue != "" || updated.ReplyEvidence != "explicit_turn_and_completion_text_unphased" || len(updated.ReplyIDs) != 1 {
		return previous, false, nil
	}
	return updated, true, nil
}
