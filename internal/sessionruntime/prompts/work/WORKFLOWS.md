{{- if .Workflows}}
Follow the workflows selected for this session. Their full text follows, one block per workflow, labelled with its canonical path.
{{- else}}
Workflows selected for this session: none
{{- end}}
{{- range .Workflows}}

<workflow path="{{.Path}}">
{{.Body}}
</workflow>
{{- end}}
