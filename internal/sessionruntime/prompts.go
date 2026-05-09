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
)

//go:embed prompts/architect/*.md
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

func loadArchitectPrompts(architectKey string, architect config.ArchitectConfig, cfg config.Config) (string, string, error) {
	systemContent, err := loadPromptFile(architect.Path, "SYSTEM.md")
	if err != nil {
		return "", "", err
	}

	kickoffTemplateSource, err := loadPromptFile(architect.Path, "KICKOFF.md")
	if err != nil {
		return "", "", err
	}

	renderedKickoff, err := renderKickoff(kickoffTemplateSource, kickoffTemplateData{
		ArchitectName:    architectKey,
		TicketList:       "(ticket system not yet implemented)",
		Sessions:         "(session history not yet implemented)",
		Repos:            renderRepos(architect.Repos),
		Variants:         strings.Join(configKeys(cfg.AgentProfiles), ", "),
		CurrentDate:      time.Now().UTC().Format(time.RFC3339),
		LastConclusionID: "",
	})
	if err != nil {
		return "", "", err
	}

	return systemContent, renderedKickoff, nil
}

func loadPromptFile(architectPath, filename string) (string, error) {
	overridePath := filepath.Join(architectPath, "prompts", "architect", filename)
	if data, err := os.ReadFile(overridePath); err == nil {
		return string(data), nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read prompt override %s: %w", overridePath, err)
	}

	data, err := promptFS.ReadFile(filepath.ToSlash(filepath.Join("prompts", "architect", filename)))
	if err != nil {
		return "", fmt.Errorf("read embedded prompt %s: %w", filename, err)
	}
	return string(data), nil
}

func renderKickoff(source string, data kickoffTemplateData) (string, error) {
	tmpl, err := template.New("kickoff").Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse kickoff template: %w", err)
	}

	var builder strings.Builder
	if err := tmpl.Execute(&builder, data); err != nil {
		return "", fmt.Errorf("render kickoff template: %w", err)
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
