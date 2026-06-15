# Project: {{.ArchitectName}}

Session started: {{.CurrentDate}}

Start by checking the kanban board with listTickets, then read the most recent conclusion with readRecentArchitectConclusion.
{{- if .Repos}}

Configured repos:
{{.Repos}}
{{- end}}
