package store

import "context"

// Remote resources are ownership records, not terminal layout snapshots. Keep
// them until confirmed cleanup so restart cannot orphan auxiliary tmux servers.
func (s *SessionStore) AddRemoteResource(ctx context.Context, id, ssh, socket, directory string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO remote_resources(session_id,ssh,socket,directory) VALUES(?,?,?,?)`, id, ssh, socket, directory)
	return err
}

// RemoveRemoteResource drops one ownership record after its cleanup was confirmed.
func (s *SessionStore) RemoveRemoteResource(ctx context.Context, id, ssh, socket string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remote_resources WHERE session_id=? AND ssh=? AND socket=?`, id, ssh, socket)
	return err
}
func (s *SessionStore) RemoteResources(ctx context.Context, id string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ssh,socket FROM remote_resources WHERE session_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	resources := map[string][]string{}
	for rows.Next() {
		var ssh, socket string
		if err := rows.Scan(&ssh, &socket); err != nil {
			return nil, err
		}
		resources[ssh] = append(resources[ssh], socket)
	}
	return resources, rows.Err()
}

func (s *SessionStore) RemoteDirectories(ctx context.Context, id string) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ssh,directory FROM remote_resources WHERE session_id=? AND directory<>''`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string][]string{}
	for rows.Next() {
		var ssh, dir string
		if err := rows.Scan(&ssh, &dir); err != nil {
			return nil, err
		}
		result[ssh] = append(result[ssh], dir)
	}
	return result, rows.Err()
}
