package domain

import (
	"context"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export the file-based workspace contracts from shared. The desktop parses
// these too — the workspace report and the workflow list are read contracts for
// both the MCP tool surface and the app — so they live in shared, not here.
type (
	ArtifactKind           = sd.ArtifactKind
	ArtifactField          = sd.ArtifactField
	ArtifactFieldType      = sd.ArtifactFieldType
	ArtifactSchema         = sd.ArtifactSchema
	DiagnosticSeverity     = sd.DiagnosticSeverity
	WorkspaceDiagnostic    = sd.WorkspaceDiagnostic
	WorkspaceEntry         = sd.WorkspaceEntry
	WorkspaceNode          = sd.WorkspaceNode
	WorkspaceNodeType      = sd.WorkspaceNodeType
	WorkspaceReport        = sd.WorkspaceReport
	WorkspaceTicketTotals  = sd.WorkspaceTicketTotals
	WorkspaceTicketWarning = sd.WorkspaceTicketWarning
	WorkspaceTickets       = sd.WorkspaceTickets

	Workflow       = sd.Workflow
	WorkflowAttach = sd.WorkflowAttach
	WorkflowList   = sd.WorkflowList
)

const (
	ArtifactProjectOverview = sd.ArtifactProjectOverview
	ArtifactProjectState    = sd.ArtifactProjectState
	ArtifactRoadmapCurrent  = sd.ArtifactRoadmapCurrent
	ArtifactRoadmapArchive  = sd.ArtifactRoadmapArchive
	ArtifactArchitectSystem = sd.ArtifactArchitectSystem
	ArtifactWorkflow        = sd.ArtifactWorkflow
	ArtifactHiverynYAML     = sd.ArtifactHiverynYAML
)

const (
	ArtifactFieldString       = sd.ArtifactFieldString
	ArtifactFieldStringList   = sd.ArtifactFieldStringList
	ArtifactFieldStringMap    = sd.ArtifactFieldStringMap
	ArtifactFieldEnum         = sd.ArtifactFieldEnum
	ArtifactFieldTimestampUTC = sd.ArtifactFieldTimestampUTC
	ArtifactFieldMapping      = sd.ArtifactFieldMapping
	ArtifactFieldMappingList  = sd.ArtifactFieldMappingList
)

const (
	DiagnosticError   = sd.DiagnosticError
	DiagnosticWarning = sd.DiagnosticWarning
)

const (
	WorkspaceNodeFile      = sd.WorkspaceNodeFile
	WorkspaceNodeDirectory = sd.WorkspaceNodeDirectory
)

const (
	WorkflowAttachManual    = sd.WorkflowAttachManual
	WorkflowAttachSuggested = sd.WorkflowAttachSuggested
)

const (
	DiagMissingRequiredFile      = sd.DiagMissingRequiredFile
	DiagMissingRequiredDirectory = sd.DiagMissingRequiredDirectory
	DiagNotAFile                 = sd.DiagNotAFile
	DiagNotADirectory            = sd.DiagNotADirectory
	DiagUnreadable               = sd.DiagUnreadable
	DiagInvalidUTF8              = sd.DiagInvalidUTF8
	DiagIncompleteRead           = sd.DiagIncompleteRead
	DiagOutsideWorkspace         = sd.DiagOutsideWorkspace
	DiagEmptyBody                = sd.DiagEmptyBody
	DiagMissingFrontmatter       = sd.DiagMissingFrontmatter
	DiagInvalidFrontmatter       = sd.DiagInvalidFrontmatter
	DiagMissingField             = sd.DiagMissingField
	DiagInvalidTimestamp         = sd.DiagInvalidTimestamp
	DiagUnexpectedField          = sd.DiagUnexpectedField
	DiagArchiveNameInvalid       = sd.DiagArchiveNameInvalid
	DiagArchiveDateMismatch      = sd.DiagArchiveDateMismatch
	DiagWorkflowSubdirectory     = sd.DiagWorkflowSubdirectory
	DiagWorkflowInvalidAttach    = sd.DiagWorkflowInvalidAttach
	DiagWorkflowMissingRepos     = sd.DiagWorkflowMissingRepos
	DiagWorkflowUnknownRepo      = sd.DiagWorkflowUnknownRepo
	DiagWorkflowDuplicateRepo    = sd.DiagWorkflowDuplicateRepo
	DiagWorkflowReposNotAllowed  = sd.DiagWorkflowReposNotAllowed
	DiagConfigInvalid            = sd.DiagConfigInvalid
	DiagRepoPathMissing          = sd.DiagRepoPathMissing
	DiagRepoPathNotDirectory     = sd.DiagRepoPathNotDirectory
	DiagTicketScanFailed         = sd.DiagTicketScanFailed
)

// ArtifactKinds is the complete, ordered catalog describeArtifact accepts.
func ArtifactKinds() []ArtifactKind { return sd.ArtifactKinds() }

// WorkspaceService inspects an architect workspace. Every method is read-only:
// an implementation reports what is on disk and repairs nothing, so a broken
// workspace stays inspectable instead of blocking architect startup.
type WorkspaceService interface {
	// CheckWorkspace validates the whole workspace and reports the
	// expected/discovered tree, its diagnostics, and ticket totals/warnings.
	CheckWorkspace(ctx context.Context, architectKey, workspacePath string) (WorkspaceReport, error)
	// ListWorkflows discovers workflows/*.md and marks which are suggested for
	// the given writable repo scope. An empty scope suggests nothing.
	ListWorkflows(ctx context.Context, architectKey, workspacePath string, scopeRepos []string) (WorkflowList, error)
	// DescribeArtifact renders the schema for one artifact kind from the same
	// definitions the checks execute.
	DescribeArtifact(ctx context.Context, kind ArtifactKind) (ArtifactSchema, error)
}
