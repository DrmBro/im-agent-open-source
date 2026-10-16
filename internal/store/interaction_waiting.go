package store

import "time"

// expires_at=0 表示不因用户等待时间而失效。只更新仍有效的旧表单，
// 不复活已提交、取消或替换的操作；桌面审批另由原生目录精确复核。
func (s *Store) migrateInteractionWaiting() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, query := range []string{
		`UPDATE pending_actions SET expires_at=0 WHERE status='pending' AND expires_at>?`,
		`UPDATE creation_forms SET expires_at=0 WHERE state='pending' AND expires_at>?`,
		`UPDATE session_controls SET expires_at=0 WHERE kind='model' AND state='open' AND expires_at>?`,
	} {
		if _, err = tx.Exec(query, now); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE desktop_approval_cards SET expires_at=0 WHERE state IN ('pending','responding','uncertain')`); err != nil {
		return err
	}
	return tx.Commit()
}
