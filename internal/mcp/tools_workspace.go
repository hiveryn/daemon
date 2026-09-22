package mcp

import (
	"context"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerWorkspaceTools registers the architect-only workspace inspection
// tools. Both are read-only: they report what is on disk and change nothing, so
// an architect can run them freely, including in a workspace that is currently
// broken.
//
// There is no worker equivalent of either tool. A worker is handed the files it
// needs and does not maintain the workspace.
func (s *Server) registerWorkspaceTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "checkWorkspace",
		Description: "Inspect this architect workspace. Takes no parameters — the workspace is resolved for you. " +
			"Returns the expected/discovered file tree with, per entry: whether it exists, whether it is structurally valid, " +
			"the document's own lastUpdatedAt/archivedAt, the filesystem mtime, and diagnostics carrying code/path/line/message. " +
			"Also returns ticket totals by status and warnings for individual backlog tickets. " +
			"It is read-only and changes nothing. `valid` is a structural verdict only: it does not mean a document's contents are current or correct, " +
			"and ticket warnings never make the workspace invalid. Run it at startup and after each coherent edit to the managed documents, workflows or config.",
	}, s.handleCheckWorkspace)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "describeArtifact",
		Description: "Get the schema for one workspace artifact kind: its location and naming, its frontmatter/config fields, the rules enforced on it, and a minimal example. " +
			"These are the same definitions checkWorkspace validates against, so anything described here is actually enforced. " +
			"Use it before authoring or editing a project document, a workflow, an archived roadmap or hiveryn.yaml. " +
			"Tickets and conclusions are not artifact kinds — they keep their own tools and schemas.",
	}, s.handleDescribeArtifact)
}

func (s *Server) handleCheckWorkspace(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ CheckWorkspaceInput,
) (*mcp.CallToolResult, CheckWorkspaceOutput, error) {
	output, err := s.checkWorkspace(ctx)
	if err != nil {
		return nil, CheckWorkspaceOutput{}, err
	}
	return nil, output, nil
}

func (s *Server) handleDescribeArtifact(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input DescribeArtifactInput,
) (*mcp.CallToolResult, DescribeArtifactOutput, error) {
	kind := strings.TrimSpace(input.Kind)
	if !domain.ArtifactKind(kind).Valid() {
		return nil, DescribeArtifactOutput{}, newValidationError("kind", "must be one of: "+strings.Join(artifactKindNames(), ", "))
	}

	output, err := s.describeArtifact(ctx, kind)
	if err != nil {
		return nil, DescribeArtifactOutput{}, err
	}
	return nil, output, nil
}

func artifactKindNames() []string {
	kinds := domain.ArtifactKinds()
	names := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		names = append(names, string(kind))
	}
	return names
}
