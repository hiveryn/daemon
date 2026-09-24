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
	if sessionType != SessionTypeArchitect && sessionType != SessionTypeTicket && sessionType != SessionTypeAction {
		return nil, fmt.Errorf("unsupported HIVERYN_SESSION_TYPE %q", sessionType)
	}

	// Action sessions belong to no architect; every other role is scoped by one.
	architectKey := strings.TrimSpace(cfg.ArchitectKey)
	if sessionType == SessionTypeAction {
		if architectKey != "" {
			return nil, fmt.Errorf("action sessions do not take an architect key (got %q)", architectKey)
		}
	} else if architectKey == "" {
		return nil, fmt.Errorf("mcp server requires HIVERYN_ARCHITECT_KEY to be set")
	}

	// Every session type's tools are addressed by session id (intent-routed
	// createWorkTicket and conclude tools, and the action tools), so this is
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
	case SessionTypeAction:
		server.registerActionTools()
	}

	return server, nil
}

func (s *Server) Run() error {
	return s.mcpServer.Run(context.Background(), &mcp.StdioTransport{})
}

func (s *Server) SessionType() SessionType {
	return s.sessionType
}
