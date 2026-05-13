# Project: {{.ArchitectName}}

Session started: {{.CurrentDate}}

Start by checking the kanban board with listTickets, then read the most recent conclusion with readRecentConclusion.
{{- if .Repos}}

Configured repos:
{{.Repos}}
{{- end}}
