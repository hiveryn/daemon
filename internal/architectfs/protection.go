package architectfs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hiveryn/daemon/internal/domain"
)

// Protection serializes daemon mutations with reconciliation. Files are never
// reverted: accepted fingerprints remain unchanged until a daemon mutation.
type Protection struct {
	mu      sync.Mutex
	records domain.WorkspaceRecords
}

func NewProtection(records domain.WorkspaceRecords) *Protection { return &Protection{records: records} }

// SnapshotManaged reads content fingerprints, not mtimes, so atomic saves and
// same-size rewrites are observed. Symlinks are fingerprinted, never followed.
func SnapshotManaged(root string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range []string{"hiveryn.yaml", "OVERVIEW.md", "workflows", "tickets", "architect-sessions"} {
		base := filepath.Join(root, name)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				out[rel+"/"] = "directory"
				return nil
			}
			if strings.HasPrefix(rel, "tickets/") && filepath.Base(path) != "ticket.md" && filepath.Base(path) != "conclusion.md" {
				return nil
			}
			if strings.HasPrefix(rel, "architect-sessions/") && filepath.Base(path) != "conclusion.md" {
				return nil
			}
			if strings.HasPrefix(rel, "workflows/") && filepath.Ext(path) != ".md" {
				return nil
			}
			var data []byte
			if d.Type()&os.ModeSymlink != 0 {
				target, e := os.Readlink(path)
				err = e
				data = []byte("symlink:" + target)
			} else {
				data, err = os.ReadFile(path)
			}
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("%x", sha256.Sum256(data))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("inspect managed workspace %s: %w", base, err)
		}
	}
	return out, nil
}
func protectedRecords(all map[string]string) map[string]string {
	out := map[string]string{}
	for path, hash := range all {
		if path == "tickets/progress/" || path == "tickets/done/" {
			continue
		}
		if strings.HasPrefix(path, "tickets/progress/") || strings.HasPrefix(path, "tickets/done/") || strings.HasSuffix(path, "/conclusion.md") {
			out[path] = hash
		}
	}
	return out
}
func (p *Protection) inspect(ctx context.Context, root string) (map[string]string, []domain.WorkspaceDiagnostic, error) {
	all, err := SnapshotManaged(root)
	if err != nil {
		return nil, nil, err
	}
	actual := protectedRecords(all)
	accepted, exists, err := p.records.ReadWorkspaceRecords(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	if !exists {
		if err := p.records.WriteWorkspaceRecords(ctx, root, actual); err != nil {
			return nil, nil, err
		}
		return all, nil, nil
	}
	paths := map[string]bool{}
	for path := range actual {
		paths[path] = true
	}
	for path := range accepted {
		paths[path] = true
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	var diags []domain.WorkspaceDiagnostic
	for _, path := range ordered {
		if actual[path] == accepted[path] {
			continue
		}
		diags = append(diags, domain.WorkspaceDiagnostic{Code: "PROTECTED_RECORD_CONFLICT", Severity: domain.DiagnosticError, Path: path, Message: "Runtime-owned record was created, edited, moved or deleted outside Hiveryn. Restore the accepted file from the reviewed history or resolve the conflict with the user; no lifecycle transition was applied."})
	}
	return all, diags, nil
}
func (p *Protection) Inspect(ctx context.Context, root string) (map[string]string, []domain.WorkspaceDiagnostic, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inspect(ctx, root)
}
func (p *Protection) Mutate(ctx context.Context, root string, allowed []string, fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	before, diags, err := p.inspect(ctx, root)
	if err != nil {
		return err
	}
	if len(diags) > 0 {
		return &domain.ConflictError{Resource: "workspace", Message: diags[0].Path + ": " + diags[0].Message}
	}
	if err := fn(); err != nil {
		return err
	}
	all, err := SnapshotManaged(root)
	if err != nil {
		return err
	}
	accepted := protectedRecords(before)
	actual := protectedRecords(all)
	permitted := func(path string) bool {
		for _, prefix := range allowed {
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	for path := range accepted {
		if permitted(path) {
			delete(accepted, path)
		}
	}
	for path, hash := range actual {
		if permitted(path) {
			accepted[path] = hash
		}
	}
	return p.records.WriteWorkspaceRecords(ctx, root, accepted)
}
