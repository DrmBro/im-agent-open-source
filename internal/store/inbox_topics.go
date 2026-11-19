package store

import "context"

func (s *Store) migrateInboxTopics() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbox_topics(event_id TEXT PRIMARY KEY REFERENCES personal_inbox(id) ON DELETE CASCADE, topic_id TEXT NOT NULL,title TEXT NOT NULL); CREATE TABLE IF NOT EXISTS inbox_sidebar_queue(thread_id TEXT PRIMARY KEY REFERENCES sessions(thread_id) ON DELETE CASCADE,event_id TEXT NOT NULL,retry_at INTEGER NOT NULL DEFAULT 0)`)
	return err
}

type InboxTopic struct {
	ID       string
	Title    string
	Messages []string
}

// 归类仅标注仍待处理的原消息，不改变任务和收件状态。
func (s *Store) SaveInboxTopics(ctx context.Context, topics []InboxTopic, replace bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, topic := range topics {
		for _, id := range topic.Messages {
			if replace {
				if _, err = tx.ExecContext(ctx, "DELETE FROM inbox_topics WHERE event_id=? AND EXISTS(SELECT 1 FROM personal_inbox WHERE id=? AND state='new')", id, id); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO inbox_topics(event_id,topic_id,title)
SELECT id,?,? FROM personal_inbox WHERE id=? AND state='new' ON CONFLICT(event_id) DO NOTHING`, topic.ID, topic.Title, id)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
