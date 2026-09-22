package workspacefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// Service inspects architect workspaces. It holds no state beyond the ticket
// system it reads counts from, and it never writes to a workspace.
type Service struct {
	tickets domain.TicketService
	now     func() time.Time
}

// NewService builds the workspace service. tickets supplies ticket totals and
// backlog warnings; it may be nil, in which case the ticket section is reported
// as unavailable rather than silently zeroed.
func NewService(tickets domain.TicketService) *Service {
	return &Service{tickets: tickets, now: func() time.Time { return time.Now().UTC() }}
}

var _ domain.WorkspaceService = (*Service)(nil)

// DescribeArtifact renders the schema for one artifact kind.
func (s *Service) DescribeArtifact(_ context.Context, kind domain.ArtifactKind) (domain.ArtifactSchema, error) {
	schema, err := Describe(kind)
	if err != nil {
		return domain.ArtifactSchema{}, &domain.ValidationError{Field: "kind", Message: err.Error()}
	}
	return schema, nil
}

// ListWorkflows discovers the workspace's workflows and marks the ones
// suggested for scopeRepos.
func (s *Service) ListWorkflows(_ context.Context, architectKey, workspacePath string, scopeRepos []string) (domain.WorkflowList, error) {
	listDiags := newDiagnostics(WorkflowsDirName)

	// A broken config does not stop workflow discovery: the workflows are
	// still there to read and repair. It only turns off the repo-key check,
	// with the config problem reported alongside so the gap is visible.
	configured, configErr := loadRepoScope(workspacePath, architectKey)
	if configErr != nil {
		listDiags.warnf(domain.DiagConfigInvalid, 0,
			"%s could not be read, so workflow repo keys were not checked: %v", ConfigFileName, configErr)
	}

	validateDirectory(joinWorkspace(workspacePath, WorkflowsDirName), listDiags)

	workflows, err := discoverWorkflows(workspacePath, configured, listDiags)
	if err != nil {
		return domain.WorkflowList{}, err
	}

	writable := normalizeScopeRepos(scopeRepos)
	markSuggested(workflows, writable)

	return domain.WorkflowList{
		ArchitectKey: architectKey,
		ScopeRepos:   writable,
		Workflows:    workflows,
		Diagnostics:  listDiags.items,
	}, nil
}

// CheckWorkspace validates the whole workspace and assembles the report.
//
// It resolves nothing from its caller beyond the architect's identity and
// walks a fixed expected shape, so two runs over unchanged files produce
// identical output. Missing and invalid files become diagnostics; none of them
// is an error return, because a broken workspace has to stay inspectable so the
// architect can repair it.
func (s *Service) CheckWorkspace(ctx context.Context, architectKey, workspacePath string) (domain.WorkspaceReport, error) {
	report := domain.WorkspaceReport{
		ArchitectKey:  architectKey,
		WorkspacePath: workspacePath,
		CheckedAt:     s.now(),
		Nodes:         []domain.WorkspaceNode{},
		Diagnostics:   []domain.WorkspaceDiagnostic{},
		Tickets: domain.WorkspaceTickets{
			Warnings: []domain.WorkspaceTicketWarning{},
		},
	}

	if info, err := os.Stat(workspacePath); err != nil || !info.IsDir() {
		// The workspace root itself is the one thing a report cannot work
		// around. It is still a diagnostic, not an error return: the architect
		// needs to be told which path is wrong.
		message := fmt.Sprintf("architect workspace %s is not a readable directory", workspacePath)
		if err != nil {
			message = fmt.Sprintf("cannot open architect workspace %s: %v", workspacePath, err)
		}
		report.Diagnostics = append(report.Diagnostics, domain.WorkspaceDiagnostic{
			Code:     domain.DiagUnreadable,
			Severity: domain.DiagnosticError,
			Message:  message,
		})
		return report, nil
	}

	scope, configNode := checkConfig(workspacePath, architectKey)
	report.Nodes = append(report.Nodes, configNode)

	for _, kind := range []domain.ArtifactKind{
		domain.ArtifactProjectOverview,
		domain.ArtifactProjectState,
		domain.ArtifactRoadmapCurrent,
		domain.ArtifactArchitectSystem,
	} {
		report.Nodes = append(report.Nodes, checkRootDocument(workspacePath, kind))
	}

	workflowsNode, err := checkWorkflows(workspacePath, scope)
	if err != nil {
		return domain.WorkspaceReport{}, err
	}
	report.Nodes = append(report.Nodes, workflowsNode)

	archivesNode, err := checkArchives(workspacePath)
	if err != nil {
		return domain.WorkspaceReport{}, err
	}
	report.Nodes = append(report.Nodes, archivesNode)

	report.Tickets, report.Diagnostics = s.checkTickets(ctx, workspacePath, report.Diagnostics)
	report.Valid = reportIsValid(report)
	return report, nil
}

// checkConfig validates hiveryn.yaml through the loader's own ruleset, so the
// check agrees with what the daemon will actually accept, and returns the repo
// keys the rest of the check validates against.
//
// A config that fails to load yields no repo keys, which would make every
// workflow's repos look unknown. So the workflow repo check is skipped in that
// case: the config error is the real finding, and a pile of derived
// WORKFLOW_UNKNOWN_REPO noise would bury it.
func checkConfig(workspacePath, architectKey string) (scope repoScope, node domain.WorkspaceNode) {
	def := definitions[domain.ArtifactHiverynYAML]
	diags := newDiagnostics(ConfigFileName)
	absPath := joinWorkspace(workspacePath, ConfigFileName)

	node = domain.WorkspaceNode{
		Kind:     def.Kind,
		Type:     domain.WorkspaceNodeFile,
		Path:     ConfigFileName,
		Required: true,
	}

	info, err := os.Stat(absPath)
	switch {
	case os.IsNotExist(err):
		diags.errorf(domain.DiagMissingRequiredFile, 0, "%s is required but does not exist; the workspace has no repo map without it", ConfigFileName)
		node.Diagnostics = diags.items
		return repoScope{}, node
	case err != nil:
		diags.errorf(domain.DiagUnreadable, 0, "cannot inspect %s: %v", ConfigFileName, err)
		node.Diagnostics = diags.items
		return repoScope{}, node
	case info.IsDir():
		diags.errorf(domain.DiagNotAFile, 0, "%s is a directory; a YAML file is expected", ConfigFileName)
		node.Diagnostics = diags.items
		return repoScope{}, node
	}

	node.Exists = true
	modTime := info.ModTime().UTC()
	node.ModifiedAt = &modTime

	resolved, err := config.ValidateArchitectConfig(workspacePath, architectKey)
	if err != nil {
		diags.errorf(domain.DiagConfigInvalid, 0, "%s is invalid: %v", ConfigFileName, err)
		node.Diagnostics = diags.items
		return repoScope{}, node
	}

	// Repo paths are checked here and nowhere else at load time: a repo
	// directory that moves after being configured must not stop the daemon
	// loading the config, but the architect does need to be told about it.
	for _, repoKey := range sortedRepoKeys(resolved.Repos) {
		repoPath := resolved.Repos[repoKey]
		repoInfo, statErr := os.Stat(repoPath)
		switch {
		case os.IsNotExist(statErr):
			diags.errorf(domain.DiagRepoPathMissing, 0, "repos.%s points at %s, which does not exist", repoKey, repoPath)
		case statErr != nil:
			diags.errorf(domain.DiagRepoPathMissing, 0, "repos.%s points at %s, which cannot be inspected: %v", repoKey, repoPath, statErr)
		case !repoInfo.IsDir():
			diags.errorf(domain.DiagRepoPathNotDirectory, 0, "repos.%s points at %s, which is not a directory", repoKey, repoPath)
		}
	}

	node.Diagnostics = diags.items
	node.Valid = !diags.hasErrors()
	return scopeFromRepos(resolved.Repos), node
}

// checkRootDocument validates one workspace-root markdown document.
func checkRootDocument(workspacePath string, kind domain.ArtifactKind) domain.WorkspaceNode {
	def := definitions[kind]
	name := rootDocumentFileName(kind)
	diags := newDiagnostics(name)

	node := domain.WorkspaceNode{
		Kind:     kind,
		Type:     domain.WorkspaceNodeFile,
		Path:     name,
		Required: def.Required,
	}

	result := validateMarkdownArtifact(def, joinWorkspace(workspacePath, name), def.Required, diags)
	node.Exists = result.Exists
	node.ModifiedAt = result.ModifiedAt
	node.DocumentUpdatedAt = result.DocumentUpdatedAt
	node.Diagnostics = diags.items
	// An absent optional document is valid; an absent required one already
	// carries its error.
	node.Valid = !diags.hasErrors()
	return node
}

// checkWorkflows validates workflows/ and every workflow inside it.
func checkWorkflows(workspacePath string, scope repoScope) (domain.WorkspaceNode, error) {
	diags := newDiagnostics(WorkflowsDirName)
	exists, modTime := validateDirectory(joinWorkspace(workspacePath, WorkflowsDirName), diags)

	node := domain.WorkspaceNode{
		Kind:       domain.ArtifactWorkflow,
		Type:       domain.WorkspaceNodeDirectory,
		Path:       WorkflowsDirName,
		Required:   true,
		Exists:     exists,
		ModifiedAt: modTime,
		Children:   []domain.WorkspaceEntry{},
	}

	workflows, err := discoverWorkflows(workspacePath, scope, diags)
	if err != nil {
		return domain.WorkspaceNode{}, err
	}
	for _, workflow := range workflows {
		node.Children = append(node.Children, domain.WorkspaceEntry{
			Kind:        domain.ArtifactWorkflow,
			Path:        workflow.RelPath,
			Valid:       workflow.Valid,
			ModifiedAt:  workflow.ModifiedAt,
			Diagnostics: workflow.Diagnostics,
		})
	}

	node.Diagnostics = diags.items
	node.Valid = !diags.hasErrors()
	return node, nil
}

// checkArchives validates archives/roadmaps/ and every archived roadmap in it.
func checkArchives(workspacePath string) (domain.WorkspaceNode, error) {
	diags := newDiagnostics(RoadmapArchiveDir)
	dir := joinWorkspace(workspacePath, RoadmapArchiveDir)
	exists, modTime := validateDirectory(dir, diags)

	node := domain.WorkspaceNode{
		Kind:       domain.ArtifactRoadmapArchive,
		Type:       domain.WorkspaceNodeDirectory,
		Path:       RoadmapArchiveDir,
		Required:   true,
		Exists:     exists,
		ModifiedAt: modTime,
		Children:   []domain.WorkspaceEntry{},
	}

	if exists {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return domain.WorkspaceNode{}, fmt.Errorf("read roadmap archive directory %q: %w", dir, err)
		}
		byName := make(map[string]os.DirEntry, len(entries))
		for _, entry := range entries {
			byName[entry.Name()] = entry
		}
		for _, name := range sortedNames(entries) {
			if byName[name].IsDir() {
				diags.errorf(domain.DiagNotAFile, 0, "%s/%s is a directory; %s holds archive files directly", RoadmapArchiveDir, name, RoadmapArchiveDir)
				continue
			}
			if !isMarkdown(name) {
				continue
			}
			node.Children = append(node.Children, validateArchiveFile(dir, name, RoadmapArchiveDir))
		}
	}

	node.Diagnostics = diags.items
	node.Valid = !diags.hasErrors()
	return node, nil
}

// checkTickets reports totals per status plus the warnings the ticket system
// raised for backlog tickets.
//
// Ticket warnings are diagnostic only and never affect the workspace verdict —
// a stale reference in a backlog ticket is worth surfacing but is not a broken
// workspace. A scan failure becomes a workspace-level diagnostic carrying the
// verbatim error, rather than a zeroed count that would read as "no tickets".
func (s *Service) checkTickets(ctx context.Context, workspacePath string, reportDiags []domain.WorkspaceDiagnostic) (domain.WorkspaceTickets, []domain.WorkspaceDiagnostic) {
	tickets := domain.WorkspaceTickets{Warnings: []domain.WorkspaceTicketWarning{}}

	if s.tickets == nil {
		return tickets, append(reportDiags, domain.WorkspaceDiagnostic{
			Code:     domain.DiagTicketScanFailed,
			Severity: domain.DiagnosticWarning,
			Message:  "ticket totals are unavailable: no ticket service is wired into the workspace check",
		})
	}

	board, err := s.tickets.ListTickets(ctx, workspacePath)
	if err != nil {
		return tickets, append(reportDiags, domain.WorkspaceDiagnostic{
			Code:     domain.DiagTicketScanFailed,
			Severity: domain.DiagnosticWarning,
			Message:  fmt.Sprintf("ticket totals are unavailable: %v", err),
		})
	}

	tickets.Totals = domain.WorkspaceTicketTotals{
		Backlog:  len(board.Backlog),
		Progress: len(board.Progress),
		Done:     len(board.Done),
	}
	for _, summary := range board.Backlog {
		for _, warning := range summary.Warnings {
			tickets.Warnings = append(tickets.Warnings, domain.WorkspaceTicketWarning{
				TicketID: summary.ID,
				Code:     warning.Code,
				Message:  warning.Message,
			})
		}
	}
	return tickets, reportDiags
}

// reportIsValid is true only when no error-severity diagnostic appears anywhere
// in the report.
func reportIsValid(report domain.WorkspaceReport) bool {
	if hasError(report.Diagnostics) {
		return false
	}
	for _, node := range report.Nodes {
		if !nodeIsValid(node) {
			return false
		}
	}
	return true
}

func nodeIsValid(node domain.WorkspaceNode) bool {
	if hasError(node.Diagnostics) {
		return false
	}
	for _, child := range node.Children {
		if hasError(child.Diagnostics) {
			return false
		}
	}
	return true
}

func hasError(diags []domain.WorkspaceDiagnostic) bool {
	for _, diag := range diags {
		if diag.Severity == domain.DiagnosticError {
			return true
		}
	}
	return false
}

// loadRepoScope loads the architect's configured repo keys through the loader's
// own ruleset. A returned error leaves the scope unknown, which switches off
// repo-key checks instead of declaring every key unknown.
func loadRepoScope(workspacePath, architectKey string) (repoScope, error) {
	resolved, err := config.ValidateArchitectConfig(workspacePath, architectKey)
	if err != nil {
		return repoScope{}, err
	}
	return scopeFromRepos(resolved.Repos), nil
}

func scopeFromRepos(repos map[string]string) repoScope {
	keys := make(map[string]struct{}, len(repos))
	for key := range repos {
		keys[key] = struct{}{}
	}
	return repoScope{Keys: keys, Known: true}
}

func rootDocumentFileName(kind domain.ArtifactKind) string {
	switch kind {
	case domain.ArtifactProjectOverview:
		return ProjectOverviewFileName
	case domain.ArtifactProjectState:
		return ProjectStateFileName
	case domain.ArtifactRoadmapCurrent:
		return RoadmapCurrentFileName
	case domain.ArtifactArchitectSystem:
		return ArchitectSystemFileName
	default:
		return string(kind)
	}
}

func isMarkdown(name string) bool {
	return strings.EqualFold(filepath.Ext(name), markdownExt)
}

func sortedRepoKeys(repos map[string]string) []string {
	keys := make([]string, 0, len(repos))
	for key := range repos {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
