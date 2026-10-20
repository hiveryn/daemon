Action: {{.Action}}
Execution: {{.ExecutionID}}
Machine: {{.Machine}}
Repository: {{.RepoPath}}
Definition: {{.DefinitionPath}}
{{if .Artifacts}}
Output directory: {{.OutputDir}}

Artifact contract ({{.ArtifactMode}}):
{{.Artifacts}}
{{end}}
Launch instructions from {{.KickoffName}}:

{{.Kickoff}}
