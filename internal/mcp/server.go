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
	HTTPClient   *http.Client
	Logger       *slog.Logger
}

type Server struct {
	mcpServer    *mcp.Server
	httpClient   *http.Client
	architectKey string
	daemonURL    string
	sessionType  SessionType
	logger       *slog.Logger
}

func NewServer(cfg Config) (*Server, error) {
	daemonURL := strings.TrimRight(strings.TrimSpace(cfg.DaemonURL), "/")
	if daemonURL == "" {
		return nil, fmt.Errorf("mcp server requires HIVERYN_DAEMON_URL to be set")
	}
	architectKey := strings.TrimSpace(cfg.ArchitectKey)
	if architectKey == "" {
		return nil, fmt.Errorf("mcp server requires HIVERYN_ARCHITECT_KEY to be set")
	}

	sessionType := cfg.SessionType
	if sessionType == "" {
		sessionType = SessionTypeArchitect
	}
	if sessionType != SessionTypeArchitect && sessionType != SessionTypeWork {
		return nil, fmt.Errorf("unsupported HIVERYN_SESSION_TYPE %q", sessionType)
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
		logger:       cfg.Logger,
	}
	if server.httpClient == nil {
		server.httpClient = http.DefaultClient
	}

	switch sessionType {
	case SessionTypeArchitect:
		server.registerArchitectTools()
	case SessionTypeWork:
		server.registerWorkTools()
	}

	return server, nil
}

func (s *Server) Run() error {
	return s.mcpServer.Run(context.Background(), &mcp.StdioTransport{})
}

func (s *Server) SessionType() SessionType {
	return s.sessionType
}
