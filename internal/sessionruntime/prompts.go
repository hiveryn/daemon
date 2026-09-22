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
//go:embed prompts/architect/*.md prompts/work/*.md
var promptFS embed.FS

const (
	architectSystemPromptName  = "prompts/architect/SYSTEM.md"
	architectKickoffPromptName = "prompts/architect/KICKOFF.md"
	workerSystemPromptName     = "prompts/work/SYSTEM.md"
	workerKickoffPromptName    = "prompts/work/KICKOFF.md"
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
		Repos:         renderRepos(architect.Repos),
	})
}

// renderRepos formats a repo map as a sorted "- key: path" list.
func renderRepos(repos map[string]string) string {
	if len(repos) == 0 {
		return ""
	}
	lines := make([]string, 0, len(repos))
	for _, key := range configKeys(repos) {
		lines = append(lines, "- "+key+": "+repos[key])
	}
	return strings.Join(lines, "\n")
}

// architectInstructions is the assembled architect system prompt plus what
// happened with the workspace's optional ARCHITECT_SYSTEM.md, so the caller can
// log a file that exists but was not applied.
type architectInstructions struct {
	Text   string
	Custom workspacefs.ArchitectSystemDocument
}

// buildArchitectInstructions assembles the built-in architect system prompt and
// the architect workspace's optional ARCHITECT_SYSTEM.md.
//
// A present-but-unloadable file is surfaced inside the instructions themselves:
// the architect is told exactly which file failed and why, and that no project
// preferences are in effect, rather than being told nothing (which would read as
// "loaded"). Startup is never blocked by it — the architect is the one who can
// repair the file, so it has to be able to start.
func buildArchitectInstructions(architect config.ArchitectConfig) (architectInstructions, error) {
	system, err := builtinPrompt(architectSystemPromptName)
	if err != nil {
		return architectInstructions{}, err
	}

	custom := workspacefs.ReadArchitectSystem(architect.Path)
	var b strings.Builder
	b.WriteString(system)

	switch {
	case custom.Loaded:
		b.WriteString("\n\n## Project collaboration preferences\n\n")
		b.WriteString("The following comes from this workspace's ")
		b.WriteString(workspacefs.ArchitectSystemFileName)
		b.WriteString(". It sets how to collaborate on this project; Hiveryn's own rules above still apply.\n\n")
		b.WriteString(strings.TrimSpace(custom.Content))
	case custom.Present:
		b.WriteString("\n\n## Project collaboration preferences\n\n")
		b.WriteString(workspacefs.ArchitectSystemFileName)
		b.WriteString(" exists at ")
		b.WriteString(custom.Path)
		b.WriteString(" but was NOT loaded, so no project-specific preferences are in effect for this session:\n")
		for _, diag := range custom.Diagnostics {
			b.WriteString("- ")
			b.WriteString(diag.Code)
			b.WriteString(": ")
			b.WriteString(diag.Message)
			b.WriteString("\n")
		}
		b.WriteString("Repair the file (checkWorkspace reports it too); it is read again at the next session start.")
	}

	return architectInstructions{Text: b.String(), Custom: custom}, nil
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
// field is a daemon-supplied value: ticket identity, writable repositories, the
// canonical project documents (RoadmapCurrentPath empty when the optional
// roadmap is absent), and the workflows explicitly selected for the session
// (or none).
type workerKickoffData struct {
	TicketID            string
	Repos               []workerRepo
	ProjectOverviewPath string
	ProjectStatePath    string
	RoadmapCurrentPath  string
	Workflows           []string
}

// renderWorkerKickoff renders the fixed worker startup message. It carries only
// the task and its context; role, scope rules and tool guidance live in the
// built-in worker instructions, not here.
//
// Every path is a canonical path into the architect workspace that the worker
// reads live; nothing is copied into the prompt.
func renderWorkerKickoff(ticketID string, repos []workerRepo, ctx workspacefs.WorkerContext) (string, error) {
	return renderBuiltinPrompt(workerKickoffPromptName, workerKickoffData{
		TicketID:            ticketID,
		Repos:               repos,
		ProjectOverviewPath: ctx.ProjectOverviewPath,
		ProjectStatePath:    ctx.ProjectStatePath,
		RoadmapCurrentPath:  ctx.RoadmapCurrentPath,
		Workflows:           ctx.Workflows,
	})
}

// resumeInstructions appends the resume notice to a session's stored
// instructions. Hiveryn does not track whether a selected workflow changed while
// the agent was down (there are no content digests), so it never claims the
// files are unchanged: it tells the agent to reread the canonical files before
// continuing. Sessions with no selected workflows resume with their stored
// instructions unchanged.
func resumeInstructions(instructions string, workflows []string) string {
	if len(workflows) == 0 {
		return instructions
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(instructions))
	b.WriteString("\n\n## Session resumed\n\n")
	b.WriteString("Hiveryn resumed this session. The workflows selected for it may have been edited while the session was down; Hiveryn does not track that. Before continuing, reread the selected workflows at their canonical paths and follow the current text:\n")
	for _, path := range workflows {
		b.WriteString(path + "\n")
	}
	return strings.TrimSpace(b.String())
}
