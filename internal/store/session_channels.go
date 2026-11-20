package store

const DesktopAssistantRoute = "desktop_assistant"

// SessionRoutes 包括关闭中的会话，确保重启后的归档/恢复仍使用原执行通道。
func (s *Store) SessionRoutes() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT s.thread_id,CASE WHEN d.thread_id IS NULL THEN s.channel ELSE ? END FROM sessions s LEFT JOIN assistant_desktop_sessions d ON d.thread_id=s.thread_id`, DesktopAssistantRoute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var thread, channel string
		if err := rows.Scan(&thread, &channel); err != nil {
			return nil, err
		}
		result[thread] = channel
	}
	return result, rows.Err()
}
