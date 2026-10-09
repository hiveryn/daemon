Read your ticket with readTicket(id: "{{.TicketID}}") and complete the work.

Writable repositories:
{{- range .Repos}}
- {{.Key}}: {{.Path}}
{{- end}}
