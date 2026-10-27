package mcp

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/hiveryn/daemon/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Context tool descriptions are the worker's documentation of the reference
// grammar and of how a remote worker reads laptop context, so they say it in
// full rather than pointing elsewhere.
const (
	contextReferenceHelp = "reference is architect:<path> for a file in this project's architect workspace (OVERVIEW.md, knowledge/, investigations/, specs/, tickets/<status>/<id>/conclusion.md, …), or <repoKey>:<path> for a file in a repo configured for this project, where path is relative to that root ('.' is the root itself). " +
		"Hiveryn reads it where it lives — the architect workspace on the Hiveryn host (the user's laptop), a repo on its configured machine — so a remote worker reads laptop context and other machines' repos through this tool rather than its own filesystem. " +
		"Project documents point to context in this same form. Ticket references (resolved_references from readTicket) carry the tool to use in their access field. " +
		"Absolute paths, '..', and anything outside this project's workspace and repos are refused; reading context grants no write access."
)

func (s *Server) registerContextTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "readContext",
		Description: "Read a text file of this project's context, one page of lines at a time. " + contextReferenceHelp + " " +
			"Results are the file's current contents (not a launch snapshot): status is ok or explains what happened (missing, permission_denied, not_a_file — use listContext — binary, too_large, unreachable — the owning machine could not be reached, existence unknown — outside_root, changed). " +
			"Each page carries sha256, size and modified_at for the whole file and next_offset when more lines follow; pass the first page's sha256 as expected_sha256 on later pages to detect a change (status changed: start again at offset 1). Files up to 4 MiB; pages up to 2000 lines and 256 KiB.",
	}, s.handleReadContext)
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name: "listContext",
		Description: "List the immediate entries (not recursive) of a directory of this project's context, in name order and pages. " + contextReferenceHelp + " " +
			"Each entry gives its own reference to pass to readContext or listContext, its type (file, directory, symlink, other), size and modified_at. status is ok or explains what happened (missing, permission_denied, not_a_directory — use readContext — unreachable, outside_root, changed). " +
			"Pass next_offset for the next page and the first page's sha256 as expected_sha256 to detect a change.",
	}, s.handleListContext)
}

type ReadContextInput struct {
	Reference      string `json:"reference" jsonschema:"architect:<path> or <repoKey>:<path>, relative to that root (required)."`
	Offset         int    `json:"offset,omitempty" jsonschema:"1-based first line to read; default 1."`
	Limit          int    `json:"limit,omitempty" jsonschema:"Lines to read, 1-2000; default 400."`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"sha256 from the first page; a different current hash returns status changed."`
}

type ListContextInput struct {
	Reference      string `json:"reference" jsonschema:"architect:<path> or <repoKey>:<path> of a directory ('.' for the root), relative to that root (required)."`
	Offset         int    `json:"offset,omitempty" jsonschema:"0-based first entry; default 0."`
	Limit          int    `json:"limit,omitempty" jsonschema:"Entries to list, 1-1000; default 200."`
	ExpectedSHA256 string `json:"expected_sha256,omitempty" jsonschema:"sha256 from the first page; a different current listing returns status changed."`
}

func contextQuery(reference string, offset, limit int, expected string) string {
	q := url.Values{}
	q.Set("reference", reference)
	if offset != 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if limit != 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if expected != "" {
		q.Set("expected_sha256", expected)
	}
	return q.Encode()
}

func (s *Server) handleReadContext(ctx context.Context, _ *mcp.CallToolRequest, input ReadContextInput) (*mcp.CallToolResult, domain.ContextFile, error) {
	if strings.TrimSpace(input.Reference) == "" {
		return nil, domain.ContextFile{}, newValidationError("reference", "cannot be empty")
	}
	var out domain.ContextFile
	if err := s.sessionRequest(ctx, "GET", "context/file?"+contextQuery(input.Reference, input.Offset, input.Limit, input.ExpectedSHA256), nil, &out); err != nil {
		return nil, domain.ContextFile{}, err
	}
	return nil, out, nil
}

func (s *Server) handleListContext(ctx context.Context, _ *mcp.CallToolRequest, input ListContextInput) (*mcp.CallToolResult, domain.ContextListing, error) {
	if strings.TrimSpace(input.Reference) == "" {
		return nil, domain.ContextListing{}, newValidationError("reference", "cannot be empty")
	}
	var out domain.ContextListing
	if err := s.sessionRequest(ctx, "GET", "context/directory?"+contextQuery(input.Reference, input.Offset, input.Limit, input.ExpectedSHA256), nil, &out); err != nil {
		return nil, domain.ContextListing{}, err
	}
	return nil, out, nil
}
