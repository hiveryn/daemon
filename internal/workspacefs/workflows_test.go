package workspacefs

import (
	"context"
	"strings"
	"testing"
)

func TestPlainWorkflowSelection(t *testing.T) {
	f := newFixture(t)
	f.write("workflows/plain.md", validManualWorkflow)
	list := f.workflows("api")
	if len(list.Workflows) != 1 || !list.Workflows[0].Valid {
		t.Fatalf("%+v", list)
	}
	selected, err := ResolveWorkflowNames(f.Workspace, "example", []string{"plain", "plain.md"})
	if err != nil || len(selected) != 1 {
		t.Fatalf("%+v %v", selected, err)
	}
	got, err := ValidateWorkerContext(f.Workspace, "example", []string{selected[0].Path})
	if err != nil || got.Workflows[0].Body != validManualWorkflow {
		t.Fatalf("%+v %v", got, err)
	}
	f.write("workflows/old.md", "---\nattach: manual\n---\n\nOld.")
	list, err = NewService(nil).ListWorkflows(context.Background(), "example", f.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list.Workflows {
		if w.Name == "old" && (w.Valid || !strings.Contains(w.Diagnostics[0].Message, "attach")) {
			t.Fatalf("%+v", w)
		}
	}
}
