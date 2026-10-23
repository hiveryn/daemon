package workspacefs

import (
	"fmt"

	"github.com/hiveryn/daemon/internal/domain"
)

// ArtifactSchemaVersion is the version of the artifact definitions below. It
// changes when a rule changes, so an agent can tell a described schema apart
// from one it learned earlier.
const ArtifactSchemaVersion = 1

// definition is the single source of truth for one artifact kind.
//
// This is the whole point of the type: describeArtifact renders a definition,
// and the workspace check executes the same definition. There is no second
// place where a rule is written down, so a rule the agent is told about is a
// rule that is actually enforced, and neither can drift from the other.
type definition struct {
	Kind     domain.ArtifactKind
	Title    string
	Required bool
	Format   string
	Location string
	Naming   string
	// Fields describes top-level keys for a YAML artifact, which has no
	// frontmatter. Markdown artifacts leave it empty and use Frontmatter.
	Fields      []fieldSpec
	Frontmatter frontmatterSpec
	// RequireBody rejects a document whose markdown body is blank once the
	// frontmatter is stripped.
	RequireBody bool
	Rules       []string
	Example     string
}

// frontmatterSpec is the executable description of an artifact's YAML
// frontmatter.
type frontmatterSpec struct {
	// Required demands a frontmatter block be present at all.
	Required bool
	// Exclusive rejects any key not listed in Fields. It encodes the "contains
	// only" rule for a workflow — and is what
	// makes "no workflow dependencies" a structural check rather than prose.
	Exclusive bool
	Fields    []fieldSpec
}

type fieldSpec struct {
	Name        string
	Required    bool
	Type        domain.ArtifactFieldType
	Enum        []string
	Description string
}

// definitions is the artifact catalog, keyed by kind.
//
// Deliberately absent: any required heading, section order, body schema or line
// limit. VALIDATORS_RULES allows structural checks only, so a document's prose
// is its author's business. Timestamps record when a document was edited and
// prove nothing about whether its contents are still true.
var definitions = map[domain.ArtifactKind]definition{
	domain.ArtifactHiverynYAML: {
		Kind:     domain.ArtifactHiverynYAML,
		Title:    "Project configuration",
		Required: true,
		Format:   "yaml",
		Location: ConfigFileName + " at the workspace root",
		Naming:   "Exactly " + ConfigFileName + "; there is one per workspace",
		Fields: []fieldSpec{
			{
				Name:        "name",
				Required:    true,
				Type:        domain.ArtifactFieldString,
				Description: "Project display name.",
			},
			{
				Name:        "repos",
				Required:    true,
				Type:        domain.ArtifactFieldMapping,
				Description: "Repo key → local path string or {path, machine} object. Machine is an SSH machine key; omitted means local. Remote paths must be absolute POSIX paths and are never expanded or inspected locally.",
			},
			{
				Name:        "availableActions",
				Required:    false,
				Type:        domain.ArtifactFieldStringList,
				Description: "Names of global Actions (directories under HIVERYN_HOME/actions) this architect may discover with getAvailableActions and request with executeAction. Omitted or empty exposes no Actions.",
			},
		},
		Rules: []string{
			"Required: a workspace without a readable " + ConfigFileName + " has no repo map.",
			"Repo keys are unique. Duplicate keys are rejected by the YAML decoder.",
			"Local repo paths must resolve to existing directories; remote directories are checked over SSH at launch. All writable repos in a ticket must use one machine.",
			"A repo key referenced by a workflow or a ticket must exist here.",
			"availableActions entries are unique, well-formed action names (lowercase letters, digits, '.', '_' or '-'). Whether each Action exists in the library is not a config error: getAvailableActions reports a missing or invalid definition, and it cannot be requested until repaired. The list limits only what this architect may request; the user can still launch any Action manually. There is no default variant — the user picks one when approving each request.",
			"Edit it with your filesystem tools; an invalid edit is reported by the workspace check, blocks worker launch until repaired, and never replaces the last valid runtime config.",
		},
		Example: `name: Example Project
repos:
  example-api: /Users/you/repos/example-api
  example-web: /Users/you/repos/example-web
availableActions:
  - demo-evidence
`,
	},

	domain.ArtifactProjectOverview: {
		Kind:        domain.ArtifactProjectOverview,
		Title:       "Project overview",
		Required:    true,
		Format:      "markdown",
		Location:    ProjectOverviewFileName + " at the workspace root",
		Naming:      "Exactly " + ProjectOverviewFileName,
		RequireBody: true,
		Frontmatter: frontmatterSpec{
			Required: true,
			Fields:   []fieldSpec{updated_atField},
		},
		Rules: append(currentDocumentRules,
			"Holds durable architecture, purpose, ownership and enduring constraints.",
		),
		Example: `---
updated_at: "2026-01-15T09:30:00Z"
---

# Example Project

What the project is for, how it is put together, and who owns which part.
`,
	},

	domain.ArtifactWorkflow: {
		Kind:        domain.ArtifactWorkflow,
		Title:       "Workflow",
		Required:    false,
		Format:      "markdown",
		Location:    WorkflowsDirName + "/ — a required directory that may be empty. Workflow files are its immediate *.md children; there are no category subdirectories.",
		Naming:      "Any *.md name directly inside " + WorkflowsDirName + "/. The name without its extension identifies the workflow.",
		RequireBody: true,
		Frontmatter: frontmatterSpec{Exclusive: true},
		Rules:       []string{"Plain UTF-8 Markdown with a nonempty body. Selected explicitly for each spawn; no applicability metadata or automatic selection."},
		Example:     "# Reviewed delivery\n\nThe selected procedure.\n",
	},
}

// updated_atField is shared by the current project documents so their
// timestamp rule is defined exactly once.
var updated_atField = fieldSpec{
	Name:        "updated_at",
	Required:    true,
	Type:        domain.ArtifactFieldTimestampUTC,
	Description: "When the document was last meaningfully edited, as an RFC3339 UTC datetime.",
}

// currentDocumentRules are the rules the two required project documents share;
// the optional roadmap states its own.
var currentDocumentRules = []string{
	"Required: readable UTF-8 markdown with a nonempty body.",
	"The frontmatter contains updated_at as an RFC3339 UTC datetime. Other keys are allowed.",
	"No headings, sections or body schema are required, and there is no line limit.",
	"updated_at is an edit time. It records when the document was changed and is not a claim that its contents were re-verified.",
}

// Describe renders the definition for kind as the artifact schema returned by
// describeArtifact. The rendering is a projection of the definition the
// workspace check executes, never a separately maintained description.
func Describe(kind domain.ArtifactKind) (domain.ArtifactSchema, error) {
	def, ok := definitions[kind]
	if !ok {
		return domain.ArtifactSchema{}, fmt.Errorf("unknown artifact kind %q", kind)
	}

	fields := def.Frontmatter.Fields
	if def.Kind == domain.ArtifactHiverynYAML {
		// hiveryn.yaml is YAML all the way down; its fields are top-level
		// config keys rather than markdown frontmatter.
		fields = def.Fields
	}

	schema := domain.ArtifactSchema{
		Kind:          def.Kind,
		SchemaVersion: ArtifactSchemaVersion,
		Title:         def.Title,
		Required:      def.Required,
		Format:        def.Format,
		Location:      def.Location,
		Naming:        def.Naming,
		Fields:        make([]domain.ArtifactField, 0, len(fields)),
		Rules:         append([]string(nil), def.Rules...),
		Example:       def.Example,
	}
	for _, field := range fields {
		schema.Fields = append(schema.Fields, domain.ArtifactField{
			Name:        field.Name,
			Required:    field.Required,
			Type:        field.Type,
			Enum:        append([]string(nil), field.Enum...),
			Description: field.Description,
		})
	}
	return schema, nil
}
