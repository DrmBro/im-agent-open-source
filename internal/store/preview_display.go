package store

import "time"

// 只要求既有已结束卡片刷新展示，不清理原输出证据、不重发任何聊天消息。
func (s *Store) InvalidateFinishedPreviewCards() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var applied int
	if err = tx.QueryRow(`SELECT count(*) FROM meta WHERE key='finished-preview-cleared-v1'`).Scan(&applied); err != nil {
		return err
	}
	if applied != 0 {
		return nil
	}
	var exists int
	if err = tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='request_telemetry'`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		_, err = tx.Exec(`UPDATE session_status_cards SET observed_version='' WHERE state='sent' AND message_id<>'' AND EXISTS(
 SELECT 1 FROM request_telemetry t JOIN events e ON e.id=t.event_id LEFT JOIN request_results r ON r.event_id=t.event_id
 WHERE t.event_id=session_status_cards.request_id AND t.thread_id=session_status_cards.thread_id
 AND json_extract(CASE WHEN json_valid(t.display_json) THEN t.display_json ELSE '{}' END,'$.preview')<>''
 AND (e.status IN ('done','failed') OR r.execution_state IN ('completed','failed')
 OR coalesce(json_extract(CASE WHEN json_valid(t.display_json) THEN t.display_json ELSE '{}' END,'$.ended_at'),0)>0
 OR coalesce(json_extract(CASE WHEN json_valid(t.display_json) THEN t.display_json ELSE '{}' END,'$.outcome'),'')<>''))`)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO meta(key,value) VALUES('finished-preview-cleared-v1',?)`, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
