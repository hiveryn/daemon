package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Config struct {
	DaemonURL    string
	ArchitectKey string
	SessionType  SessionType
	SessionID    string
	HTTPClient   *http.Client
	Logger       *slog.Logger
}

type Server struct {
	mcpServer    *mcp.Server
	httpClient   *http.Client
	architectKey string
	daemonURL    string
	sessionType  SessionType
	sessionID    string
	logger       *slog.Logger
}

func NewServer(cfg Config) (*Server, error) {
	daemonURL := strings.TrimRight(strings.TrimSpace(cfg.DaemonURL), "/")
	if daemonURL == "" {
		return nil, fmt.Errorf("mcp server requires HIVERYN_DAEMON_URL to be set")
	}
	sessionType := cfg.SessionType
	if sessionType == "" {
		sessionType = SessionTypeArchitect
	}
	if sessionType != SessionTypeArchitect && sessionType != SessionTypeTicket {
		return nil, fmt.Errorf("unsupported HIVERYN_SESSION_TYPE %q", sessionType)
	}

	architectKey := strings.TrimSpace(cfg.ArchitectKey)
	if architectKey == "" {
		return nil, fmt.Errorf("mcp server requires HIVERYN_ARCHITECT_KEY to be set")
	}

	// Every session type's tools are addressed by session id (intent-routed
	// worker-spawn and conclude tools, questions and notifications), so this is
	// required up front rather than checked lazily per handler. Not
	// role-conditional on purpose.
	if strings.TrimSpace(cfg.SessionID) == "" {
		return nil, fmt.Errorf("mcp server requires HIVERYN_SESSION_ID to be set")
	}

	server := &Server{
		mcpServer: mcp.NewServer(&mcp.Implementation{
			Name:    "hiveryn-daemon-mcp",
			Version: "dev",
		}, nil),
		httpClient:   cfg.HTTPClient,
		architectKey: architectKey,
		daemonURL:    daemonURL,
		sessionType:  sessionType,
		sessionID:    cfg.SessionID,
		logger:       cfg.Logger,
	}
	if server.httpClient == nil {
		server.httpClient = http.DefaultClient
	}

	switch sessionType {
	case SessionTypeArchitect:
		server.registerArchitectTools()
	case SessionTypeTicket:
		server.registerTicketTools()
	}
	server.registerUserTools()

	return server, nil
}

func (s *Server) Run() error {
	return s.mcpServer.Run(context.Background(), &mcp.StdioTransport{})
}

func (s *Server) SessionType() SessionType {
	return s.sessionType
}

// HTTPHandler exposes the same fixed role/session tool registry as stdio.
// Authentication and listener scoping belong to the caller.
func (s *Server) HTTPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcpServer }, &mcp.StreamableHTTPOptions{Stateless: true})
}
