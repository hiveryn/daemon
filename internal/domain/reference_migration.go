package domain

import (
	"context"
	"time"
)

// Reference migration report: the reviewable plan and apply result of
// converting legacy ticket references to the strict grammar. Produced by the
// daemon (architectfs) for `hiverynd references-migrate`; daemon-local, since
// no other repo consumes it.

type ReferenceMigrationReport struct {
	Mode        string                        `json:"mode"` // plan | apply
	GeneratedAt time.Time                     `json:"generated_at"`
	Architects  []ArchitectReferenceMigration `json:"architects"`
	Rules       []ReferenceMigrationRule      `json:"rules,omitempty"`
	Totals      map[string]int                `json:"totals"`
	Guidance    string                        `json:"guidance,omitempty"`
}

type ArchitectReferenceMigration struct {
	Key       string                     `json:"key"`
	Path      string                     `json:"path"`
	Error     string                     `json:"error,omitempty"`
	Tickets   []TicketReferenceMigration `json:"tickets"`
	Documents []DocumentReferenceFinding `json:"documents"`
	// Scanned counts every ticket inspected, including those with only valid
	// references (which are not listed).
	Scanned int `json:"scanned"`
}

type TicketReferenceMigration struct {
	ID         string             `json:"id"`
	Status     string             `json:"status"`
	File       string             `json:"file"`
	SHA256     string             `json:"sha256"`
	References []ReferenceMapping `json:"references"`
	Error      string             `json:"error,omitempty"`
	// Result and Message are set by an apply.
	Result  string `json:"result,omitempty"`
	Message string `json:"message,omitempty"`
}

// ReferenceMapping is one reference's decision. A reviewer resolves an
// ambiguous, unmapped, cross-architect or invalid entry by setting To to a
// valid reference or Drop to true; an entry left undecided stays as it is
// (and keeps its INVALID_REFERENCE diagnostic).
type ReferenceMapping struct {
	From        string   `json:"from"`
	Line        int      `json:"line,omitempty"`
	Disposition string   `json:"disposition"`
	To          string   `json:"to,omitempty"`
	Drop        bool     `json:"drop,omitempty"`
	Candidates  []string `json:"candidates,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

// DocumentReferenceFinding is an absolute path or a relative Markdown link in
// architect-owned Markdown. Documents point to context only with references
// (architect:<path>, <repoKey>:<path>); Suggestion is the reference to write
// instead, when ownership is clear.
type DocumentReferenceFinding struct {
	File        string `json:"file"`
	Line        int    `json:"line"`
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	Suggestion  string `json:"suggestion,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// ReferenceMigrationRule is an operator's explicit mapping of legacy absolute
// paths under FromPrefix (a directory) to ToPrefix (`<owner>:` or
// `<owner>:<path>`), for one architect or, when Architect is empty, every
// architect where ToPrefix's owner exists. Rules take precedence over
// inference; the longest matching FromPrefix wins.
type ReferenceMigrationRule struct {
	Architect  string `json:"architect,omitempty"`
	FromPrefix string `json:"from_prefix"`
	ToPrefix   string `json:"to_prefix"`
}

// ReferenceMigrator plans and applies the reference migration inside the
// daemon, so runtime-owned records change through workspace protection.
type ReferenceMigrator interface {
	PlanReferenceMigration(ctx context.Context, architects []string, rules []ReferenceMigrationRule) (ReferenceMigrationReport, error)
	ApplyReferenceMigration(ctx context.Context, plan ReferenceMigrationReport) (ReferenceMigrationReport, error)
}
