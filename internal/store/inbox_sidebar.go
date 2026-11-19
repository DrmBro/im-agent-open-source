package store

import (
	"context"
	"time"
)

func (s *Store) NextInboxPlacement(ctx context.Context) (thread, connection, title string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT q.thread_id,s.connection_id,coalesce(t.title,'') FROM inbox_sidebar_queue q
 JOIN sessions s ON s.thread_id=q.thread_id LEFT JOIN inbox_topics t ON t.event_id=q.event_id
 WHERE s.status='active' AND q.retry_at<=? ORDER BY q.retry_at,q.rowid LIMIT 1`, time.Now().Unix()).Scan(&thread, &connection, &title)
	return
}

func (s *Store) FinishInboxPlacement(ctx context.Context, thread string, done bool) error {
	if done {
		_, err := s.db.ExecContext(ctx, "DELETE FROM inbox_sidebar_queue WHERE thread_id=?", thread)
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE inbox_sidebar_queue SET retry_at=? WHERE thread_id=?", time.Now().Add(30*time.Second).Unix(), thread)
	return err
}
