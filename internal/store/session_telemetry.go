package store

import (
	"database/sql"
	"errors"
)

// ObservedSessionRequest 仅通过已登记的用户条目或轮次身份关联 IM 请求。
// 不按相似正文、最近时间或其它会话的标识猜测归属。
func (s *Store) ObservedSessionRequest(session Session, user, turn string) (string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT e.id FROM events e
 JOIN request_results r ON r.event_id=e.id AND r.thread_id=? AND r.address_key=e.chat_key
 LEFT JOIN live_mirror_origins o ON o.event_id=e.id AND o.thread_id=r.thread_id AND o.state='matched'
 LEFT JOIN request_telemetry t ON t.event_id=e.id AND t.thread_id=r.thread_id AND t.address_key=r.address_key
 WHERE e.chat_key=? AND ((?<>'' AND o.user_item_id=?) OR (?<>'' AND json_valid(t.display_json) AND json_extract(t.display_json,'$.turn_id')=?))`,
		session.ThreadID, Hash(session.Address.Key()), user, user, turn, turn)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var found string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return "", err
		}
		if found != "" {
			return "", errors.New("轮次对应多个请求，状态待核对")
		}
		found = id
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", sql.ErrNoRows
	}
	return found, nil
}
