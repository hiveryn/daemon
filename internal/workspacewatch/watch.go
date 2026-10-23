// Package workspacewatch reconciles filesystem-authored workspaces using directory
// watches and startup snapshots. Client refreshes are event-driven.
package workspacewatch

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

type Watcher struct {
	Source     config.Source
	Protection *architectfs.Protection
	Workspaces domain.WorkspaceService
	Publish    func(string, domain.ArchitectEvent)
	Notify     func(context.Context, string, string) error
	Logger     *slog.Logger
	mu         sync.Mutex
	states     map[string]*state
}
type state struct {
	root    string
	files   map[string]string
	changed time.Time
	dirty   bool
}

func (w *Watcher) Reconcile(ctx context.Context, key string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	cfg, err := w.Source.Current()
	if err != nil {
		return err
	}
	a, ok := cfg.Architects[key]
	if !ok {
		return fmt.Errorf("architect %q is not configured", key)
	}
	return w.reconcile(ctx, key, a.Path)
}
func (w *Watcher) reconcile(ctx context.Context, key, path string) error {
	files, conflicts, err := w.Protection.Inspect(ctx, path)
	var inspectErr error
	if err != nil {
		inspectErr = err
		conflicts = append(conflicts, domain.WorkspaceDiagnostic{Code: "WORKSPACE_READ_FAILED", Severity: domain.DiagnosticError, Path: path, Message: err.Error()})
	}
	report, err := w.Workspaces.CheckWorkspace(ctx, key, path)
	if err != nil {
		return err
	}
	diags := append([]domain.WorkspaceDiagnostic{}, report.Diagnostics...)
	diags = append(diags, conflicts...)
	for _, node := range report.Nodes {
		diags = append(diags, node.Diagnostics...)
		for _, child := range node.Children {
			diags = append(diags, child.Diagnostics...)
		}
	}
	var lines []string
	for _, d := range diags {
		if d.Severity == domain.DiagnosticError {
			lines = append(lines, fmt.Sprintf("%s:%d [%s] %s", d.Path, d.Line, d.Code, d.Message))
		}
	}
	sort.Strings(lines)
	w.Publish(key, domain.ArchitectEvent{Type: domain.ArchitectEventType, ArchitectKey: key, Reason: domain.ArchitectEventTicketUpdated, At: time.Now().UTC()})
	if len(lines) > 0 {
		if err := w.Notify(ctx, key, "Workspace validation errors in "+path+":\n\n"+strings.Join(lines, "\n")+"\n\nRepair author-owned files directly. Runtime-owned record conflicts require restoring the accepted content; filesystem edits never authorize lifecycle changes."); err != nil {
			return err
		}
	}
	if w.states == nil {
		w.states = map[string]*state{}
	}
	w.states[key] = &state{root: path, files: files}
	if inspectErr != nil {
		// A later filesystem event or explicit reconciliation retries the read.
		w.Logger.Error("workspace inspection incomplete", "architect_key", key, "error", inspectErr)
	}
	return nil
}

// Run reconciles at startup and batches related filesystem edits after two
// seconds with no content change. No diagnostic announcement cache is kept.
func (w *Watcher) Run(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		w.Logger.Error("start workspace watches", "error", err)
		return
	}
	defer func() { _ = watcher.Close() }()
	watched := map[string]os.FileInfo{}
	refresh := func() {
		cfg, err := w.Source.Current()
		if err != nil {
			w.Logger.Error("workspace watch configuration", "error", err)
			return
		}
		desired := map[string]bool{}
		for key, a := range cfg.Architects {
			desired[a.Path] = true
			for _, name := range []string{"workflows", "tickets", "architect-sessions"} {
				err := filepath.WalkDir(filepath.Join(a.Path, name), func(path string, d fs.DirEntry, err error) error {
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					if d.IsDir() {
						desired[path] = true
					}
					return nil
				})
				if err != nil {
					w.Logger.Error("discover workspace watches", "architect_key", key, "error", err)
				}
			}

		}
		for path := range desired {
			info, statErr := os.Stat(path)
			previous := watched[path]
			if previous != nil && (statErr != nil || !os.SameFile(previous, info)) {
				_ = watcher.Remove(path)
				delete(watched, path)
			}
			if watched[path] == nil && statErr == nil {
				if err := watcher.Add(path); err != nil {
					w.Logger.Error("watch workspace directory", "path", path, "error", err)
				} else {
					watched[path] = info
					// A directory may have appeared with all its files before Add.
					// Reconcile its snapshot even if those create events were missed.
					w.mu.Lock()
					for key, a := range cfg.Architects {
						if path == a.Path || strings.HasPrefix(path, a.Path+string(filepath.Separator)) {
							if st := w.states[key]; st != nil {
								st.dirty = true
								st.changed = time.Now()
							}
						}
					}
					w.mu.Unlock()
				}
			}
		}
		for key, a := range cfg.Architects {
			w.mu.Lock()
			st, known := w.states[key]
			known = known && st.root == a.Path
			w.mu.Unlock()
			if !known {
				if err := w.Reconcile(ctx, key); err != nil {
					w.Logger.Error("workspace startup reconciliation", "architect_key", key, "error", err)
				}
			}
		}

		for path := range watched {
			if !desired[path] {
				_ = watcher.Remove(path)
				delete(watched, path)
			}
		}
	}
	refresh()
	timer := time.NewTicker(250 * time.Millisecond)
	defer timer.Stop()
	lastRefresh := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
				for path := range watched {
					if path == event.Name || strings.HasPrefix(path, event.Name+string(filepath.Separator)) {
						_ = watcher.Remove(path)
						delete(watched, path)
					}
				}
			}
			cfg, err := w.Source.Current()
			if err != nil {
				w.Logger.Error("resolve workspace event", "error", err)
				continue
			}
			for key, a := range cfg.Architects {
				rel, err := filepath.Rel(a.Path, event.Name)
				if err != nil {
					continue
				}
				rel = filepath.ToSlash(rel)
				if rel != "hiveryn.yaml" && rel != "OVERVIEW.md" && rel != "workflows" && rel != "tickets" && rel != "architect-sessions" && !strings.HasPrefix(rel, "workflows/") && !strings.HasPrefix(rel, "tickets/") && !strings.HasPrefix(rel, "architect-sessions/") {
					continue
				}
				w.mu.Lock()
				if st := w.states[key]; st != nil {
					st.dirty = true
					st.changed = time.Now()
				}
				w.mu.Unlock()
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
				refresh()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			w.Logger.Error("workspace watch event loss; reconciling", "error", err)
			w.mu.Lock()
			for _, st := range w.states {
				st.dirty = true
				st.changed = time.Now()
			}
			w.mu.Unlock()
		case <-timer.C:
			if time.Since(lastRefresh) >= time.Second {
				refresh()
				lastRefresh = time.Now()
			}
			w.flush(ctx)
		}
	}
}
func (w *Watcher) flush(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cfg, err := w.Source.Current()
	if err != nil {
		w.Logger.Error("workspace watch configuration", "error", err)
		return
	}
	for key, st := range w.states {
		a, exists := cfg.Architects[key]
		if !exists {
			delete(w.states, key)
			continue
		}
		if !st.dirty || time.Since(st.changed) < 2*time.Second {
			continue
		}
		files, err := architectfs.SnapshotManaged(a.Path)
		if err != nil {
			if err := w.reconcile(ctx, key, a.Path); err != nil {
				w.Logger.Error("workspace snapshot reconciliation", "architect_key", key, "error", err)
			}
			continue
		}
		if maps.Equal(st.files, files) {
			st.dirty = false
			continue
		}
		if err := w.reconcile(ctx, key, a.Path); err != nil {
			w.Logger.Error("workspace validation", "architect_key", key, "error", err)
		}
	}
}
