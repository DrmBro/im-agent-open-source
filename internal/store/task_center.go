package store

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type TaskRow struct {
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
type DeliveryRow struct {
	ThreadID   string `json:"thread_id"`
	Name       string `json:"name"`
	ItemID     string `json:"item_id"`
	Part       int    `json:"part"`
	State      string `json:"state"`
	Error      string `json:"error"`
	PlatformID string `json:"platform_id"`
	UpdatedAt  int64  `json:"updated_at"`
}
type TaskCenter struct {
	Waiting     *WaitingSummary `json:"waiting,omitempty"`
	Creations   []CreationForm  `json:"creations"`
	Attachments []Transfer      `json:"attachments"`
	Tasks       []TaskRow       `json:"tasks"`
	Deliveries  []DeliveryRow   `json:"deliveries"`
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
func TaskCenterSnapshot(dir string) (TaskCenter, error) {
	out := TaskCenter{Tasks: []TaskRow{}, Deliveries: []DeliveryRow{}}
	path := filepath.Join(dir, "sessions.db")
	info, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return out, nil
	}
	if e != nil || !info.Mode().IsRegular() {
		return out, errors.New("业务数据库不可读取")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return out, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, _ = db.Exec("PRAGMA busy_timeout=1000")
	out.Waiting, e = waitingSummarySnapshot(db)
	if e != nil {
		return out, e
	}
	// 事件保存的是地址散列；在本地使用已有唯一会话映射关联，不按正文猜测。
	sessions := map[string]Session{}
	names := map[string]string{}
	rows, e := db.Query("SELECT " + columns + " FROM sessions")
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var s Session
		if e = rows.Scan(&s.ThreadID, &s.Address.Channel, &s.Address.Connection, &s.Address.ChatID, &s.WorkDir, &s.Owner, &s.Status, &s.CreatedAt, &s.Managed, &s.Address.TopicID, &s.Address.RootMessageID); e != nil {
			rows.Close()
			return out, e
		}
		sessions[Hash(s.Address.Key())] = s
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	rows, e = db.Query("SELECT thread_id,target_name FROM session_presentation")
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var id, name string
		if e = rows.Scan(&id, &name); e != nil {
			rows.Close()
			return out, e
		}
		names[id] = name
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	rows, e = db.Query("SELECT id,status,stage,error_summary,created_at,updated_at,chat_key FROM events ORDER BY created_at DESC,id LIMIT 150")
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var r TaskRow
		var key string
		if e = rows.Scan(&r.ID, &r.Status, &r.Stage, &r.Error, &r.ReceivedAt, &r.UpdatedAt, &key); e != nil {
			rows.Close()
			return out, e
		}
		r.ThreadID = sessions[key].ThreadID
		r.Name = names[r.ThreadID]
		r.WaitingHint = RequestWaitingHint(r)
		out.Tasks = append(out.Tasks, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	var hasResults int
	if e = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='request_results'").Scan(&hasResults); e != nil {
		return out, e
	}
	if hasResults != 0 {
		for i := range out.Tasks {
			out.Tasks[i].Result, e = requestResultSnapshot(db, out.Tasks[i].ID)
			if e != nil {
				return out, e
			}
		}
	}
	out.Creations, e = creationFormsSnapshot(db)
	if e != nil {
		return out, e
	}
	out.Attachments, e = transfersSnapshot(db)
	if e != nil {
		return out, e
	}
	var has int
	if e = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='live_mirror_parts'").Scan(&has); e != nil || has == 0 {
		return out, e
	}
	rows, e = db.Query("SELECT thread_id,item_id,part_index,state,error,platform_id,updated_at FROM live_mirror_parts ORDER BY CASE WHEN state='sent' THEN 1 ELSE 0 END,updated_at DESC LIMIT 200")
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var r DeliveryRow
		if e = rows.Scan(&r.ThreadID, &r.ItemID, &r.Part, &r.State, &r.Error, &r.PlatformID, &r.UpdatedAt); e != nil {
			return out, e
		}
		r.Name = names[r.ThreadID]
		out.Deliveries = append(out.Deliveries, r)
	}
	return out, rows.Err()
}
