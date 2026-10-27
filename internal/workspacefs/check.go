package workspacefs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// ListWorkflows discovers plain Markdown workflows. Selection is explicit.
func (s *Service) ListWorkflows(_ context.Context, architectKey, workspacePath string) (domain.WorkflowList, error) {
	diags := newDiagnostics(WorkflowsDirName)
	validateDirectory(joinWorkspace(workspacePath, WorkflowsDirName), diags)
	workflows, err := discoverWorkflows(workspacePath, diags)
	if err != nil {
		return domain.WorkflowList{}, err
	}
	return domain.WorkflowList{ArchitectKey: architectKey, Workflows: workflows, Diagnostics: diags.items}, nil
}

// PreflightWorker reports whether the workspace's required project context is
// ready for a ticket session right now.
//
// It runs the launch's own project-context validation rather than deriving a
// verdict from CheckWorkspace: the check's aggregate `valid` covers
// architect-only artifacts and every discovered workflow, none of which blocks
// a worker, so reusing it would refuse launches the daemon would accept.
func (s *Service) PreflightWorker(_ context.Context, architectKey, workspacePath string) (domain.WorkerPreflight, error) {
	problems := PreflightWorkerContext(workspacePath, architectKey)
	if problems == nil {
		problems = []string{}
	}
	return domain.WorkerPreflight{
		ArchitectKey: architectKey,
		CheckedAt:    s.now(),
		Launchable:   len(problems) == 0,
		Problems:     problems,
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

	configNode := checkConfig(workspacePath, architectKey)
	report.Nodes = append(report.Nodes, configNode)

	for _, kind := range []domain.ArtifactKind{
		domain.ArtifactProjectOverview,
	} {
		report.Nodes = append(report.Nodes, checkRootDocument(workspacePath, kind))
	}

	workflowsNode, err := checkWorkflows(workspacePath)
	if err != nil {
		return domain.WorkspaceReport{}, err
	}
	report.Nodes = append(report.Nodes, workflowsNode)

	report.Tickets, report.Diagnostics = s.checkTickets(ctx, workspacePath, report.Diagnostics)
	report.Valid = reportIsValid(report)
	return report, nil
}

// checkConfig uses the config loader's rules and checks local repo paths.
func checkConfig(workspacePath, architectKey string) (node domain.WorkspaceNode) {
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
		return node
	case err != nil:
		diags.errorf(domain.DiagUnreadable, 0, "cannot inspect %s: %v", ConfigFileName, err)
		node.Diagnostics = diags.items
		return node
	case info.IsDir():
		diags.errorf(domain.DiagNotAFile, 0, "%s is a directory; a YAML file is expected", ConfigFileName)
		node.Diagnostics = diags.items
		return node
	}

	node.Exists = true
	modTime := info.ModTime().UTC()
	node.ModifiedAt = &modTime

	resolved, err := config.ValidateArchitectConfig(workspacePath, architectKey)
	if err != nil {
		diags.errorf(domain.DiagConfigInvalid, 0, "%s is invalid: %v", ConfigFileName, err)
		node.Diagnostics = diags.items
		return node
	}

	// Repo paths are checked here and nowhere else at load time: a repo
	// directory that moves after being configured must not stop the daemon
	// loading the config, but the architect does need to be told about it.
	for _, repoKey := range sortedRepoKeys(resolved.Repos) {
		if resolved.RepoMachines[repoKey] != "" {
			continue
		} // Remote paths cannot be inspected on this machine.
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
	return node
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
func checkWorkflows(workspacePath string) (domain.WorkspaceNode, error) {
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

	workflows, err := discoverWorkflows(workspacePath, diags)
	if err != nil {
		diags.errorf(domain.DiagUnreadable, 0, "cannot inspect workflows: %v", err)
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
			Severity: domain.DiagnosticError,
			Path:     "tickets",
			Message:  fmt.Sprintf("ticket scan failed: %v", err),
		})
	}

	tickets.Totals = domain.WorkspaceTicketTotals{
		Backlog:  len(board.Backlog),
		Progress: len(board.Progress),
		Done:     len(board.Done),
	}
	cfg, cfgErr := config.ValidateArchitectConfig(workspacePath, filepath.Base(workspacePath))
	for _, summary := range append(append(board.Backlog, board.Progress...), board.Done...) {
		if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{4}-.+$`).MatchString(summary.ID) {
			reportDiags = append(reportDiags, domain.WorkspaceDiagnostic{Code: "INVALID_TICKET_ID", Severity: domain.DiagnosticError, Path: "tickets/" + string(summary.Status) + "/" + summary.ID, Message: "Use a stable YYYY-MM-DD-HHMM-readable-slug ticket folder name."})
		}

		if cfgErr == nil && summary.Status != domain.TicketStatusDone {
			keys := append([]string{summary.Repo}, summary.AdditionalRepos...)
			seen := map[string]bool{}
			for _, key := range keys {
				_, exists := cfg.Repos[key]
				if !exists || seen[key] || cfg.RepoMachines[key] != cfg.RepoMachines[summary.Repo] {
					reportDiags = append(reportDiags, domain.WorkspaceDiagnostic{Code: "INVALID_TICKET_SCOPE", Severity: domain.DiagnosticError, Path: "tickets/" + string(summary.Status) + "/" + summary.ID + "/ticket.md", Message: "Repository " + key + " is unknown, duplicated or on a different machine; correct the writable scope using hiveryn.yaml."})
				}
				seen[key] = true
			}
		}

		for _, warning := range summary.Warnings {
			if warning.Code == "INVALID_REFERENCE" {
				reportDiags = append(reportDiags, invalidReferenceDiagnostic(summary, warning))
			}
			if warning.Code == "INVALID_TICKET" {
				reportDiags = append(reportDiags, domain.WorkspaceDiagnostic{Code: warning.Code, Severity: domain.DiagnosticError, Path: "tickets/" + string(summary.Status) + "/" + summary.ID + "/ticket.md", Message: warning.Message})
			}
			if summary.Status != domain.TicketStatusBacklog {
				continue
			}
			tickets.Warnings = append(tickets.Warnings, domain.WorkspaceTicketWarning{
				TicketID: summary.ID,
				Code:     warning.Code,
				Message:  warning.Message,
			})
		}
	}
	return tickets, reportDiags
}

// invalidReferenceDiagnostic reports an invalid ticket reference at its
// ticket.md line. Author-owned backlog and active progress tickets carry it
// as an error, so it reaches the architect's validation notice; a completed
// record keeps it as a warning, since only the supported reference migration
// may rewrite it.
func invalidReferenceDiagnostic(summary domain.TicketSummary, warning domain.TicketWarning) domain.WorkspaceDiagnostic {
	d := domain.WorkspaceDiagnostic{
		Code:     warning.Code,
		Severity: domain.DiagnosticError,
		Path:     "tickets/" + string(summary.Status) + "/" + summary.ID + "/ticket.md",
		Message:  warning.Message,
	}
	if rest, ok := strings.CutPrefix(warning.Message, "ticket.md line "); ok {
		if n, tail, ok := strings.Cut(rest, ": "); ok {
			if line, err := strconv.Atoi(n); err == nil {
				d.Line, d.Message = line, tail
			}
		}
	}
	remedy := " Use a same-board ticket id, architect:<path relative to the architect workspace> or <repo key>:<path relative to that repo> (see hiveryn.yaml for repo keys)."
	if summary.Status == domain.TicketStatusDone {
		d.Severity = domain.DiagnosticWarning
		remedy = " Completed records are runtime-owned: convert them with `hiverynd references-migrate`, never by direct edit."
	}
	d.Message += "." + remedy
	return d
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

func rootDocumentFileName(kind domain.ArtifactKind) string {
	switch kind {
	case domain.ArtifactProjectOverview:
		return ProjectOverviewFileName
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
