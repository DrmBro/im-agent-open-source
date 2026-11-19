package store

import "context"

func (s *Store) migrateInboxTopics() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS inbox_topics(event_id TEXT PRIMARY KEY REFERENCES personal_inbox(id) ON DELETE CASCADE, topic_id TEXT NOT NULL,title TEXT NOT NULL)`)
	return err
}

type InboxTopic struct {
	ID       string
	Title    string
	Messages []string
}

// 归类仅标注仍待处理的原消息，不改变任务和收件状态。
func (s *Store) SaveInboxTopics(ctx context.Context, topics []InboxTopic) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, topic := range topics {
		for _, id := range topic.Messages {
			_, err = tx.ExecContext(ctx, `INSERT INTO inbox_topics(event_id,topic_id,title)
SELECT id,?,? FROM personal_inbox WHERE id=? AND state='new' ON CONFLICT(event_id) DO NOTHING`, topic.ID, topic.Title, id)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
