package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (s *Server) registerFreeformTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeSession",
		Description: "End the freeform session. Records a summary of the session. The terminal is killed and the session cannot be resumed. The conclusion is sent to the user for approval. commits are optional, but if provided they must be an array of {sha, repo} objects.",
	}, s.handleConcludeSession)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "createWorkTicket",
		Description: "Create a new work ticket in the backlog. Returns the created Ticket.",
	}, s.handleCreateWorkTicket)
}
