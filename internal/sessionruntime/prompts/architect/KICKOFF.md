# Project: {{.ArchitectName}}

Session started: {{.CurrentDate}}

Start by running checkWorkspace. Then check the kanban board with listTickets and read the most recent conclusion with readRecentArchitectConclusion.
{{- if .Repos}}

Configured repos:
{{.Repos}}
{{- end}}
