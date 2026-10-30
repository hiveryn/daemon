package sessionruntime

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
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
	// A remote ticket worker gets exactly the worker tools of its project, a
	// remote companion exactly the companion tools — never architect or
	// worker authority it would inherit from the other role.
	var role daemonmcp.SessionType
	switch session.SessionType {
	case domain.SessionTypeTicket:
		role = daemonmcp.SessionTypeTicket
	case domain.SessionTypeCompanion:
		role = daemonmcp.SessionTypeCompanion
	}
	if len(session.RemoteToken) != 64 || role == "" {
		return nil, 0, fmt.Errorf("remote gateway requires a ticket or companion session and a 256-bit token")
	}
	server, err := daemonmcp.NewServer(daemonmcp.Config{DaemonURL: s.baseURL, ArchitectKey: session.ArchitectKey, SessionType: role, SessionID: session.ID, Logger: s.logger})
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
			s.serveGatewayMCP(w, r, session, mcpHandler)
			return
		}
		if !hook || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == hookPrefix+"claude/wait" {
			s.serveRemoteWaiter(w, r, session)
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

// serveGatewayMCP serves one /mcp request. A companion's stop waits for the
// acknowledgement of its completeCompanionShutdown (companions.go): it is
// released only by the response of the request whose tool call the daemon
// accepted, for the bound request_id, once that response was written and
// flushed into the tunnel without error while its caller was still
// connected. Any other response — another tool call, an error, a refused
// request_id — and a response the transport lost release nothing; the stop
// then waits out its bounded wait and reports that.
func (s *Service) serveGatewayMCP(w http.ResponseWriter, r *http.Request, session domain.Session, mcpHandler http.Handler) {
	if session.SessionType != domain.SessionTypeCompanion || r.Method != http.MethodPost {
		mcpHandler.ServeHTTP(w, r)
		return
	}
	ctx, completion := daemonmcp.WithShutdownCompletion(r.Context())
	rw := &observedResponse{ResponseWriter: w, status: http.StatusOK}
	mcpHandler.ServeHTTP(rw, r.WithContext(ctx))
	requestID := completion.Accepted()
	if requestID == "" {
		return
	}
	if !rw.written() || http.NewResponseController(rw).Flush() != nil || rw.failed() || r.Context().Err() != nil {
		s.logger.Warn("companion shutdown acknowledgement not written; the transport lost it", "session_id", session.ID, "request_id", requestID, "transport", companionAckGateway, "status", rw.status, "write_error", rw.err)
		return
	}
	if s.companionRT.acknowledged(session.ID, requestID, companionAckGateway) {
		s.logger.Info("companion shutdown acknowledgement written", "session_id", session.ID, "request_id", requestID, "transport", companionAckGateway)
	}
}

// observedResponse records whether a response was written successfully.
type observedResponse struct {
	http.ResponseWriter
	status int
	wrote  bool
	err    error
}

func (o *observedResponse) WriteHeader(status int) {
	o.status = status
	o.ResponseWriter.WriteHeader(status)
}

func (o *observedResponse) Write(b []byte) (int, error) {
	n, err := o.ResponseWriter.Write(b)
	if n > 0 {
		o.wrote = true
	}
	if err != nil && o.err == nil {
		o.err = err
	}
	return n, err
}

// Flush keeps streaming responses flushable through the wrapper.
func (o *observedResponse) Flush() {
	if err := http.NewResponseController(o.ResponseWriter).Flush(); err != nil && o.err == nil {
		o.err = err
	}
}

func (o *observedResponse) Unwrap() http.ResponseWriter { return o.ResponseWriter }

func (o *observedResponse) written() bool { return o.wrote && o.status == http.StatusOK }

func (o *observedResponse) failed() bool { return o.err != nil }

// serveRemoteWaiter admits a remote Claude worker's asyncRewake waiter (its
// long poll arrives through the reverse tunnel at the token-scoped hook path)
// into the daemon's mailbox. The waiter is bound to this gateway's session: an
// envelope naming another session is refused, so one worker can never arm or
// receive another session's input. The body is never logged.
func (s *Service) serveRemoteWaiter(w http.ResponseWriter, r *http.Request, session domain.Session) {
	if s.messages == nil {
		// The waiter exits 0 on 404; nothing can be delivered.
		http.NotFound(w, r)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, "invalid waiter body", http.StatusBadRequest)
		return
	}
	var envelope struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		http.Error(w, "invalid waiter body", http.StatusBadRequest)
		return
	}
	if envelope.Env["AGENTRUNTIME_SESSION_ID"] != session.ID {
		http.Error(w, "waiter session mismatch", http.StatusBadRequest) // 400: the waiter exits instead of retrying
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(b))
	s.messages.mailbox.Handler().ServeHTTP(w, r2)
}
