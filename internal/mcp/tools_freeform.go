package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (s *Server) registerFreeformTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeSession",
		Description: "Conclude the freeform session. commits are optional, but if provided they must be an array of {sha, repo} objects.",
	}, s.handleConcludeSession)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "createWorkTicket",
		Description: "Create a new work ticket in the backlog. Returns the created Ticket.",
	}, s.handleCreateWorkTicket)
}
