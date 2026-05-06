package domain

import (
	"context"
	"encoding/json"
	"time"
)

type ArchitectGroup struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ArchitectGroupInput struct {
	Name string `json:"name"`
}

type ArchitectGroupUpdate struct {
	Name    *string `json:"name,omitempty"`
	NameSet bool    `json:"-"`
}

func (u *ArchitectGroupUpdate) UnmarshalJSON(data []byte) error {
	type rawArchitectGroupUpdate struct {
		Name *string `json:"name"`
	}

	var raw rawArchitectGroupUpdate
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	u.Name = raw.Name
	_, u.NameSet = fields["name"]
	return nil
}

type Architect struct {
	ID           string             `json:"id"`
	Path         string             `json:"path"`
	Title        string             `json:"title"`
	Group        *ArchitectGroupRef `json:"group,omitempty"`
	Exists       bool               `json:"exists"`
	RepoCount    int                `json:"repo_count,omitempty"`
	Repos        []Repo             `json:"repos,omitempty"`
	LastOpenedAt *time.Time         `json:"last_opened_at"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

type ArchitectGroupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ArchitectInput struct {
	Path    string  `json:"path"`
	Title   string  `json:"title,omitempty"`
	GroupID *string `json:"group_id,omitempty"`
}

type ArchitectUpdate struct {
	Title      *string `json:"title,omitempty"`
	GroupID    *string `json:"group_id,omitempty"`
	Path       *string `json:"path,omitempty"`
	TitleSet   bool    `json:"-"`
	GroupIDSet bool    `json:"-"`
	PathSet    bool    `json:"-"`
}

func (u *ArchitectUpdate) UnmarshalJSON(data []byte) error {
	type rawArchitectUpdate struct {
		Title   *string `json:"title"`
		GroupID *string `json:"group_id"`
		Path    *string `json:"path"`
	}

	var raw rawArchitectUpdate
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	u.Title = raw.Title
	u.GroupID = raw.GroupID
	u.Path = raw.Path
	_, u.TitleSet = fields["title"]
	_, u.GroupIDSet = fields["group_id"]
	_, u.PathSet = fields["path"]
	return nil
}

type Repo struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Path        *string   `json:"path"`
	ArchitectID string    `json:"architect_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type RepoInput struct {
	Key  string  `json:"key"`
	Path *string `json:"path,omitempty"`
}

type RepoUpdate struct {
	Key     *string `json:"key,omitempty"`
	Path    *string `json:"path,omitempty"`
	KeySet  bool    `json:"-"`
	PathSet bool    `json:"-"`
}

func (u *RepoUpdate) UnmarshalJSON(data []byte) error {
	type rawRepoUpdate struct {
		Key  *string `json:"key"`
		Path *string `json:"path"`
	}

	var raw rawRepoUpdate
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	u.Key = raw.Key
	u.Path = raw.Path
	_, u.KeySet = fields["key"]
	_, u.PathSet = fields["path"]
	return nil
}

type ArchitectGroupRepository interface {
	List(ctx context.Context) ([]ArchitectGroup, error)
	Create(ctx context.Context, input ArchitectGroupInput) (ArchitectGroup, error)
	Get(ctx context.Context, id string) (ArchitectGroup, error)
	Update(ctx context.Context, id string, input ArchitectGroupUpdate) (ArchitectGroup, error)
	Delete(ctx context.Context, id string) error
}

type ArchitectRepository interface {
	List(ctx context.Context) ([]Architect, error)
	Create(ctx context.Context, input ArchitectInput) (Architect, error)
	Get(ctx context.Context, id string) (Architect, error)
	Update(ctx context.Context, id string, input ArchitectUpdate) (Architect, error)
	Delete(ctx context.Context, id string) error
	Touch(ctx context.Context, id string, openedAt time.Time) error
}

type RepoRepository interface {
	List(ctx context.Context, architectID string) ([]Repo, error)
	Create(ctx context.Context, architectID string, input RepoInput) (Repo, error)
	Get(ctx context.Context, architectID, id string) (Repo, error)
	Update(ctx context.Context, architectID, id string, input RepoUpdate) (Repo, error)
	Delete(ctx context.Context, architectID, id string) error
}
