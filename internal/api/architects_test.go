package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

func TestArchitectsReadOnlyAPI(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/architects", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected architect list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		Architects []architectResponse `json:"architects"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.Architects) != 2 {
		t.Fatalf("expected 2 architects, got %d", len(listed.Architects))
	}
	if listed.Architects[0].Key != "hiveryn" {
		t.Fatalf("expected sorted architect keys, got %#v", listed.Architects)
	}
	if len(listed.Architects[0].Repos) != 2 {
		t.Fatalf("expected 2 repos on architect list payload, got %#v", listed.Architects[0])
	}
	if listed.Architects[0].Repos[0].Key != "daemon" || listed.Architects[0].Repos[1].Key != "desktop" {
		t.Fatalf("expected sorted repos on architect list payload, got %#v", listed.Architects[0].Repos)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected architect get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var architect architectResponse
	decodeEnvelopeData(t, getBody, &architect)
	if architect.Key != "hiveryn" || architect.Name != "Hiveryn" {
		t.Fatalf("unexpected architect payload: %#v", architect)
	}
	if len(architect.Repos) != 2 {
		t.Fatalf("expected 2 repos on architect detail, got %#v", architect.Repos)
	}
}

func TestReposReadOnlyAPI(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	listStatus, listBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected repo list status %d, got %d: %s", http.StatusOK, listStatus, string(listBody))
	}

	var listed struct {
		Repos []repoResponse `json:"repos"`
	}
	decodeEnvelopeData(t, listBody, &listed)
	if len(listed.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(listed.Repos))
	}
	if listed.Repos[0].Key != "daemon" {
		t.Fatalf("expected sorted repos, got %#v", listed.Repos)
	}

	getStatus, getBody := request(t, handler, http.MethodGet, "/api/architects/hiveryn/repos/desktop", nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected repo get status %d, got %d: %s", http.StatusOK, getStatus, string(getBody))
	}

	var repo repoResponse
	decodeEnvelopeData(t, getBody, &repo)
	if repo.Key != "desktop" || repo.Path == "" {
		t.Fatalf("unexpected repo payload: %#v", repo)
	}
}

func TestArchitectMutationEndpointsRemoved(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	cases := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/architects"},
		{method: http.MethodPatch, path: "/api/architects/hiveryn"},
		{method: http.MethodDelete, path: "/api/architects/hiveryn"},
		{method: http.MethodPost, path: "/api/architects/hiveryn/repos"},
		{method: http.MethodPatch, path: "/api/architects/hiveryn/repos/daemon"},
		{method: http.MethodDelete, path: "/api/architects/hiveryn/repos/daemon"},
	}

	for _, tc := range cases {
		status, _ := requestJSON(t, handler, tc.method, tc.path, map[string]any{"ignored": true})
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("expected %s %s to return %d, got %d", tc.method, tc.path, http.StatusMethodNotAllowed, status)
		}
	}
}

func TestArchitectAndRepoNotFound(t *testing.T) {
	t.Parallel()

	handler := newTestHandler(t)

	cases := []string{
		"/api/architects/missing",
		"/api/architects/missing/repos",
		"/api/architects/hiveryn/repos/missing",
	}

	for _, path := range cases {
		status, body := request(t, handler, http.MethodGet, path, nil)
		if status != http.StatusNotFound {
			t.Fatalf("expected %s to return %d, got %d: %s", path, http.StatusNotFound, status, string(body))
		}
	}
}

func TestArchitectsAPIReloadsArchitectsFile(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	hiverynPath := t.TempDir()
	lithoPath := t.TempDir()
	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
	})
	handler := newReloadingTestHandler(t, cfgPath, nil)

	status, body := request(t, handler, http.MethodGet, "/api/architects", nil)
	if status != http.StatusOK {
		t.Fatalf("expected initial status %d, got %d: %s", http.StatusOK, status, string(body))
	}
	var initial struct {
		Architects []architectResponse `json:"architects"`
	}
	decodeEnvelopeData(t, body, &initial)
	if len(initial.Architects) != 1 || initial.Architects[0].Key != "hiveryn" {
		t.Fatalf("unexpected initial architects: %#v", initial.Architects)
	}

	writeAPIArchitects(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
		"litho": {
			Path:  lithoPath,
			Repos: map[string]string{"app": "/tmp/lithoapp"},
		},
	})

	status, body = request(t, handler, http.MethodGet, "/api/architects", nil)
	if status != http.StatusOK {
		t.Fatalf("expected reloaded status %d, got %d: %s", http.StatusOK, status, string(body))
	}
	var reloaded struct {
		Architects []architectResponse `json:"architects"`
	}
	decodeEnvelopeData(t, body, &reloaded)
	if len(reloaded.Architects) != 2 {
		t.Fatalf("expected 2 architects after reload, got %#v", reloaded.Architects)
	}
	if reloaded.Architects[1].Key != "litho" {
		t.Fatalf("expected litho architect after reload, got %#v", reloaded.Architects)
	}
}

func TestArchitectsStatusAPI(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	hiverynPath := t.TempDir()
	lithoPath := t.TempDir()
	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
		"litho": {
			Path:  lithoPath,
			Repos: map[string]string{"app": "/tmp/app"},
		},
	})

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := config.NewReloadingSource(cfgPath, cfg)
	if err != nil {
		t.Fatalf("create config source: %v", err)
	}

	ticketService := architectfs.NewTicketService()
	ticket, err := ticketService.CreateTicket(context.Background(), hiverynPath, domain.CreateTicketParams{
		Title: "Add status endpoint",
		Body:  "Describe the endpoint.",
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	architectStartedAt := time.Date(2026, 6, 7, 18, 20, 0, 0, time.UTC)
	ticketStartedAt := time.Date(2026, 6, 7, 18, 21, 42, 123456000, time.UTC)
	freeformStartedAt := time.Date(2026, 6, 7, 18, 25, 10, 0, time.UTC)
	service := &fakeSessionService{
		sessions: []domain.Session{
			{
				ID:           "architect-session",
				ArchitectKey: "hiveryn",
				SessionType:  domain.SessionTypeArchitect,
				ContextID:    "hiveryn",
				CurrentRun: &domain.SessionRun{
					ID:          "run-architect",
					Status:      domain.SessionRunStatusRunning,
					AgentStatus: domain.AgentStatusActive,
					StartedAt:   &architectStartedAt,
				},
			},
			{
				ID:           "ticket-session",
				ArchitectKey: "hiveryn",
				SessionType:  domain.SessionTypeTicket,
				ContextID:    ticket.ID,
				CurrentRun: &domain.SessionRun{
					ID:          "run-ticket",
					Status:      domain.SessionRunStatusRunning,
					AgentStatus: domain.AgentStatusActive,
					StartedAt:   &ticketStartedAt,
				},
			},
			{
				ID:           "freeform-session",
				ArchitectKey: "hiveryn",
				SessionType:  domain.SessionTypeFreeform,
				ContextID:    "2026-06-07-1825-investigate-login-failure",
				CurrentRun: &domain.SessionRun{
					ID:          "run-freeform",
					Status:      domain.SessionRunStatusRunning,
					AgentStatus: domain.AgentStatusIdle,
					StartedAt:   &freeformStartedAt,
				},
			},
			{
				ID:           "completed-session",
				ArchitectKey: "hiveryn",
				SessionType:  domain.SessionTypeTicket,
				ContextID:    "ignored",
				CurrentRun: &domain.SessionRun{
					ID:        "run-completed",
					Status:    domain.SessionRunStatusCompleted,
					StartedAt: &ticketStartedAt,
				},
			},
		},
	}

	handler := NewHandler(Dependencies{
		Config:       cfg,
		ConfigSource: source,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions:     service,
		Tickets:      ticketService,
	})

	status, body := request(t, handler, http.MethodGet, "/api/architects/status", nil)
	if status != http.StatusOK {
		t.Fatalf("expected architect status %d, got %d: %s", http.StatusOK, status, string(body))
	}

	var payload struct {
		Architects []architectStatusResponse `json:"architects"`
	}
	decodeEnvelopeData(t, body, &payload)
	if len(payload.Architects) != 2 {
		t.Fatalf("expected 2 architects, got %#v", payload.Architects)
	}

	hiveryn := payload.Architects[0]
	if hiveryn.Key != "hiveryn" || hiveryn.Path != hiverynPath {
		t.Fatalf("unexpected hiveryn architect payload %#v", hiveryn)
	}
	if hiveryn.Status == nil || *hiveryn.Status != domain.AgentStatusActive {
		t.Fatalf("expected running architect status, got %#v", hiveryn.Status)
	}
	if len(hiveryn.Sessions) != 2 {
		t.Fatalf("expected 2 running worker sessions, got %#v", hiveryn.Sessions)
	}
	if hiveryn.Sessions[0].ID != "ticket-session" || hiveryn.Sessions[0].Title != "Add status endpoint" {
		t.Fatalf("unexpected first worker session %#v", hiveryn.Sessions[0])
	}
	if hiveryn.Sessions[0].Status != domain.SessionRunStatusRunning || hiveryn.Sessions[0].AgentStatus != domain.AgentStatusActive {
		t.Fatalf("unexpected ticket worker status %#v", hiveryn.Sessions[0])
	}
	if !hiveryn.Sessions[0].StartedAt.Equal(ticketStartedAt) {
		t.Fatalf("unexpected ticket started_at %#v", hiveryn.Sessions[0].StartedAt)
	}
	if hiveryn.Sessions[1].ID != "freeform-session" || hiveryn.Sessions[1].Title != "Investigate login failure" {
		t.Fatalf("unexpected freeform worker session %#v", hiveryn.Sessions[1])
	}
	if hiveryn.Sessions[1].AgentStatus != domain.AgentStatusIdle {
		t.Fatalf("unexpected freeform agent status %#v", hiveryn.Sessions[1])
	}
	if !hiveryn.Sessions[1].StartedAt.Equal(freeformStartedAt) {
		t.Fatalf("unexpected freeform started_at %#v", hiveryn.Sessions[1].StartedAt)
	}

	litho := payload.Architects[1]
	if litho.Key != "litho" || litho.Path != lithoPath {
		t.Fatalf("unexpected litho architect payload %#v", litho)
	}
	if litho.Status != nil {
		t.Fatalf("expected nil status for inactive architect, got %#v", litho.Status)
	}
	if len(litho.Sessions) != 0 {
		t.Fatalf("expected no worker sessions for inactive architect, got %#v", litho.Sessions)
	}
}

func TestArchitectsStatusAPIReloadsArchitectsFile(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	hiverynPath := t.TempDir()
	lithoPath := t.TempDir()
	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
	})

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := config.NewReloadingSource(cfgPath, cfg)
	if err != nil {
		t.Fatalf("create config source: %v", err)
	}
	handler := NewHandler(Dependencies{
		Config:       cfg,
		ConfigSource: source,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions:     &fakeSessionService{},
		Tickets:      &fakeTicketService{},
	})

	status, body := request(t, handler, http.MethodGet, "/api/architects/status", nil)
	if status != http.StatusOK {
		t.Fatalf("expected initial status %d, got %d: %s", http.StatusOK, status, string(body))
	}
	var initial struct {
		Architects []architectStatusResponse `json:"architects"`
	}
	decodeEnvelopeData(t, body, &initial)
	if len(initial.Architects) != 1 || initial.Architects[0].Key != "hiveryn" {
		t.Fatalf("unexpected initial architect status payload %#v", initial.Architects)
	}

	writeAPIArchitects(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
		"litho": {
			Path:  lithoPath,
			Repos: map[string]string{"app": "/tmp/lithoapp"},
		},
	})

	status, body = request(t, handler, http.MethodGet, "/api/architects/status", nil)
	if status != http.StatusOK {
		t.Fatalf("expected reloaded status %d, got %d: %s", http.StatusOK, status, string(body))
	}
	var reloaded struct {
		Architects []architectStatusResponse `json:"architects"`
	}
	decodeEnvelopeData(t, body, &reloaded)
	if len(reloaded.Architects) != 2 {
		t.Fatalf("expected 2 architects after reload, got %#v", reloaded.Architects)
	}
	if reloaded.Architects[1].Key != "litho" || reloaded.Architects[1].Status != nil || len(reloaded.Architects[1].Sessions) != 0 {
		t.Fatalf("unexpected reloaded architect payload %#v", reloaded.Architects[1])
	}
}

func TestArchitectsStatusAPIFailsWhenRunningTicketIsMissing(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	hiverynPath := t.TempDir()
	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {
			Path:  hiverynPath,
			Repos: map[string]string{"daemon": "/tmp/daemon"},
		},
	})

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	source, err := config.NewReloadingSource(cfgPath, cfg)
	if err != nil {
		t.Fatalf("create config source: %v", err)
	}
	startedAt := time.Date(2026, 6, 7, 18, 21, 42, 0, time.UTC)
	handler := NewHandler(Dependencies{
		Config:       cfg,
		ConfigSource: source,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sessions: &fakeSessionService{sessions: []domain.Session{{
			ID:           "ticket-session",
			ArchitectKey: "hiveryn",
			SessionType:  domain.SessionTypeTicket,
			ContextID:    "missing-ticket",
			CurrentRun: &domain.SessionRun{
				ID:        "run-ticket",
				Status:    domain.SessionRunStatusRunning,
				StartedAt: &startedAt,
			},
		}}},
		Tickets: architectfs.NewTicketService(),
	})

	status, body := request(t, handler, http.MethodGet, "/api/architects/status", nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected missing ticket to fail with %d, got %d: %s", http.StatusNotFound, status, string(body))
	}
	errBody := decodeEnvelopeError(t, body)
	if !strings.Contains(errBody.Message, "read ticket title for session ticket-session") {
		t.Fatalf("expected detailed ticket lookup error, got %#v", errBody)
	}
}
