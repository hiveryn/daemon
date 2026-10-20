package sessionruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestArtifactFreeActionCompletesWithoutOutputInstructions(t *testing.T) {
	f := newActionFixture(t)
	f.writeAction(t, "review", "name: review\ndescription: Review the request\nsuggestions: [Review this idea]\n", "Review {{prompt}}.")
	live := f.launch(t, "review", "a useful idea")
	if live.Run.OutputDir != "" || len(live.Session.AdditionalWorkdirs) != 0 || live.Run.ArtifactContract != "" {
		t.Fatalf("artifact-free launch: %+v", live)
	}
	for _, forbidden := range []string{"Output directory:", "deliverArtifacts", "Artifact contract:", "delivery_dir"} {
		if strings.Contains(live.Session.Prompt, forbidden) || strings.Contains(live.Session.Instructions, forbidden) {
			t.Fatalf("artifact-free instructions contain %q", forbidden)
		}
	}
	if _, err := f.service.DeliverActionArtifacts(context.Background(), live.Session.ID, t.TempDir()); err == nil || !strings.Contains(err.Error(), "did not declare artifacts") {
		t.Fatalf("delivery = %v", err)
	}
	run, err := f.conclude(t, live.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "Reviewed; no issues"})
	if err != nil || run.Status != domain.ActionRunCompleted || run.Package != nil {
		t.Fatalf("conclusion = %+v, %v", run, err)
	}
	artifacts, dir := f.service.callerArtifacts(context.Background(), domain.Session{}, run, 0)
	if artifacts.State != domain.ActionArtifactsNone || dir != "" {
		t.Fatalf("artifacts = %+v, %q", artifacts, dir)
	}
}

func TestPreparationReplacementRestartAndPartialFailure(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	live := f.launch(t, "demo", "two exports")
	f.deliver(t, live.Run)
	ctx := context.Background()
	first, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, live.Run.OutputDir)
	if err != nil || first.State != domain.ActionArtifactsPrepared {
		t.Fatalf("prepare = %+v, %v", first, err)
	}
	run, _ := f.service.GetActionRun(ctx, live.Run.ID)
	artifacts, dir := f.service.callerArtifacts(ctx, domain.Session{}, run, 0)
	if run.Package != nil || artifacts.State != domain.ActionArtifactsNone || dir != "" {
		t.Fatalf("prepared package published: %+v %+v", run, artifacts)
	}
	// Invalid replacement must retain the previous immutable snapshot.
	if _, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, t.TempDir()); err == nil {
		t.Fatal("empty replacement accepted")
	}
	pkg, _, _ := f.service.artifactRecords(ctx, run.ID, "")
	if pkg.Path != first.Path || readFile(t, filepath.Join(first.Path, "summary.md")) != "ok" {
		t.Fatal("previous snapshot lost")
	}
	// A copy-stage failure (valid source, unusable managed destination) also
	// preserves the previous package and records the failed candidate.
	badRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badRoot, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	f.service.actions.artifactsRoot = badRoot
	if _, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, live.Run.OutputDir); err == nil {
		t.Fatal("copy into a file succeeded")
	}
	f.service.actions.artifactsRoot = f.artRoot
	pkg, _, _ = f.service.artifactRecords(ctx, run.ID, "")
	if pkg.Path != first.Path || readFile(t, filepath.Join(pkg.Path, "summary.md")) != "ok" {
		t.Fatal("copy failure lost snapshot")
	}
	// Restart and startup cleanup retain a running execution's preparation.
	f.open(t)
	f.service.CleanupActionArtifacts(ctx)
	pkg, _, _ = f.service.artifactRecords(ctx, run.ID, "")
	if pkg.State != domain.ActionArtifactsPrepared || readFile(t, filepath.Join(pkg.Path, "summary.md")) != "ok" {
		t.Fatal("restart lost preparation")
	}
	if err := os.WriteFile(filepath.Join(live.Run.OutputDir, "summary.md"), []byte("partial capture"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, live.Run.OutputDir)
	if err != nil || second.Path == first.Path {
		t.Fatalf("replace = %+v %v", second, err)
	}
	f.service.CleanupActionArtifacts(ctx)
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatalf("retired snapshot not removed: %v", err)
	}
	finished, err := f.conclude(t, live.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionFailed, Summary: "One export succeeded; second failed"})
	if err != nil || finished.Package == nil || finished.Status != domain.ActionRunFailed {
		t.Fatalf("partial conclusion = %+v, %v", finished, err)
	}
	artifacts, dir = f.service.callerArtifacts(ctx, domain.Session{}, finished, 0)
	if artifacts.State != domain.ActionArtifactsReady || artifacts.ExpiresAt == nil || dir != second.Path {
		t.Fatalf("partial delivery = %+v %q", artifacts, dir)
	}
	if readFile(t, filepath.Join(dir, "summary.md")) != "partial capture" {
		t.Fatal("wrong package")
	}
}

func TestChangedPreparationInvalidatesPendingConclusion(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	live := f.launch(t, "demo", "go")
	f.deliver(t, live.Run)
	ctx := context.Background()
	if _, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, live.Run.OutputDir); err != nil {
		t.Fatal(err)
	}
	done := f.startConclude(live.Session.ID, domain.ConcludeActionRequest{Outcome: domain.ActionConclusionCompleted, Summary: "first"})
	id, returned := f.pendingConclusion(t, live.Session.ID, done)
	if returned != nil {
		t.Fatalf("early return: %+v", returned)
	}
	if _, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, live.Run.OutputDir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ApproveIntent(ctx, live.Session.ID, id, nil); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("approval = %v", err)
	}
	result := awaitConclude(t, done)
	if result.res.Outcome != domain.IntentOutcomeError {
		t.Fatalf("result %+v", result)
	}
	f.requireRunning(t, live)
}

func TestCancelledPreparedPackageIsNeverPublishedAndIsCleaned(t *testing.T) {
	f := newActionFixture(t)
	f.writeValidAction(t, "demo")
	live := f.launch(t, "demo", "go")
	f.deliver(t, live.Run)
	ctx := context.Background()
	pkg, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, live.Run.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	// Overlapping mutations fail promptly and leave the running execution alone.
	if err := f.service.actions.artifacts.beginMutation(live.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CancelActionRun(ctx, live.Run.ID); err == nil {
		t.Fatal("overlapping cancel accepted")
	}
	if _, err := f.service.DeliverActionArtifacts(ctx, live.Session.ID, live.Run.OutputDir); err == nil {
		t.Fatal("overlapping preparation accepted")
	}
	f.service.actions.artifacts.endMutation(live.Run.ID)
	run, err := f.service.CancelActionRun(ctx, live.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.service.CleanupActionArtifacts(ctx)
	if _, err := os.Stat(pkg.Path); !os.IsNotExist(err) {
		t.Fatalf("abandoned package remains: %v", err)
	}
	artifacts, dir := f.service.callerArtifacts(ctx, domain.Session{}, run, 0)
	if artifacts.State != domain.ActionArtifactsNone || dir != "" || run.Package != nil {
		t.Fatalf("cancel published artifacts: %+v", artifacts)
	}
	if _, err := os.Stat(live.Run.OutputDir); err != nil {
		t.Fatalf("original removed: %v", err)
	}
}
