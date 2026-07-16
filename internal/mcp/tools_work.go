package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (s *Server) registerTicketTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeTicketSession",
		Description: "End the session and mark the ticket as done. The terminal is killed and the session cannot be resumed. Provide the structured fields (summary, outcome, implementation, follow_ups, open_questions); the daemon renders them into the conclusion, which is sent to the user for approval. outcome=completed requires commits (array of {sha, repo} objects, at least one) and implementation; outcome=exploratory requires implementation but no commits; outcome=rejected requires rejection_reason.",
	}, s.handleTicketConcludeSession)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readTicket",
		Description: "Read full ticket details by ID.",
	}, s.handleReadTicket)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "createWorkTicket",
		Description: "Create a new work ticket in the backlog. Returns the created Ticket.",
	}, s.handleCreateWorkTicket)

	s.registerBrowserTools()
}
