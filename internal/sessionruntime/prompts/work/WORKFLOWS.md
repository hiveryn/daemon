{{- if .Resumed}}
## Session resumed

Hiveryn resumed this session and read the workflows selected for it again from the architect workspace. Their current text follows and replaces any earlier copy in this conversation; follow it when continuing.
{{- else if .Workflows}}
Follow the workflows selected for this session. Their full text follows, one block per workflow, labelled with its canonical path.
{{- else}}
Workflows selected for this session: none
{{- end}}
{{- range .Workflows}}

<workflow path="{{.Path}}">
{{.Body}}
</workflow>
{{- end}}
