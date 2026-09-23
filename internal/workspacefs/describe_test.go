package workspacefs

import (
	"strings"
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestDescribeCoversEveryKind(t *testing.T) {
	for _, kind := range domain.ArtifactKinds() {
		t.Run(string(kind), func(t *testing.T) {
			schema, err := Describe(kind)
			if err != nil {
				t.Fatalf("Describe(%s): %v", kind, err)
			}
			if schema.Kind != kind {
				t.Errorf("kind = %s, want %s", schema.Kind, kind)
			}
			if schema.SchemaVersion != ArtifactSchemaVersion {
				t.Errorf("schema_version = %d", schema.SchemaVersion)
			}
			for name, value := range map[string]string{
				"title": schema.Title, "format": schema.Format,
				"location": schema.Location, "naming": schema.Naming, "example": schema.Example,
			} {
				if strings.TrimSpace(value) == "" {
					t.Errorf("%s is empty", name)
				}
			}
			if len(schema.Rules) == 0 {
				t.Error("rules is empty")
			}
		})
	}
}

// Tickets and conclusions keep their own schemas and are never described here.
func TestDescribeRejectsUnknownKinds(t *testing.T) {
	for _, kind := range []string{"TICKET", "CONCLUSION", "", "project_overview", "AGENTS"} {
		if _, err := Describe(domain.ArtifactKind(kind)); err == nil {
			t.Errorf("Describe(%q) should have failed", kind)
		}
		if domain.ArtifactKind(kind).Valid() {
			t.Errorf("%q must not be a valid artifact kind", kind)
		}
	}
}

// describeArtifact and the workspace check must be driven by one definition.
// This test pins that: the described fields are exactly the fields validated.
func TestDescribeFieldsMatchValidatedFields(t *testing.T) {
	for kind, def := range definitions {
		schema, err := Describe(kind)
		if err != nil {
			t.Fatalf("Describe(%s): %v", kind, err)
		}

		want := def.Frontmatter.Fields
		if kind == domain.ArtifactHiverynYAML {
			want = def.Fields
		}
		if len(schema.Fields) != len(want) {
			t.Errorf("%s: described %d fields, definition has %d", kind, len(schema.Fields), len(want))
			continue
		}
		for i, field := range want {
			got := schema.Fields[i]
			if got.Name != field.Name || got.Required != field.Required || got.Type != field.Type {
				t.Errorf("%s field %d = %+v, definition says %+v", kind, i, got, field)
			}
			if strings.TrimSpace(got.Description) == "" {
				t.Errorf("%s field %q has no description", kind, got.Name)
			}
		}
	}
}

// The described timestamp rule must be the one the validator enforces.
func TestDescribeTimestampFieldsAreEnforced(t *testing.T) {
	overview, err := Describe(domain.ArtifactProjectOverview)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := overview.Fields[0]; got.Name != "lastUpdatedAt" || got.Type != domain.ArtifactFieldTimestampUTC || !got.Required {
		t.Fatalf("overview field = %+v", got)
	}
}

func TestDescribeWorkflowAttachEnum(t *testing.T) {
	schema, err := Describe(domain.ArtifactWorkflow)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	attach := schema.Fields[0]
	if attach.Name != "attach" || attach.Type != domain.ArtifactFieldEnum {
		t.Fatalf("attach field = %+v", attach)
	}
	if strings.Join(attach.Enum, ",") != "manual,suggested" {
		t.Errorf("enum = %v", attach.Enum)
	}
	if len(schema.Fields) != 2 || schema.Fields[1].Name != "repos" {
		t.Errorf("fields = %+v; a workflow declares attach and repos, and nothing else", schema.Fields)
	}
}

// Every example must survive the validator that the same definition drives —
// otherwise describeArtifact would hand the agent a template that fails the
// check it is meant to satisfy.
func TestDescribeExamplesPassTheirOwnValidator(t *testing.T) {
	for kind, def := range definitions {
		if def.Format != "markdown" {
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			schema, err := Describe(kind)
			if err != nil {
				t.Fatalf("Describe: %v", err)
			}

			f := newFixture(t)
			// The workflow example names the repo key its own hiveryn.yaml
			// example configures, so the fixture configures it too.
			if kind == domain.ArtifactWorkflow {
				repos := map[string]string{"example-api": f.Repos["api"]}
				f.writeConfig("Example Project", repos)
			}
			rel := exampleFixturePath(t, kind)
			f.write(rel, schema.Example)

			var diags []domain.WorkspaceDiagnostic
			report := f.check()
			switch kind {
			case domain.ArtifactWorkflow:
				diags = child(t, node(t, report, WorkflowsDirName), rel).Diagnostics
			default:
				diags = node(t, report, rel).Diagnostics
			}

			for _, diag := range diags {
				if diag.Severity == domain.DiagnosticError {
					t.Errorf("the documented example fails validation: %s %s", diag.Code, diag.Message)
				}
			}
		})
	}
}

// exampleFixturePath maps an artifact kind onto the workspace path its example
// belongs at.
func exampleFixturePath(t *testing.T, kind domain.ArtifactKind) string {
	t.Helper()
	switch kind {
	case domain.ArtifactProjectOverview:
		return ProjectOverviewFileName
	case domain.ArtifactProjectState:
		return ProjectStateFileName
	case domain.ArtifactRoadmapCurrent:
		return RoadmapCurrentFileName
	case domain.ArtifactArchitectSystem:
		return ArchitectSystemFileName
	case domain.ArtifactWorkflow:
		return WorkflowsDirName + "/EXAMPLE.md"
	default:
		t.Fatalf("no fixture path for %s", kind)
		return ""
	}
}
