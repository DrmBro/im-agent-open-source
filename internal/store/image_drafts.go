package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"im-agent/internal/im"
)

const ImageDraftTTL = 10 * time.Minute

type ImageDraft struct {
	ID        string          `json:"id"`
	Address   im.Address      `json:"address"`
	UserID    string          `json:"user_id"`
	Images    []im.Attachment `json:"images"`
	ExpiresAt int64           `json:"expires_at"`
}

func imageScope(a im.Address, user string) string { return Hash(a.Key() + "\x00" + user) }
func (s *Store) migrateImageDrafts() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS image_drafts (
 id TEXT PRIMARY KEY,scope_key TEXT NOT NULL,address_json TEXT NOT NULL,user_id TEXT NOT NULL,
 images_json TEXT NOT NULL,state TEXT NOT NULL,trigger_id TEXT NOT NULL DEFAULT '',expires_at INTEGER NOT NULL,created_at INTEGER NOT NULL
 );
 CREATE UNIQUE INDEX IF NOT EXISTS image_drafts_pending ON image_drafts(scope_key) WHERE state='pending';
 CREATE TABLE IF NOT EXISTS image_draft_events(event_id TEXT PRIMARY KEY,draft_id TEXT NOT NULL REFERENCES image_drafts(id));`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE image_drafts SET state='interrupted' WHERE state='submitted'")
	return err
}
func scanImageDraft(row interface{ Scan(...any) error }) (ImageDraft, error) {
	var d ImageDraft
	var address, images string
	err := row.Scan(&d.ID, &address, &d.UserID, &images, &d.ExpiresAt)
	if err != nil {
		return d, err
	}
	if json.Unmarshal([]byte(address), &d.Address) != nil || json.Unmarshal([]byte(images), &d.Images) != nil {
		return d, errors.New("暂存附件记录损坏，未提交")
	}
	return d, nil
}

const imageDraftColumns = "id,address_json,user_id,images_json,expires_at"

func (s *Store) StageImages(a im.Address, user, event string, images []im.Attachment, now time.Time) (ImageDraft, error) {
	if a.ChatID == "" || user == "" || event == "" || len(images) == 0 || len(images) > 5 {
		return ImageDraft{}, errors.New("附件暂存参数无效或超过5个")
	}
	for _, i := range images {
		if (i.Kind != "image" && i.Kind != "file") || i.SourceMessageID == "" || i.Key == "" || i.SourceIndex < 0 {
			return ImageDraft{}, errors.New("附件来源不完整，未暂存")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ImageDraft{}, err
	}
	defer tx.Rollback()
	d, err := scanImageDraft(tx.QueryRow("SELECT "+imageDraftColumns+" FROM image_drafts WHERE scope_key=? AND state='pending'", imageScope(a, user)))
	if errors.Is(err, sql.ErrNoRows) {
		d = ImageDraft{ID: ID(), Address: a, UserID: user, Images: images, ExpiresAt: now.Add(ImageDraftTTL).Unix()}
		_, err = tx.Exec("INSERT INTO image_drafts(id,scope_key,address_json,user_id,images_json,state,expires_at,created_at) VALUES(?,?,?,?,?,'pending',?,?)", d.ID, imageScope(a, user), JSON(a), user, JSON(images), d.ExpiresAt, now.Unix())
	} else if err == nil {
		if d.ExpiresAt <= now.Unix() {
			return d, errors.New("旧附件组已过期，等待清理；本条未追加，请重新发送")
		}
		if len(d.Images)+len(images) > 5 {
			return d, errors.New("一组附件最多5个；已有附件保留，请先发说明或 /send")
		}
		d.Images = append(d.Images, images...)
		_, err = tx.Exec("UPDATE image_drafts SET images_json=? WHERE id=? AND state='pending'", JSON(d.Images), d.ID)
	}
	if err != nil {
		return d, err
	}
	if _, err = tx.Exec("INSERT INTO image_draft_events(event_id,draft_id) VALUES(?,?)", event, d.ID); err != nil {
		return d, err
	}
	if _, err = tx.Exec("UPDATE events SET status='waiting_input',stage='waiting_caption',updated_at=? WHERE id=?", now.Unix(), event); err != nil {
		return d, err
	}
	return d, tx.Commit()
}
func (s *Store) ConsumeImages(a im.Address, user, trigger string, extra int, now time.Time) (ImageDraft, bool, error) {
	if trigger == "" || extra < 0 || extra > 5 {
		return ImageDraft{}, false, errors.New("附件提交参数无效")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ImageDraft{}, false, err
	}
	defer tx.Rollback()
	d, err := scanImageDraft(tx.QueryRow("SELECT "+imageDraftColumns+" FROM image_drafts WHERE scope_key=? AND state='pending' AND expires_at>?", imageScope(a, user), now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		// 进程退出后重做同一触发消息，不能把已领取的图片组悄悄丢掉。
		d, err = scanImageDraft(tx.QueryRow("SELECT "+imageDraftColumns+" FROM image_drafts WHERE scope_key=? AND trigger_id=? AND state IN ('submitted','interrupted')", imageScope(a, user), trigger))
		if err == nil {
			return d, true, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			return d, false, nil
		}
		return d, false, err
	}
	if err != nil {
		return d, false, err
	}
	if len(d.Images)+extra > 5 {
		return d, false, errors.New("暂存与本条消息合计超过5个附件，未提交；请先处理暂存附件")
	}
	if _, err = tx.Exec("UPDATE image_drafts SET state='submitted',trigger_id=? WHERE id=? AND state='pending'", trigger, d.ID); err != nil {
		return d, false, err
	}
	if _, err = tx.Exec("UPDATE events SET status='bundled',stage='images_bundled',updated_at=? WHERE id IN (SELECT event_id FROM image_draft_events WHERE draft_id=?)", now.Unix(), d.ID); err != nil {
		return d, false, err
	}
	return d, true, tx.Commit()
}
func (s *Store) CancelImages(a im.Address, user string, now time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow("SELECT id FROM image_drafts WHERE scope_key=? AND state='pending'", imageScope(a, user)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.Exec("UPDATE image_drafts SET state='cancelled' WHERE id=?", id); err != nil {
		return false, err
	}
	if _, err = tx.Exec("UPDATE events SET status='cancelled',stage='images_cancelled',updated_at=? WHERE id IN (SELECT event_id FROM image_draft_events WHERE draft_id=?)", now.Unix(), id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
func (s *Store) ExpireImageDrafts(now time.Time) ([]ImageDraft, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT "+imageDraftColumns+" FROM image_drafts WHERE state='pending' AND expires_at<=? ORDER BY created_at LIMIT 100", now.Unix())
	if err != nil {
		return nil, err
	}
	var expired []ImageDraft
	for rows.Next() {
		d, e := scanImageDraft(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		expired = append(expired, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, d := range expired {
		if _, err = tx.Exec("UPDATE image_drafts SET state='expired' WHERE id=? AND state='pending'", d.ID); err != nil {
			return nil, err
		}
		if _, err = tx.Exec("UPDATE events SET status='expired',stage='images_expired',updated_at=? WHERE id IN (SELECT event_id FROM image_draft_events WHERE draft_id=?)", now.Unix(), d.ID); err != nil {
			return nil, err
		}
	}
	return expired, tx.Commit()
}
func (s *Store) FinishImageDraft(trigger, status string) error {
	if status != "done" && status != "failed" && status != "uncertain" {
		return errors.New("附件任务结果无效")
	}
	_, err := s.db.Exec("UPDATE image_drafts SET state=? WHERE trigger_id=? AND state='submitted'", status, trigger)
	return err
}
