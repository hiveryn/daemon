package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (s *Server) registerFreeformTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeFreeformSession",
		Description: "End the freeform session. The terminal is killed and the session cannot be resumed. Provide the structured fields (summary, findings, recommendations, open_questions); the daemon renders them into the conclusion, which is sent to the user for approval. commits are optional as an array of {sha, repo} objects.",
	}, s.handleFreeformConcludeSession)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "createWorkTicket",
		Description: "Create a new work ticket in the backlog. Returns the created Ticket.",
	}, s.handleCreateWorkTicket)

	s.registerBrowserTools()
}
