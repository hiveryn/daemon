package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type WorkspaceRecordStore struct{ db *sql.DB }

func NewWorkspaceRecordStore(db *sql.DB) *WorkspaceRecordStore { return &WorkspaceRecordStore{db: db} }
func (s *WorkspaceRecordStore) ReadWorkspaceRecords(ctx context.Context, path string) (map[string]string, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT fingerprints FROM workspace_records WHERE workspace = ?`, path).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read protected workspace records: %w", err)
	}
	var records map[string]string
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		return nil, false, fmt.Errorf("decode protected workspace records: %w", err)
	}
	return records, true, nil
}
func (s *WorkspaceRecordStore) WriteWorkspaceRecords(ctx context.Context, path string, records map[string]string) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO workspace_records(workspace,fingerprints) VALUES (?,?) ON CONFLICT(workspace) DO UPDATE SET fingerprints=excluded.fingerprints`, path, string(raw))
	if err != nil {
		return fmt.Errorf("write protected workspace records: %w", err)
	}
	return nil
}
