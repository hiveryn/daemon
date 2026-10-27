package domain

import sd "github.com/hiveryn/shared/domain"

// Re-export the context reference grammar and worker context read contract.
type (
	ReferenceKind        = sd.ReferenceKind
	ParsedReference      = sd.ParsedReference
	ReferenceSyntaxError = sd.ReferenceSyntaxError
	ReferenceStatus      = sd.ReferenceStatus
	ReferenceLocation    = sd.ReferenceLocation
	ReferenceAccess      = sd.ReferenceAccess
	PathKind             = sd.PathKind

	ContextReadStatus  = sd.ContextReadStatus
	ContextReadRequest = sd.ContextReadRequest
	ContextTarget      = sd.ContextTarget
	ContextFile        = sd.ContextFile
	ContextListRequest = sd.ContextListRequest
	ContextEntryType   = sd.ContextEntryType
	ContextEntry       = sd.ContextEntry
	ContextListing     = sd.ContextListing
)

const (
	ArchitectReferencePrefix = sd.ArchitectReferencePrefix
	ReferenceRootTarget      = sd.ReferenceRootTarget

	ReferenceKindTicket    = sd.ReferenceKindTicket
	ReferenceKindArchitect = sd.ReferenceKindArchitect
	ReferenceKindRepo      = sd.ReferenceKindRepo

	ReferencePresent    = sd.ReferencePresent
	ReferenceMissing    = sd.ReferenceMissing
	ReferenceUnverified = sd.ReferenceUnverified
	ReferenceInvalid    = sd.ReferenceInvalid

	ReferenceLocal   = sd.ReferenceLocal
	ReferenceRemote  = sd.ReferenceRemote
	ReferenceUnknown = sd.ReferenceUnknown

	ReferenceAccessReadTicket  = sd.ReferenceAccessReadTicket
	ReferenceAccessReadContext = sd.ReferenceAccessReadContext
	ReferenceAccessListContext = sd.ReferenceAccessListContext

	PathKindFile      = sd.PathKindFile
	PathKindDirectory = sd.PathKindDirectory

	ContextOK               = sd.ContextOK
	ContextMissing          = sd.ContextMissing
	ContextPermissionDenied = sd.ContextPermissionDenied
	ContextNotFile          = sd.ContextNotFile
	ContextNotDirectory     = sd.ContextNotDirectory
	ContextBinary           = sd.ContextBinary
	ContextTooLarge         = sd.ContextTooLarge
	ContextChanged          = sd.ContextChanged
	ContextUnreachable      = sd.ContextUnreachable
	ContextOutsideRoot      = sd.ContextOutsideRoot

	MaxContextFileBytes   = sd.MaxContextFileBytes
	DefaultContextLines   = sd.DefaultContextLines
	MaxContextLines       = sd.MaxContextLines
	MaxContextPageBytes   = sd.MaxContextPageBytes
	DefaultContextEntries = sd.DefaultContextEntries
	MaxContextEntries     = sd.MaxContextEntries

	ContextEntryFile      = sd.ContextEntryFile
	ContextEntryDirectory = sd.ContextEntryDirectory
	ContextEntrySymlink   = sd.ContextEntrySymlink
	ContextEntryOther     = sd.ContextEntryOther
)

var (
	ParseReference = sd.ParseReference
	ValidRepoKey   = sd.ValidRepoKey
)
