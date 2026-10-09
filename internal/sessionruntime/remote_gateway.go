package sessionruntime

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hiveryn/agentruntime"
	"github.com/hiveryn/daemon/internal/domain"
	daemonmcp "github.com/hiveryn/daemon/internal/mcp"
)

// The reverse tunnel terminates at this dedicated listener, never the daemon's
// API port. Each listener has one fixed worker identity and tool registry.
func (s *Service) remoteGateway(session domain.Session) (*http.Server, int, error) {
	if len(session.RemoteToken) != 64 || session.SessionType != domain.SessionTypeTicket {
		return nil, 0, fmt.Errorf("remote gateway requires a ticket session and a 256-bit token")
	}
	server, err := daemonmcp.NewServer(daemonmcp.Config{DaemonURL: s.baseURL, ArchitectKey: session.ArchitectKey, SessionType: daemonmcp.SessionTypeTicket, SessionID: session.ID, Logger: s.logger})
	if err != nil {
		return nil, 0, err
	}
	mcpHandler := server.HTTPHandler()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Hooks use an unguessable path because native hook transports do not
		// accept custom headers. Neither path nor headers enter request logging.
		hookPrefix := "/hooks/" + session.RemoteToken + "/"
		hook := strings.HasPrefix(r.URL.Path, hookPrefix)
		if !hook && subtle.ConstantTimeCompare([]byte(token), []byte(session.RemoteToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/mcp" {
			mcpHandler.ServeHTTP(w, r)
			return
		}
		if !hook || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		kind := agentruntime.AgentKind(strings.TrimPrefix(r.URL.Path, hookPrefix))
		if kind != agentruntime.AgentClaude && kind != agentruntime.AgentCodex && kind != agentruntime.AgentOpenCode {
			http.NotFound(w, r)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			http.Error(w, "invalid hook body", http.StatusBadRequest)
			return
		}
		event, err := s.adapters[kind].NormalizeEvent(r.Context(), b)
		if err != nil {
			http.Error(w, "invalid hook", http.StatusBadRequest)
			return
		}
		if event != nil && event.ID != session.ID {
			http.Error(w, "hook session mismatch", http.StatusForbidden)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = ingestPathPrefix + "/" + string(kind)
		r2.Body = io.NopCloser(strings.NewReader(string(b)))
		s.ingestHTTP.ServeHTTP(w, r2)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("worker gateway: %w", err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Error("remote worker gateway stopped", "session_id", session.ID, "error", err)
		}
	}()
	return httpServer, ln.Addr().(*net.TCPAddr).Port, nil
}
