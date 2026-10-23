package sessionruntime

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/workspacefs"
)

// The built-in role instructions and kickoffs. They are Hiveryn's, not a
// project's: there is no per-architect override, and nothing in hiveryn.yaml
// points at a prompt file. They live as markdown so they can be read and
// reviewed as text; the kickoffs are Go templates whose placeholders are
// daemon-supplied values, not project-customizable fields. The one
// project-level customization is the optional ARCHITECT_SYSTEM.md in the
// architect workspace, which is appended to the architect instructions.
//
//go:embed prompts/architect/*.md prompts/work/*.md prompts/action/*.md
var promptFS embed.FS

const (
	architectSystemPromptName  = "prompts/architect/SYSTEM.md"
	architectKickoffPromptName = "prompts/architect/KICKOFF.md"
	workerSystemPromptName     = "prompts/work/SYSTEM.md"
	workerKickoffPromptName    = "prompts/work/KICKOFF.md"
	workerContextPromptName    = "prompts/work/CONTEXT.md"
	actionSystemPromptName     = "prompts/action/SYSTEM.md"
	actionKickoffPromptName    = "prompts/action/KICKOFF.md"
)

// builtinPrompt reads one embedded prompt. A missing embed is a build defect,
// so it is returned as an error rather than tolerated.
func builtinPrompt(name string) (string, error) {
	data, err := promptFS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read embedded prompt %s: %w", name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// renderBuiltinPrompt renders an embedded Go-template prompt with data.
func renderBuiltinPrompt(name string, data any) (string, error) {
	source, err := builtinPrompt(name)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse embedded prompt %s: %w", name, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("render embedded prompt %s: %w", name, err)
	}
	return strings.TrimSpace(b.String()), nil
}

// architectKickoffData is the template data for prompts/architect/KICKOFF.md:
// project identity, session start time and the configured repo map, all
// resolved daemon-side.
type architectKickoffData struct {
	ArchitectName string
	CurrentDate   string
	Repos         string
}

// renderArchitectKickoff renders the fixed architect startup message. It opens
// with checkWorkspace, then the board and the latest conclusion; the built-in
// system prompt carries the rest of the reading order.
func renderArchitectKickoff(architectKey string, architect config.ArchitectConfig, now time.Time) (string, error) {
	return renderBuiltinPrompt(architectKickoffPromptName, architectKickoffData{
		ArchitectName: architectKey,
		CurrentDate:   now.UTC().Format(time.RFC3339),
		Repos:         renderRepos(architect.Repos, architect.RepoMachines),
	})
}

// renderRepos formats a repo map as a sorted "- key: path" list.
func renderRepos(repos map[string]string, machines ...map[string]string) string {
	if len(repos) == 0 {
		return ""
	}
	lines := make([]string, 0, len(repos))
	for _, key := range configKeys(repos) {
		label := key
		if len(machines) > 0 && machines[0][key] != "" {
			label += " (SSH machine " + machines[0][key] + ")"
		}
		lines = append(lines, "- "+label+": "+repos[key])
	}
	return strings.Join(lines, "\n")
}

// architectInstructions contains the approved built-in instructions only.
type architectInstructions struct{ Text string }

func buildArchitectInstructions(_ config.ArchitectConfig) (architectInstructions, error) {
	system, err := builtinPrompt(architectSystemPromptName)
	return architectInstructions{Text: system}, err
}

// workerInstructions returns the built-in WORKER_SYSTEM text. It is additive to
// the provider's own instructions and explains only the two Hiveryn tools a
// worker acts through, createWorkTicket and concludeTicketSession.
func workerInstructions() (string, error) {
	return builtinPrompt(workerSystemPromptName)
}

// workerRepo is one writable repository named in the worker kickoff.
type workerRepo struct {
	Key  string
	Path string
}

// workerKickoffData is the template data for prompts/work/KICKOFF.md. Every
// field is a daemon-supplied value: ticket identity and writable repositories.
type workerKickoffData struct {
	TicketID string
	Repos    []workerRepo
}

// renderWorkerKickoff renders the fixed worker startup message that a ticket
// session stores as its prompt. It carries only the task and its scope; role,
// scope rules and tool guidance live in the built-in worker instructions, not
// here.
//
// The project documents and selected workflows are not part of the stored
// kickoff: their content is read at each launch and attached by
// workerLaunchPrompt.
func renderWorkerKickoff(ticketID string, repos []workerRepo) (string, error) {
	return renderBuiltinPrompt(workerKickoffPromptName, workerKickoffData{
		TicketID: ticketID,
		Repos:    repos,
	})
}

// renderWorkerContext renders the project documents and the selected
// workflows in full, each in a block labelled with its name — never with an
// architect-workspace path, which a worker may be unable to reach. Content is
// given verbatim apart from leading and trailing blank lines; nothing is
// summarized or rewritten.
func renderWorkerContext(ctx workspacefs.WorkerContext) (string, error) {
	documents := make([]workspacefs.ProjectDocument, 0, len(ctx.Documents))
	for _, document := range ctx.Documents {
		documents = append(documents, workspacefs.ProjectDocument{
			Name:    document.Name,
			Content: strings.Trim(document.Content, "\r\n"),
		})
	}
	workflows := make([]workspacefs.SelectedWorkflow, 0, len(ctx.Workflows))
	for _, workflow := range ctx.Workflows {
		workflows = append(workflows, workspacefs.SelectedWorkflow{
			Name: workflow.Name,
			Body: strings.Trim(workflow.Body, "\r\n"),
		})
	}
	return renderBuiltinPrompt(workerContextPromptName, workspacefs.WorkerContext{
		Documents: documents,
		Workflows: workflows,
	})
}

// workerLaunchPrompt is the first message of a ticket run: the stored kickoff
// followed by the project documents and the selected workflows (or "none") as
// just read and validated by the launch check. Delivering the text rather than
// workspace paths means a worker needs no access to the architect workspace,
// and reading at launch means a run never starts from a copy older than its
// launch. The content goes only in this first message, never in the
// instructions (system prompt), and a resumed agent continues its conversation
// as it is: nothing is re-sent.
func workerLaunchPrompt(kickoff string, ctx workspacefs.WorkerContext) (string, error) {
	section, err := renderWorkerContext(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(kickoff) + "\n\n" + section, nil
}
