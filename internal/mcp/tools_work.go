package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (s *Server) registerWorkTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "concludeSession",
		Description: "Conclude the session and mark the ticket as done. commits is required (at least one SHA). If no commits were produced, set rejected=true and provide a rejection_reason.",
	}, s.handleConcludeSession)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readTicket",
		Description: "Read full ticket details by ID.",
	}, s.handleReadTicket)
}
