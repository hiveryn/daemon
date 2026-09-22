Read your ticket with readTicket(id: "{{.TicketID}}") and complete the work.

Writable repositories:
{{- range .Repos}}
- {{.Key}}: {{.Path}}
{{- end}}

Read these project documents:
{{.ProjectOverviewPath}}
{{.ProjectStatePath}}
{{- if .RoadmapCurrentPath}}
{{.RoadmapCurrentPath}}
{{- end}}

Read and follow the workflows selected for this session:
{{- if .Workflows}}
{{- range .Workflows}}
{{.}}
{{- end}}
{{- else}}
none
{{- end}}
