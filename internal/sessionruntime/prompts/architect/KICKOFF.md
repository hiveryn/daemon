# Project: {{.ArchitectName}}

**Session started**: {{.CurrentDate}}

# Tickets

{{.TicketList}}
{{- if .Sessions}}

# Recent Conclusions

{{.Sessions}}
{{- end}}
{{- if .Repos}}

# Configured Repos

{{.Repos}}
{{- end}}
{{- if .Variants}}

# Available Agent Variants

{{.Variants}}
{{- end}}
{{- if .LastConclusionID}}

Start by reading the last architect session conclusion: readConclusion(id: "{{.LastConclusionID}}")
{{- end}}
