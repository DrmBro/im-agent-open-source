package store

// SessionChannels 包括关闭中的会话，确保重启后的归档/恢复仍使用原执行通道。
func (s *Store) SessionChannels() (map[string]string, error) {
	rows, err := s.db.Query("SELECT thread_id,channel FROM sessions")
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
