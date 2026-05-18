package domain

type CommitRef struct {
	SHA  string `json:"sha" yaml:"sha" jsonschema:"Commit SHA"`
	Repo string `json:"repo" yaml:"repo" jsonschema:"Stable repo key/name, for example daemon or desktop"`
}
