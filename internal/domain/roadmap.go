package domain

import (
	"context"

	sd "github.com/hiveryn/shared/domain"
)

// Re-export pure roadmap data types from shared. RoadmapService and the
// read-query shape remain daemon-local.
type (
	RoadmapItemKind            = sd.RoadmapItemKind
	RoadmapItemStatus          = sd.RoadmapItemStatus
	RoadmapItem                = sd.RoadmapItem
	Roadmap                    = sd.Roadmap
	RoadmapArchiveEntry        = sd.RoadmapArchiveEntry
	RoadmapArchive             = sd.RoadmapArchive
	RoadmapArchiveEntrySummary = sd.RoadmapArchiveEntrySummary
	RoadmapTicketInfo          = sd.RoadmapTicketInfo
	RoadmapOpType              = sd.RoadmapOpType
	RoadmapOp                  = sd.RoadmapOp
	UpdateRoadmapParams        = sd.UpdateRoadmapParams
	RoadmapView                = sd.RoadmapView
	RoadmapOpResult            = sd.RoadmapOpResult
	RoadmapUpdateResult        = sd.RoadmapUpdateResult
)

const RoadmapSchemaVersion = sd.RoadmapSchemaVersion

const (
	RoadmapItemGoal       = sd.RoadmapItemGoal
	RoadmapItemInitiative = sd.RoadmapItemInitiative
	RoadmapItemMilestone  = sd.RoadmapItemMilestone
)

const (
	RoadmapStatusPlanned = sd.RoadmapStatusPlanned
	RoadmapStatusActive  = sd.RoadmapStatusActive
	RoadmapStatusBlocked = sd.RoadmapStatusBlocked
	RoadmapStatusDone    = sd.RoadmapStatusDone
)

const (
	RoadmapOpCreate       = sd.RoadmapOpCreate
	RoadmapOpUpdate       = sd.RoadmapOpUpdate
	RoadmapOpMove         = sd.RoadmapOpMove
	RoadmapOpLinkTicket   = sd.RoadmapOpLinkTicket
	RoadmapOpUnlinkTicket = sd.RoadmapOpUnlinkTicket
	RoadmapOpArchive      = sd.RoadmapOpArchive
	RoadmapOpRestore      = sd.RoadmapOpRestore
)

// RoadmapQuery selects what a roadmap read returns. View is "current"
// (default) or "archive". ID focuses the read on one current item's subtree
// or one archive entry. Depth (current view only) limits levels below the
// focus: nil = full subtree, 0 = the focused item only.
type RoadmapQuery struct {
	View  string
	ID    string
	Depth *int
}

type RoadmapService interface {
	Read(ctx context.Context, architectPath string, query RoadmapQuery) (RoadmapView, error)
	Update(ctx context.Context, architectPath string, params UpdateRoadmapParams) (RoadmapUpdateResult, error)
}
