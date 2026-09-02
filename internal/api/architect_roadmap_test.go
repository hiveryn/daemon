package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/architectfs"
	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

// newRoadmapTestHandler wires a reloading handler over a single architect
// with an empty temp workspace and a real filesystem ticket service.
func newRoadmapTestHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	configDir := t.TempDir()
	workspace := t.TempDir()
	cfgPath := writeReloadingConfigFiles(t, configDir, map[string]config.ArchitectConfig{
		"hiveryn": {Name: "Hiveryn", Path: workspace},
	})
	return newReloadingTestHandler(t, cfgPath, architectfs.NewTicketService()), workspace
}

func readRoadmap(t *testing.T, handler http.Handler, query string) domain.RoadmapView {
	t.Helper()
	status, body := request(t, handler, http.MethodGet, "/api/architects/hiveryn/roadmap"+query, nil)
	if status != http.StatusOK {
		t.Fatalf("read status = %d, body: %s", status, body)
	}
	var view domain.RoadmapView
	decodeEnvelopeData(t, body, &view)
	return view
}

func TestRoadmapUnknownArchitect(t *testing.T) {
	t.Parallel()
	handler, _ := newRoadmapTestHandler(t)
	status, _ := request(t, handler, http.MethodGet, "/api/architects/nope/roadmap", nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

func TestRoadmapReadEmptyAndUpdateFlow(t *testing.T) {
	t.Parallel()
	handler, _ := newRoadmapTestHandler(t)

	view := readRoadmap(t, handler, "")
	if view.Version == "" || len(view.Items) != 0 {
		t.Fatalf("empty view = %#v", view)
	}

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/roadmap", domain.UpdateRoadmapParams{
		Version: view.Version,
		Ops: []domain.RoadmapOp{{
			Type:    domain.RoadmapOpCreate,
			ID:      "first-goal",
			Kind:    domain.RoadmapItemGoal,
			Title:   ptr("First goal"),
			Outcome: ptr("Something durable"),
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("update status = %d, body: %s", status, body)
	}
	var result domain.RoadmapUpdateResult
	decodeEnvelopeData(t, body, &result)
	if result.Version == view.Version || len(result.Applied) != 1 {
		t.Fatalf("result = %#v", result)
	}

	after := readRoadmap(t, handler, "?id=first-goal&depth=0")
	if len(after.Items) != 1 || after.Items[0].ID != "first-goal" {
		t.Fatalf("focused read = %#v", after.Items)
	}
}

func TestRoadmapStaleVersionConflicts(t *testing.T) {
	t.Parallel()
	handler, _ := newRoadmapTestHandler(t)

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/roadmap", domain.UpdateRoadmapParams{
		Version: "stale",
		Ops: []domain.RoadmapOp{{
			Type: domain.RoadmapOpCreate, ID: "x", Kind: domain.RoadmapItemGoal,
			Title: ptr("T"), Outcome: ptr("O"),
		}},
	})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != string(domain.ErrCodeConflict) {
		t.Fatalf("code = %q", errBody.Code)
	}
}

func TestRoadmapBadOpNamesIndex(t *testing.T) {
	t.Parallel()
	handler, _ := newRoadmapTestHandler(t)
	view := readRoadmap(t, handler, "")

	status, body := requestJSON(t, handler, http.MethodPut, "/api/architects/hiveryn/roadmap", domain.UpdateRoadmapParams{
		Version: view.Version,
		Ops: []domain.RoadmapOp{
			{Type: domain.RoadmapOpCreate, ID: "ok", Kind: domain.RoadmapItemGoal, Title: ptr("T"), Outcome: ptr("O")},
			{Type: domain.RoadmapOpCreate, ID: "bad", Kind: "epic", Title: ptr("T"), Outcome: ptr("O")},
		},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, body: %s", status, body)
	}
	errBody := decodeEnvelopeError(t, body)
	if errBody.Code != string(domain.ErrCodeValidation) || !strings.Contains(errBody.Message, "ops[1]") {
		t.Fatalf("error = %#v", errBody)
	}
	after := readRoadmap(t, handler, "")
	if len(after.Items) != 0 {
		t.Fatalf("failed batch must write nothing, items = %#v", after.Items)
	}
}

func TestRoadmapQueryValidation(t *testing.T) {
	t.Parallel()
	handler, _ := newRoadmapTestHandler(t)

	status, _ := request(t, handler, http.MethodGet, "/api/architects/hiveryn/roadmap?depth=abc", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("non-integer depth status = %d", status)
	}
	status, _ = request(t, handler, http.MethodGet, "/api/architects/hiveryn/roadmap?view=bogus", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bogus view status = %d", status)
	}
	view := readRoadmap(t, handler, "?view=archive")
	if view.View != "archive" || len(view.ArchiveEntries) != 0 {
		t.Fatalf("archive view = %#v", view)
	}
}

func ptr[T any](v T) *T { return &v }
