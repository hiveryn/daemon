package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (s *Server) registerTicketTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeSession",
		Description: "End the session and mark the ticket as done. The terminal is killed and the session cannot be resumed. The conclusion is sent to the user for approval. commits is required and must be an array of {sha, repo} objects. If no commits were produced, set rejected=true and provide a rejection_reason.",
	}, s.handleConcludeSession)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readTicket",
		Description: "Read full ticket details by ID.",
	}, s.handleReadTicket)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "createWorkTicket",
		Description: "Create a new work ticket in the backlog. Returns the created Ticket.",
	}, s.handleCreateWorkTicket)
}
