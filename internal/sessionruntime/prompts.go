package sessionruntime

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
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
	systemContent, err := loadPromptFile(architect.Path, "architect", "SYSTEM.md")
	if err != nil {
		return "", "", err
	}

	kickoffTemplateSource, err := loadPromptFile(architect.Path, "architect", "KICKOFF.md")
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
	kickoffTemplateSource, err := loadPromptFile(architect.Path, "work", "KICKOFF.md")
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

func loadPromptFile(architectPath, promptSubDir, filename string) (string, error) {
	overridePath := filepath.Join(architectPath, "prompts", promptSubDir, filename)
	if data, err := os.ReadFile(overridePath); err == nil {
		return string(data), nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read prompt override %s: %w", overridePath, err)
	}

	data, err := promptFS.ReadFile(filepath.ToSlash(filepath.Join("prompts", promptSubDir, filename)))
	if err != nil {
		return "", fmt.Errorf("read embedded prompt %s/%s: %w", promptSubDir, filename, err)
	}
	return string(data), nil
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
