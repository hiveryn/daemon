package sessionruntime

import (
	"embed"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/hiveryn/daemon/internal/config"
	"github.com/hiveryn/daemon/internal/domain"
)

//go:embed prompts/architect/*.md prompts/work/*.md
var promptFS embed.FS

type kickoffTemplateData struct {
	ArchitectName    string
	TicketList       string
	Sessions         string
	Repos            string
	Variants         string
	CurrentDate      string
	LastConclusionID string
}

type workerKickoffTemplateData struct {
	TicketTitle   string
	TicketBody    string
	TicketID      string
	Repo          string
	RepoPath      string
	References    string
	Created       string
	Updated       string
	ArchitectName string
	ProjectPath   string
	Repos         string
}

func loadArchitectPrompts(architectKey string, architect config.ArchitectConfig, cfg config.Config) (string, string, error) {
	systemContent, err := loadPrompt(architect.SystemPromptPath, "prompts/architect/SYSTEM.md")
	if err != nil {
		return "", "", err
	}

	kickoffTemplateSource, err := loadPrompt(architect.KickoffPromptPath, "prompts/architect/KICKOFF.md")
	if err != nil {
		return "", "", err
	}

	renderedKickoff, err := renderKickoff(kickoffTemplateSource, kickoffTemplateData{
		ArchitectName:    architectKey,
		TicketList:       "(ticket system not yet implemented)",
		Sessions:         "(session history not yet implemented)",
		Repos:            renderRepos(architect.Repos),
		Variants:         strings.Join(configKeys(cfg.Variants), ", "),
		CurrentDate:      time.Now().UTC().Format(time.RFC3339),
		LastConclusionID: "",
	})
	if err != nil {
		return "", "", err
	}

	return systemContent, renderedKickoff, nil
}

func loadWorkerPrompt(architectKey string, architect config.ArchitectConfig, cfg config.Config, ticket domain.Ticket) (string, error) {
	kickoffTemplateSource, err := loadPrompt(selectTicketKickoff(architect, ticket.Repo), "prompts/work/KICKOFF.md")
	if err != nil {
		return "", err
	}

	var created, updated string
	if ticket.Created != nil {
		created = ticket.Created.UTC().Format(time.RFC3339)
	}
	if ticket.Updated != nil {
		updated = ticket.Updated.UTC().Format(time.RFC3339)
	}

	var references string
	if len(ticket.References) > 0 {
		lines := make([]string, 0, len(ticket.References))
		for _, ref := range ticket.References {
			lines = append(lines, "- "+ref)
		}
		references = strings.Join(lines, "\n")
	}

	rendered, err := renderWorkerKickoff(kickoffTemplateSource, workerKickoffTemplateData{
		TicketTitle:   ticket.Title,
		TicketBody:    ticket.Body,
		TicketID:      ticket.ID,
		Repo:          ticket.Repo,
		RepoPath:      architect.Repos[ticket.Repo],
		References:    references,
		Created:       created,
		Updated:       updated,
		ArchitectName: architectKey,
		ProjectPath:   architect.Path,
		Repos:         renderRepos(architect.Repos),
	})
	if err != nil {
		return "", err
	}

	return rendered, nil
}

// loadPrompt reads the prompt at overridePath when set; otherwise it reads the
// embedded default identified by embeddedName (a forward-slash path within
// promptFS). A configured override that cannot be read is a hard error.
func loadPrompt(overridePath, embeddedName string) (string, error) {
	if strings.TrimSpace(overridePath) != "" {
		data, err := os.ReadFile(overridePath)
		if err != nil {
			return "", fmt.Errorf("read prompt %s: %w", overridePath, err)
		}
		return string(data), nil
	}

	data, err := promptFS.ReadFile(embeddedName)
	if err != nil {
		return "", fmt.Errorf("read embedded prompt %s: %w", embeddedName, err)
	}
	return string(data), nil
}

// selectTicketKickoff returns the configured ticket-kickoff path for the given
// repo. A repo-scoped entry wins over the default (no-repos) entry. Returns ""
// when no entry applies, meaning the embedded default should be used.
func selectTicketKickoff(architect config.ArchitectConfig, repo string) string {
	defaultPath := ""
	for _, kickoff := range architect.TicketKickoffs {
		if len(kickoff.Repos) == 0 {
			defaultPath = kickoff.Path
			continue
		}
		for _, repoKey := range kickoff.Repos {
			if repoKey == repo {
				return kickoff.Path
			}
		}
	}
	return defaultPath
}

func renderKickoff(source string, data kickoffTemplateData) (string, error) {
	return renderTemplate("kickoff", source, data)
}

func renderWorkerKickoff(source string, data workerKickoffTemplateData) (string, error) {
	return renderTemplate("worker_kickoff", source, data)
}

func renderTemplate(name, source string, data any) (string, error) {
	tmpl, err := template.New(name).Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse %s template: %w", name, err)
	}

	var builder strings.Builder
	if err := tmpl.Execute(&builder, data); err != nil {
		return "", fmt.Errorf("render %s template: %w", name, err)
	}
	return strings.TrimSpace(builder.String()), nil
}

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
