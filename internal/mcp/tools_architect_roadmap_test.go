package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestHandleReadRoadmapSuccess(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/roadmap" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("id"); got != "big-goal" {
			t.Fatalf("id = %q", got)
		}
		if got := r.URL.Query().Get("depth"); got != "1" {
			t.Fatalf("depth = %q", got)
		}
		writeEnvelope(t, w, http.StatusOK, ReadRoadmapOutput{
			View:    "current",
			Version: "v1",
			Items:   []domain.RoadmapItem{{ID: "big-goal", Kind: domain.RoadmapItemGoal}},
		})
	})

	depth := 1
	_, output, err := server.handleReadRoadmap(context.Background(), nil, ReadRoadmapInput{ID: "big-goal", Depth: &depth})
	if err != nil {
		t.Fatalf("handleReadRoadmap: %v", err)
	}
	if output.Version != "v1" || len(output.Items) != 1 || output.Items[0].ID != "big-goal" {
		t.Fatalf("output = %#v", output)
	}
}

func TestHandleReadRoadmapValidation(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn", SessionID: "sess-test"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	cases := []ReadRoadmapInput{
		{View: "bogus"},
		{View: "archive", Depth: intPtr(1)},
		{Depth: intPtr(-1)},
	}
	for _, input := range cases {
		_, _, err := server.handleReadRoadmap(context.Background(), nil, input)
		assertValidationToolError(t, err)
	}
}

// decodeParams asserts a PUT to the roadmap endpoint and returns the decoded
// batch body the tool built.
func roadmapMutationServer(t *testing.T, capture *domain.UpdateRoadmapParams) *Server {
	t.Helper()
	return newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s", r.Method)
		}
		if r.URL.Path != "/api/architects/hiveryn/roadmap" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(capture); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeEnvelope(t, w, http.StatusOK, UpdateRoadmapOutput{Version: "v2"})
	})
}

func TestHandleCreateRoadmapItemSendsOneOp(t *testing.T) {
	t.Parallel()

	var params domain.UpdateRoadmapParams
	server := roadmapMutationServer(t, &params)

	order := 15
	_, output, err := server.handleCreateRoadmapItem(context.Background(), nil, CreateRoadmapItemInput{
		Version:         "v1",
		ID:              "big-goal",
		Kind:            "goal",
		Title:           "Big goal",
		Outcome:         "Something durable",
		Status:          "active",
		ParentID:        "",
		Order:           &order,
		SuccessCriteria: []string{"works"},
		DependsOn:       []string{"other-goal"},
	})
	if err != nil {
		t.Fatalf("handleCreateRoadmapItem: %v", err)
	}
	if output.Version != "v2" {
		t.Fatalf("output = %#v", output)
	}
	if params.Version != "v1" || params.Title != nil || len(params.Ops) != 1 {
		t.Fatalf("params = %#v", params)
	}
	op := params.Ops[0]
	if op.Type != domain.RoadmapOpCreate || op.ID != "big-goal" || op.Kind != domain.RoadmapItemGoal ||
		op.Title == nil || *op.Title != "Big goal" || op.Status == nil || *op.Status != domain.RoadmapStatusActive ||
		op.ParentID != nil || op.Order == nil || *op.Order != 15 ||
		op.SuccessCriteria == nil || len(*op.SuccessCriteria) != 1 ||
		op.DependsOn == nil || (*op.DependsOn)[0] != "other-goal" {
		t.Fatalf("op = %#v", op)
	}
}

func TestHandleLinkAndUnlinkRoadmapTicket(t *testing.T) {
	t.Parallel()

	var params domain.UpdateRoadmapParams
	server := roadmapMutationServer(t, &params)

	_, _, err := server.handleLinkRoadmapTicket(context.Background(), nil, LinkRoadmapTicketInput{
		Version: "v1", ID: "big-goal", TicketID: "2026-01-01-0000-x",
	})
	if err != nil {
		t.Fatalf("handleLinkRoadmapTicket: %v", err)
	}
	if len(params.Ops) != 1 || params.Ops[0].Type != domain.RoadmapOpLinkTicket || params.Ops[0].TicketID != "2026-01-01-0000-x" {
		t.Fatalf("params = %#v", params)
	}

	_, _, err = server.handleUnlinkRoadmapTicket(context.Background(), nil, UnlinkRoadmapTicketInput{
		Version: "v1", ID: "big-goal", TicketID: "2026-01-01-0000-x",
	})
	if err != nil {
		t.Fatalf("handleUnlinkRoadmapTicket: %v", err)
	}
	if params.Ops[0].Type != domain.RoadmapOpUnlinkTicket {
		t.Fatalf("params = %#v", params)
	}
}

func TestHandleSetRoadmapTitleSendsTitleOnly(t *testing.T) {
	t.Parallel()

	var params domain.UpdateRoadmapParams
	server := roadmapMutationServer(t, &params)

	_, _, err := server.handleSetRoadmapTitle(context.Background(), nil, SetRoadmapTitleInput{Version: "v1", Title: "Hiveryn roadmap"})
	if err != nil {
		t.Fatalf("handleSetRoadmapTitle: %v", err)
	}
	if params.Title == nil || *params.Title != "Hiveryn roadmap" || len(params.Ops) != 0 {
		t.Fatalf("params = %#v", params)
	}
}

func TestHandleReadRoadmapForwardsTrimmedInput(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("view"); got != "archive" {
			t.Fatalf("view = %q, want trimmed archive", got)
		}
		if got := r.URL.Query().Get("id"); got != "big-goal" {
			t.Fatalf("id = %q, want trimmed big-goal", got)
		}
		writeEnvelope(t, w, http.StatusOK, ReadRoadmapOutput{View: "archive", Version: "v1"})
	})

	if _, _, err := server.handleReadRoadmap(context.Background(), nil, ReadRoadmapInput{View: " archive ", ID: " big-goal "}); err != nil {
		t.Fatalf("handleReadRoadmap: %v", err)
	}
}

func TestHandleMoveRoadmapItemParentTriState(t *testing.T) {
	t.Parallel()

	var params domain.UpdateRoadmapParams
	server := roadmapMutationServer(t, &params)

	// Omitted parent_id: pure reorder, nil on the wire keeps the parent.
	_, _, err := server.handleMoveRoadmapItem(context.Background(), nil, MoveRoadmapItemInput{Version: "v1", ID: "x", Order: intPtr(1)})
	if err != nil {
		t.Fatalf("move reorder: %v", err)
	}
	if params.Ops[0].ParentID != nil {
		t.Fatalf("omitted parent_id must stay nil on the wire, got %#v", params.Ops[0].ParentID)
	}

	// Explicit empty string: move to top level.
	empty := ""
	_, _, err = server.handleMoveRoadmapItem(context.Background(), nil, MoveRoadmapItemInput{Version: "v1", ID: "x", ParentID: &empty})
	if err != nil {
		t.Fatalf("move to root: %v", err)
	}
	if params.Ops[0].ParentID == nil || *params.Ops[0].ParentID != "" {
		t.Fatalf("explicit empty parent_id must reach the wire, got %#v", params.Ops[0].ParentID)
	}
}

func TestHandleRestoreRoadmapItemParentTriState(t *testing.T) {
	t.Parallel()

	var params domain.UpdateRoadmapParams
	server := roadmapMutationServer(t, &params)

	// Omitted parent_id: restore to the original location.
	_, _, err := server.handleRestoreRoadmapItem(context.Background(), nil, RestoreRoadmapItemInput{Version: "v1", ID: "big-goal"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if params.Ops[0].ParentID != nil {
		t.Fatalf("omitted parent_id must stay nil on the wire, got %#v", params.Ops[0].ParentID)
	}

	// Explicit empty string: restore to the top level.
	empty := ""
	_, _, err = server.handleRestoreRoadmapItem(context.Background(), nil, RestoreRoadmapItemInput{Version: "v1", ID: "big-goal", ParentID: &empty})
	if err != nil {
		t.Fatalf("restore to root: %v", err)
	}
	if params.Ops[0].ParentID == nil || *params.Ops[0].ParentID != "" {
		t.Fatalf("explicit empty parent_id must reach the wire, got %#v", params.Ops[0].ParentID)
	}
}

func TestRoadmapMutationLocalValidation(t *testing.T) {
	t.Parallel()

	server, err := NewServer(Config{DaemonURL: "http://127.0.0.1:4200", ArchitectKey: "hiveryn", SessionID: "sess-test"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"create blank version", func() error {
			_, _, err := server.handleCreateRoadmapItem(ctx, nil, CreateRoadmapItemInput{ID: "x", Kind: "goal", Title: "T", Outcome: "O"})
			return err
		}},
		{"create blank id", func() error {
			_, _, err := server.handleCreateRoadmapItem(ctx, nil, CreateRoadmapItemInput{Version: "v1", Kind: "goal", Title: "T", Outcome: "O"})
			return err
		}},
		{"update no fields", func() error {
			_, _, err := server.handleUpdateRoadmapItem(ctx, nil, UpdateRoadmapItemInput{Version: "v1", ID: "x"})
			return err
		}},
		{"move blank version", func() error {
			_, _, err := server.handleMoveRoadmapItem(ctx, nil, MoveRoadmapItemInput{ID: "x"})
			return err
		}},
		{"link blank ticket", func() error {
			_, _, err := server.handleLinkRoadmapTicket(ctx, nil, LinkRoadmapTicketInput{Version: "v1", ID: "x"})
			return err
		}},
		{"archive blank id", func() error {
			_, _, err := server.handleArchiveRoadmapItem(ctx, nil, ArchiveRoadmapItemInput{Version: "v1"})
			return err
		}},
		{"restore blank version", func() error {
			_, _, err := server.handleRestoreRoadmapItem(ctx, nil, RestoreRoadmapItemInput{ID: "x"})
			return err
		}},
		{"set title blank title", func() error {
			_, _, err := server.handleSetRoadmapTitle(ctx, nil, SetRoadmapTitleInput{Version: "v1", Title: " "})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertValidationToolError(t, tc.call())
		})
	}
}

func assertValidationToolError(t *testing.T, err error) {
	t.Helper()
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T (%v)", err, err)
	}
	if toolErr.Code != ErrorCodeValidation {
		t.Fatalf("code = %q", toolErr.Code)
	}
}

func TestRoadmapMutationMapsConflict(t *testing.T) {
	t.Parallel()

	server := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(t, w, http.StatusConflict, &domain.ErrorBody{
			Code:    string(domain.ErrCodeConflict),
			Message: "roadmap changed since you read it",
		})
	})

	_, _, err := server.handleSetRoadmapTitle(context.Background(), nil, SetRoadmapTitleInput{Version: "stale", Title: "T"})
	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("expected *ToolError, got %T", err)
	}
	if toolErr.Code != ErrorCodeStateConflict {
		t.Fatalf("code = %q, want %q", toolErr.Code, ErrorCodeStateConflict)
	}
}

func intPtr(i int) *int { return &i }
