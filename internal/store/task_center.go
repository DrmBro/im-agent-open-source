package store

import (
	"errors"
	"time"
)

type TaskRow struct {
	StateLabel  string         `json:"state_label"`
	Result      *RequestResult `json:"result,omitempty"`
	ID          string         `json:"id"`
	ThreadID    string         `json:"thread_id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Stage       string         `json:"stage"`
	WaitingHint string         `json:"waiting_hint,omitempty"`
	Error       string         `json:"error"`
	ReceivedAt  int64          `json:"received_at"`
	UpdatedAt   int64          `json:"updated_at"`
}
type DeliveryResolution struct {
	ThreadID  string `json:"thread_id"`
	ItemID    string `json:"item_id"`
	Part      int    `json:"part"`
	Action    string `json:"action"`
	Confirmed bool   `json:"confirmed"`
}

func (s *Store) ResolveDelivery(r DeliveryResolution) error {
	if r.ThreadID == "" || r.ItemID == "" || r.Part < 0 {
		return errors.New("投递标识无效")
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var state string
	if e = tx.QueryRow("SELECT state FROM live_mirror_parts WHERE thread_id=? AND item_id=? AND part_index=?", r.ThreadID, r.ItemID, r.Part).Scan(&state); e != nil {
		return e
	}
	next := "pending"
	switch r.Action {
	case "retry":
		if state != "pending" {
			return errors.New("只能重试明确失败的待投递消息；未知结果必须人工核对")
		}
	case "confirmed_missing":
		if state != "unknown" || !r.Confirmed {
			return errors.New("需确认在 IM 中未收到此片段；重试可能产生重复消息")
		}
	case "confirmed_sent":
		if state != "unknown" || !r.Confirmed {
			return errors.New("需人工核对该片段确实已送达")
		}
		next = "sent"
	default:
		return errors.New("不支持的投递操作")
	}
	hint := ""
	if next == "sent" {
		hint = "用户已在 IM 核对送达；非平台自动回执"
	}
	result, e := tx.Exec(`UPDATE live_mirror_parts SET state=?,error=?,next_attempt=0,updated_at=?
 WHERE thread_id=? AND item_id=? AND part_index=? AND state=?`, next, hint, time.Now().Unix(), r.ThreadID, r.ItemID, r.Part, state)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("投递状态已变化")
	}
	if _, e = tx.Exec("INSERT INTO meta(key,value) VALUES (?,?)", "delivery-resolution:"+ID(), JSON(map[string]any{"request": r, "previous_state": state, "at": time.Now().Unix()})); e != nil {
		return e
	}
	return tx.Commit()
}
