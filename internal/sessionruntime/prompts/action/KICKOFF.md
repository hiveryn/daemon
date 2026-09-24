Execute the Hiveryn action "{{.Action}}" (execution {{.ExecutionID}}).

Action repository (working directory): {{.RepoPath}}
Output directory: {{.OutputDir}}

The output directory was created empty for this execution and lies outside the repository. Deliver every artifact there, at exactly that path.

Action definition: {{.DefinitionPath}}

Artifact contract:
{{.Artifacts}}

Launch instructions from the action's {{.KickoffName}}:

{{.Kickoff}}
