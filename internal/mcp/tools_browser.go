package mcp

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerBrowserTools registers the browser-tab tools shared across all
// session types (architect, ticket, freeform) — any running agent's desktop
// window can preview a file/URL, mirroring how createWorkTicket is shared.
func (s *Server) registerBrowserTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "previewInBrowserTab",
		Description: "Open or navigate a browser tab in the user's desktop window. target is a file://, absolute path, http://localhost:*, or https:// URL. Omit tab_id to open a new tab; pass an existing tab_id to navigate it in place. Returns the tab id and target.",
	}, s.handlePreviewInBrowserTab)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "getBrowserTabs",
		Description: "List the session's currently open browser tabs (id + current target).",
	}, s.handleGetBrowserTabs)
}

func (s *Server) handlePreviewInBrowserTab(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	input PreviewInBrowserTabInput,
) (*mcp.CallToolResult, PreviewInBrowserTabOutput, error) {
	target := strings.TrimSpace(input.Target)
	if target == "" {
		return nil, PreviewInBrowserTabOutput{}, newValidationError("target", "is required")
	}

	output, err := s.previewInBrowserTab(ctx, target, strings.TrimSpace(input.TabID))
	if err != nil {
		return nil, PreviewInBrowserTabOutput{}, err
	}

	return nil, output, nil
}

func (s *Server) handleGetBrowserTabs(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ struct{},
) (*mcp.CallToolResult, GetBrowserTabsOutput, error) {
	output, err := s.getBrowserTabs(ctx)
	if err != nil {
		return nil, GetBrowserTabsOutput{}, err
	}

	return nil, output, nil
}
