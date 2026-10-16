package store

import (
	"encoding/json"
	"errors"
	"im-agent/internal/telemetry"
	"time"
)

// 为旧解析器的空清单保存独立修订审计。原游标不变，原观察原样进入审计。
func (s *Store) SaveUnphasedReplyRecovery(p RequestProbe, state string) (bool, error) {
	var old, next struct {
		Outcome     string   `json:"outcome"`
		MatchedTurn string   `json:"matched_turn"`
		UserItemID  string   `json:"user_item_id"`
		ReplyIDs    []string `json:"reply_ids"`
		Evidence    string   `json:"reply_evidence"`
	}
	if json.Unmarshal([]byte(p.State), &old) != nil || json.Unmarshal([]byte(state), &next) != nil || old.Outcome != "completed" || len(old.ReplyIDs) != 0 || next.Outcome != "completed" || len(next.ReplyIDs) != 1 || old.MatchedTurn != next.MatchedTurn || old.UserItemID != next.UserItemID || next.UserItemID != p.UserItemID || next.Evidence != "explicit_turn_and_completion_text_unphased" {
		return false, errors.New("回复格式修订证据无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	r, err := tx.Exec(`UPDATE request_results SET observer_state=?,observation_error='',next_probe=?,updated_at=?
 WHERE event_id=? AND thread_id=? AND address_key=? AND turn_ref=? AND observer_path=? AND observer_offset=? AND observer_state=? AND sealed=0 AND execution_state='completed'
 AND EXISTS(SELECT 1 FROM events e WHERE e.id=request_results.event_id AND e.status IN ('uncertain','interrupted'))
 AND EXISTS(SELECT 1 FROM live_mirror_origins o WHERE o.event_id=request_results.event_id AND o.thread_id=request_results.thread_id AND o.state='matched' AND o.user_item_id=? AND o.path=request_results.observer_path AND o.min_offset=?)`, state, time.Now().Unix(), time.Now().Unix(), p.EventID, p.ThreadID, p.AddressKey, p.TurnRef, p.Path, p.Offset, p.State, p.UserItemID, p.OriginOffset)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if err = auditRequest(tx, p.EventID, "unphased_reply_evidence_recovered", "uncertain", "reply_delivery_unconfirmed", "原观察保留："+p.State+"；以原请求同一完成事件逐字核验缺省阶段回复，未回退游标或重跑任务"); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// 已逐字核验的原回复补充到旧遥测快照，保留用量及已有观察游标。
func (s *Store) RecoverReplyPreview(p RequestProbe, actualTurn, id, text, preview string, at int64) error {
	if at <= 0 {
		return nil
	}
	var proof struct {
		Outcome        string `json:"outcome"`
		MatchedTurn    string `json:"matched_turn"`
		ReplyLocations []struct {
			ItemID      string `json:"item_id"`
			Fingerprint string `json:"fingerprint"`
			Timestamp   int64  `json:"timestamp"`
		} `json:"reply_locations"`
	}
	if json.Unmarshal([]byte(p.State), &proof) != nil || proof.Outcome != "completed" || proof.MatchedTurn != actualTurn {
		return errors.New("预览缺少完成证据")
	}
	valid := false
	for _, location := range proof.ReplyLocations {
		if location.ItemID == id && location.Fingerprint == Hash(text) && location.Timestamp == at {
			valid = true
		}
	}
	if !valid {
		return errors.New("预览原文与回复位置证据不一致")
	}
	var stateJSON, displayJSON string
	var revision int64
	if err := s.db.QueryRow(`SELECT state_json,display_json,revision FROM request_telemetry WHERE event_id=? AND thread_id=? AND address_key=? AND user_item_id=?`, p.EventID, p.ThreadID, p.AddressKey, p.UserItemID).Scan(&stateJSON, &displayJSON, &revision); err != nil {
		return err
	}
	var state telemetry.State
	var snapshot telemetry.Snapshot
	if json.Unmarshal([]byte(stateJSON), &state) != nil || json.Unmarshal([]byte(displayJSON), &snapshot) != nil || state.Halted || state.MatchedTurn != actualTurn {
		return errors.New("预览遥测关联无效")
	}
	snapshot.TurnID = actualTurn
	snapshot.Outcome = "completed"
	if snapshot.FirstOutputAt == 0 || at < snapshot.FirstOutputAt {
		snapshot.FirstOutputAt = at
	}
	if at > snapshot.LastOutputAt {
		snapshot.LastOutputAt = at
	}
	runes := []rune(preview)
	if len(runes) > 1000 {
		runes = runes[len(runes)-1000:]
	}
	snapshot.Preview, snapshot.PreviewAt = string(runes), at
	snapshot.CompletionOutputVerified = true
	snapshot.ObservedAt = 0
	body, _ := json.Marshal(snapshot)
	digest := Hash(string(body))
	snapshot.ObservedAt = time.Now().UnixMilli()
	state.Snapshot = snapshot
	nextState, _ := json.Marshal(state)
	nextDisplay, _ := json.Marshal(snapshot)
	_, err := s.db.Exec(`UPDATE request_telemetry SET state_json=?,display_json=?,digest=?,revision=revision+1
 WHERE event_id=? AND thread_id=? AND address_key=? AND state_json=? AND revision=?
 AND EXISTS(SELECT 1 FROM request_results r WHERE r.event_id=request_telemetry.event_id AND r.observer_state=? AND r.observer_offset=? AND r.execution_state='completed' AND r.sealed=0)`, string(nextState), string(nextDisplay), digest, p.EventID, p.ThreadID, p.AddressKey, stateJSON, revision, p.State, p.Offset)
	return err
}
