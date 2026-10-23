package domain

import "context"

// WorkspaceRecords persist the accepted fingerprints of lifecycle-owned files.
// A missing record bootstraps existing history once; subsequent external changes
// are conflicts, including changes made while the daemon was stopped.
type WorkspaceRecords interface {
	ReadWorkspaceRecords(context.Context, string) (map[string]string, bool, error)
	WriteWorkspaceRecords(context.Context, string, map[string]string) error
}
