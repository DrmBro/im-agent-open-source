package store

import (
	"errors"
	"time"
)

type CompletionRecovery struct {
	ThreadID, Path, Fingerprint string
	Offset, Length              int64
}

func (s *Store) migrateCompletionRecovery() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS live_mirror_completion_recoveries (
 thread_id TEXT NOT NULL, path TEXT NOT NULL, byte_offset INTEGER NOT NULL,
 fingerprint TEXT NOT NULL, parser_version INTEGER NOT NULL, state TEXT NOT NULL,
 recovered_count INTEGER NOT NULL, checked_at INTEGER NOT NULL,
 PRIMARY KEY(thread_id,path,byte_offset,fingerprint,parser_version)
 )`)
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"next_attempt", "INTEGER NOT NULL DEFAULT 0"}, {"attempts", "INTEGER NOT NULL DEFAULT 0"}, {"detail", "TEXT NOT NULL DEFAULT ''"},
	} {
		has, checkErr := hasColumn(s.db, "live_mirror_completion_recoveries", column.name)
		if checkErr != nil {
			return checkErr
		}
		if !has {
			if _, err = s.db.Exec("ALTER TABLE live_mirror_completion_recoveries ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	return nil
}

// 每条原异常由本版解析器核对一次，避免不可恢复的旧记录阻塞后续记录。
func (s *Store) PendingCompletionRecoveries(thread string) ([]CompletionRecovery, error) {
	rows, err := s.db.Query(`SELECT i.thread_id,i.path,i.byte_offset,i.byte_length,i.fingerprint
 FROM live_mirror_read_issues i LEFT JOIN live_mirror_completion_recoveries r
 ON r.thread_id=i.thread_id AND r.path=i.path AND r.byte_offset=i.byte_offset AND r.fingerprint=i.fingerprint AND r.parser_version=3
 WHERE i.thread_id=? AND i.kind='final_reply_unconfirmed' AND (r.thread_id IS NULL OR (r.state='retry' AND r.next_attempt<=?))
 ORDER BY coalesce(r.checked_at,0),i.created_at,i.byte_offset LIMIT 4`, thread, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CompletionRecovery
	for rows.Next() {
		var item CompletionRecovery
		if err = rows.Scan(&item.ThreadID, &item.Path, &item.Offset, &item.Length, &item.Fingerprint); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// 原消息入账、异常消除和修订审计同事务提交；原游标及已有送达回执保持不变。
func (s *Store) SaveCompletionRecovery(item CompletionRecovery, entries []MirrorEntry, verified bool) error {
	if !verified && len(entries) > 0 {
		return errors.New("未核验的完成记录不能补发")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow(`SELECT count(*) FROM live_mirror_read_issues
 WHERE thread_id=? AND path=? AND byte_offset=? AND byte_length=? AND fingerprint=? AND kind='final_reply_unconfirmed'`, item.ThreadID, item.Path, item.Offset, item.Length, item.Fingerprint).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return errors.New("原同步异常已变化，未补发")
	}
	state := "unconfirmed"
	if verified {
		state = "recovered"
		for _, entry := range entries {
			if entry.Role != "assistant" || entry.ItemID == "" {
				return errors.New("补齐内容不是原助手回复")
			}
			if err = prepareMirrorEntry(tx, item.ThreadID, item.Path, entry); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(`DELETE FROM live_mirror_read_issues WHERE thread_id=? AND path=? AND byte_offset=? AND fingerprint=?`, item.ThreadID, item.Path, item.Offset, item.Fingerprint); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO live_mirror_completion_recoveries
(thread_id,path,byte_offset,fingerprint,parser_version,state,recovered_count,checked_at)
VALUES(?,?,?,?,3,?,?,?) ON CONFLICT DO UPDATE SET state=excluded.state,recovered_count=excluded.recovered_count,checked_at=excluded.checked_at,next_attempt=0,detail=''`, item.ThreadID, item.Path, item.Offset, item.Fingerprint, state, len(entries), time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// 临时读取失败退避并让出队列位置；保留原异常和原投递账本。
func (s *Store) RetryCompletionRecovery(item CompletionRecovery, detail string) error {
	_, err := s.db.Exec(`INSERT INTO live_mirror_completion_recoveries
 (thread_id,path,byte_offset,fingerprint,parser_version,state,recovered_count,checked_at,next_attempt,attempts,detail)
 VALUES(?,?,?,?,3,'retry',0,?,?,1,?) ON CONFLICT DO UPDATE SET
 state='retry',checked_at=excluded.checked_at,attempts=live_mirror_completion_recoveries.attempts+1,
 next_attempt=excluded.checked_at+MIN(300,10*(1 << MIN(live_mirror_completion_recoveries.attempts,5))),detail=excluded.detail
 WHERE live_mirror_completion_recoveries.state='retry'`, item.ThreadID, item.Path, item.Offset, item.Fingerprint, time.Now().Unix(), time.Now().Unix()+10, detail)
	return err
}
