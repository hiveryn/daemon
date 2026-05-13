package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

func (s *Server) registerWorkTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "readTicket",
		Description: "Read full ticket details by ID.",
	}, s.handleReadTicket)
}
