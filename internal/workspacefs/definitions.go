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
				Type:        domain.ArtifactFieldStringMap,
				Description: "Repo key → repository path. Keys are unique and are the keys tickets and workflows refer to. Paths may be absolute or ~-prefixed and are stored verbatim.",
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
			"The file holds name, repos and the optional availableActions. Any other key is rejected — there is no prompts block: architect and worker instructions are built into Hiveryn, and " + ArchitectSystemFileName + " carries per-project collaboration preferences.",
			"Repo keys are unique. Duplicate keys are rejected by the YAML decoder.",
			"Every repo path must resolve to an existing directory.",
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
			Fields:   []fieldSpec{lastUpdatedAtField},
		},
		Rules: append(currentDocumentRules,
			"Holds durable architecture, purpose and ownership. Dated operational facts belong in "+ProjectStateFileName+".",
		),
		Example: `---
lastUpdatedAt: "2026-01-15T09:30:00Z"
---

# Example Project

What the project is for, how it is put together, and who owns which part.
`,
	},

	domain.ArtifactProjectState: {
		Kind:        domain.ArtifactProjectState,
		Title:       "Project state",
		Required:    true,
		Format:      "markdown",
		Location:    ProjectStateFileName + " at the workspace root",
		Naming:      "Exactly " + ProjectStateFileName,
		RequireBody: true,
		Frontmatter: frontmatterSpec{
			Required: true,
			Fields:   []fieldSpec{lastUpdatedAtField},
		},
		Rules: append(currentDocumentRules,
			"Holds current facts, constraints and dated evidence. Distinguish proposed, implemented, deployed and verified behavior, and say where a claim came from.",
		),
		Example: `---
lastUpdatedAt: "2026-01-15T09:30:00Z"
---

# Current state

Evidence reviewed: 2026-01-15. What is deployed, what is in flight, and what
is known to be broken.
`,
	},

	domain.ArtifactRoadmapCurrent: {
		Kind:        domain.ArtifactRoadmapCurrent,
		Title:       "Current roadmap",
		Required:    false,
		Format:      "markdown",
		Location:    RoadmapCurrentFileName + " at the workspace root",
		Naming:      "Exactly " + RoadmapCurrentFileName,
		RequireBody: true,
		Frontmatter: frontmatterSpec{
			Required: true,
			Fields:   []fieldSpec{lastUpdatedAtField},
		},
		Rules: append([]string{
			"Optional. A workspace without it is valid, and a worker is launched without a roadmap to read.",
			"When present it must be readable UTF-8 markdown with a nonempty body and lastUpdatedAt as an RFC3339 UTC datetime. Other frontmatter keys are allowed.",
			"No headings, sections or body schema are required, and there is no line limit.",
			"lastUpdatedAt is an edit time. It records when the document was changed and is not a claim that its contents were re-verified.",
		},
			"Holds intended outcomes and priorities as prose. There is no item schema, status enum or completion inference — a done ticket is evidence, not automatic acceptance of an outcome.",
			"It is the only roadmap document. Edit it in place; earlier versions live in the workspace's Git history, not in archive files.",
		),
		Example: `---
lastUpdatedAt: "2026-01-15T09:30:00Z"
---

# Current roadmap

## Ship the ingest rewrite

The outcome we are aiming at, what would count as reaching it, and what is
still open.
`,
	},

	domain.ArtifactArchitectSystem: {
		Kind:        domain.ArtifactArchitectSystem,
		Title:       "Architect collaboration preferences",
		Required:    false,
		Format:      "markdown",
		Location:    ArchitectSystemFileName + " at the workspace root",
		Naming:      "Exactly " + ArchitectSystemFileName,
		RequireBody: true,
		Frontmatter: frontmatterSpec{},
		Rules: []string{
			"Optional. A workspace without it is valid.",
			"When present it must be readable UTF-8 markdown with a nonempty body — a file that exists but cannot be read is reported, never silently treated as loaded.",
			"No frontmatter is required, and none is rejected.",
			"Holds how to collaborate on this project. Hiveryn's own roles, lifecycle and maintenance rules are built in and do not belong here.",
		},
		Example: `Work as a peer: concise, candid, collaborative.
Investigate before recommending a change, and discuss material decisions.
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
		Frontmatter: frontmatterSpec{
			Required:  true,
			Exclusive: true,
			Fields: []fieldSpec{
				{
					Name:        "attach",
					Required:    true,
					Type:        domain.ArtifactFieldEnum,
					Enum:        []string{string(domain.WorkflowAttachManual), string(domain.WorkflowAttachSuggested)},
					Description: "manual — only ever added by hand. suggested — preselected when a session's writable repo scope overlaps repos.",
				},
				{
					Name:        "repos",
					Required:    false,
					Type:        domain.ArtifactFieldStringList,
					Description: "Required and nonempty when attach is suggested; not allowed when attach is manual. Unique repo keys, each configured in " + ConfigFileName + ".",
				},
			},
		},
		Rules: []string{
			"Readable UTF-8 markdown with a nonempty body.",
			"The frontmatter contains attach, plus repos when attach is suggested, and nothing else. In particular there is no dependencies field: workflows do not depend on one another.",
			"attach: manual takes no repos. attach: suggested takes a nonempty list of unique, configured repo keys.",
			"A repo match makes a workflow suggested, never mandatory: suggestions are preselected for the user and can be removed, and manual workflows can be added.",
			"An invalid workflow is reported with its diagnostics and excluded from suggestions rather than silently dropped.",
			"Workflows are flat and unordered. Two workflow bodies that contradict each other are for you and the user to resolve — file order establishes no precedence and nothing here resolves it for you.",
			"Selecting a workflow records the canonical path of the file in this workspace; nothing is stored. At each run launch the daemon rereads and validates the file and gives the worker its body below the frontmatter, verbatim, in the first message; a resume continues the conversation without re-sending it.",
		},
		Example: `---
attach: suggested
repos:
- example-api
---

# Reviewed delivery

The procedure to follow, written so it stands on its own.
`,
	},
}

// lastUpdatedAtField is shared by the current project documents so their
// timestamp rule is defined exactly once.
var lastUpdatedAtField = fieldSpec{
	Name:        "lastUpdatedAt",
	Required:    true,
	Type:        domain.ArtifactFieldTimestampUTC,
	Description: "When the document was last meaningfully edited, as an RFC3339 UTC datetime.",
}

// currentDocumentRules are the rules the two required project documents share;
// the optional roadmap states its own.
var currentDocumentRules = []string{
	"Required: readable UTF-8 markdown with a nonempty body.",
	"The frontmatter contains lastUpdatedAt as an RFC3339 UTC datetime. Other keys are allowed.",
	"No headings, sections or body schema are required, and there is no line limit.",
	"lastUpdatedAt is an edit time. It records when the document was changed and is not a claim that its contents were re-verified.",
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
