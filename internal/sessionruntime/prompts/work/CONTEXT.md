The project documents follow in full, one block per document, labelled with its name. They are read-only context, current as of this launch.
{{- range .Documents}}

<document name="{{.Name}}">
{{.Content}}
</document>
{{- end}}

{{if .Workflows -}}
Follow the workflows selected for this session. Their full text follows, one block per workflow, labelled with its name.
{{- else -}}
Workflows selected for this session: none
{{- end}}
{{- range .Workflows}}

<workflow name="{{.Name}}">
{{.Body}}
</workflow>
{{- end}}
