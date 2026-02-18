package oor

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
)

const (
	// DefaultIncomingPageSize is the default event count fetched per script
	// poll in one source call.
	DefaultIncomingPageSize int32 = 100

	// DefaultIncomingPollInterval is the default delay between background
	// incoming-sync cycles.
	DefaultIncomingPollInterval = 2 * time.Second
)

// OORService is the high-level client API for outgoing transfer orchestration,
// incoming transfer ingestion, and local unroll package resolution.
//
// The service hides low-level actor/FSM/outbox plumbing from application code.
// Callers interact with typed request/response methods while the service keeps
// ordering and idempotency guarantees for incoming cursor progression.
type OORService interface {
	// StartOutgoing starts one outgoing OOR transfer session and returns
	// the stable session identifier.
	StartOutgoing(ctx context.Context, req StartOutgoingRequest) (
		SessionID, error,
	)

	// GetOutgoingState returns the current state summary for one outgoing
	// session.
	GetOutgoingState(ctx context.Context, sessionID SessionID) (
		OutgoingStateView, error,
	)

	// SyncIncomingOnce runs one full incoming-sync cycle across all tracked
	// receive scripts.
	SyncIncomingOnce(ctx context.Context) error

	// StartIncomingSync starts the background incoming-sync loop.
	StartIncomingSync(ctx context.Context) error

	// StopIncomingSync stops the background incoming-sync loop.
	// The call waits for shutdown.
	StopIncomingSync(ctx context.Context) error

	// GetIncomingSyncStatus returns incoming-sync runtime status.
	GetIncomingSyncStatus() IncomingSyncStatus

	// ResolveUnrollPackages resolves stored OOR packages for one
	// outpoint.
	ResolveUnrollPackages(ctx context.Context,
		outpoint wire.OutPoint) (*db.OORUnrollPackages, error)

	// Stop stops the service and all managed workers.
	Stop(ctx context.Context) error
}

// StartOutgoingRequest carries parameters for creating one outgoing transfer.
type StartOutgoingRequest struct {
	// Policy defines the checkpoint policy used to construct the submit
	// package.
	Policy scripts.CheckpointPolicy

	// Inputs are the local VTXOs to spend in the outgoing transfer.
	Inputs []TransferInput

	// Recipients are the Ark transfer outputs.
	Recipients []oortx.RecipientOutput
}

// OutgoingStateView is a stable, caller-facing summary of one session state.
type OutgoingStateView struct {
	// SessionID is the queried outgoing session identifier.
	SessionID SessionID

	// StateName is the concrete FSM state name.
	StateName string

	// Terminal reports whether the current state is terminal.
	Terminal bool

	// FailedReason is populated for terminal failure states.
	FailedReason string

	// RetryAfter is populated when the state is retry-backoff.
	RetryAfter time.Duration

	// RetryReason is populated when the state is retry-backoff.
	RetryReason string
}

// IncomingRecipientEvent is one incoming recipient notification returned by a
// polling source.
//
// EventID is expected to be monotonically increasing per recipient script.
type IncomingRecipientEvent struct {
	// EventID is the per-script cursor value used for resume.
	EventID int64

	// SessionID identifies the transfer session this event belongs to.
	SessionID SessionID

	// RecipientPkScript identifies the script this event targets.
	RecipientPkScript []byte

	// ArkPSBT is the finalized Ark transaction package for the transfer.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs is the finalized checkpoint package set.
	FinalCheckpointPSBTs []*psbt.Packet

	// CreatedAt is the server-side creation time when available.
	CreatedAt time.Time
}

// IncomingEventSource lists incoming recipient events after a cursor.
type IncomingEventSource interface {
	// ListRecipientEvents returns recipient events for one script
	// strictly after afterEventID. Results must be sorted by EventID
	// ascending.
	ListRecipientEvents(
		ctx context.Context,
		recipientPkScript []byte,
		afterEventID int64,
		limit int32,
	) ([]*IncomingRecipientEvent, error)
}

// OwnedReceiveScript is one locally tracked receive script entry.
type OwnedReceiveScript struct {
	// PkScript is the raw script used for recipient polling.
	PkScript []byte
}

// RecipientCursor is the local processing cursor for one recipient script.
type RecipientCursor struct {
	// RecipientPkScript identifies the script this cursor belongs to.
	RecipientPkScript []byte

	// LastEventID is the highest successfully processed event ID.
	LastEventID int64

	// LastSessionID is the last processed session ID when available.
	LastSessionID *SessionID
}

// IncomingCursorStore persists and loads recipient cursors and tracked scripts.
type IncomingCursorStore interface {
	// ListOwnedReceiveScripts returns all scripts that should be polled for
	// incoming events.
	ListOwnedReceiveScripts(
		ctx context.Context,
	) ([]OwnedReceiveScript, error)

	// GetRecipientCursor returns the current cursor for one script.
	// Nil means no cursor row exists yet.
	GetRecipientCursor(ctx context.Context,
		recipientPkScript []byte) (*RecipientCursor, error)

	// UpsertRecipientCursor stores the latest processed cursor for
	// one script.
	UpsertRecipientCursor(ctx context.Context,
		recipientPkScript []byte, lastEventID int64,
		lastSessionID *SessionID) error
}

// UnrollPackageResolver resolves locally persisted OOR package chains by
// outpoint.
type UnrollPackageResolver interface {
	// ResolveUnrollPackages returns the known package chain
	// needed to unroll the target outpoint.
	ResolveUnrollPackages(ctx context.Context,
		outpoint wire.OutPoint) (*db.OORUnrollPackages, error)
}

// IncomingSyncStatus reports runtime state of incoming-sync processing.
type IncomingSyncStatus struct {
	// Running reports whether the background worker is currently active.
	Running bool

	// LastRunStartedAt is when the last sync cycle started.
	LastRunStartedAt time.Time

	// LastRunFinishedAt is when the last sync cycle completed.
	LastRunFinishedAt time.Time

	// LastRunProcessedScripts is how many scripts were
	// processed in the last cycle.
	LastRunProcessedScripts int

	// LastRunProcessedEvents is how many events were processed in
	// the last cycle.
	LastRunProcessedEvents int

	// TotalProcessedScripts is the cumulative number of processed scripts.
	TotalProcessedScripts int64

	// TotalProcessedEvents is the cumulative number of processed events.
	TotalProcessedEvents int64

	// LastError is the last cycle error text, when present.
	LastError string
}

// ServiceConfig configures a concrete OORService implementation.
type ServiceConfig struct {
	// ActorID identifies the outgoing actor instance.
	//
	// When the service constructs the actor locally, this value is used as
	// the durable mailbox ID.
	//
	// When resolving through ActorSystem and no explicit OutgoingServiceKey
	// is provided, this value is used as the service-key name.
	ActorID string

	// ActorSystem provides actor lookup for outgoing OOR commands.
	ActorSystem *actor.ActorSystem

	// OutgoingServiceKey overrides actor-system lookup key for the outgoing
	// actor.
	OutgoingServiceKey *actor.ServiceKey[ActorMsg, ActorResp]

	// OutgoingRef directly injects the outgoing actor reference. This takes
	// precedence over ActorSystem lookup.
	OutgoingRef actor.ActorRef[ActorMsg, ActorResp]

	// DeliveryStore backs durable actor mailbox/checkpoint persistence.
	// Required only when the service constructs the outgoing actor locally.
	DeliveryStore actor.DeliveryStore

	// TransportOutboxHandler handles protocol/network/signing
	// outbox requests not handled by local persistence.
	TransportOutboxHandler OutboxHandler

	// VTXOStore stores local VTXO state updates emitted by OOR
	// outbox handlers.
	VTXOStore vtxo.VTXOStore

	// PackageStore persists outgoing and incoming OOR package artifacts.
	PackageStore PackagePersistence

	// OperatorKey is used to reconstruct incoming VTXO tapscripts.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the unilateral CSV delay used for incoming descriptors.
	ExitDelay uint32

	// ResolveIncomingClientKey resolves wallet key ownership for incoming
	// recipients.
	ResolveIncomingClientKey IncomingClientKeyResolver

	// ResolveIncomingMetadata resolves lineage metadata for
	// incoming recipients.
	ResolveIncomingMetadata IncomingMetadataResolver

	// IncomingSource provides recipient events for incoming polling.
	IncomingSource IncomingEventSource

	// IncomingCursorStore persists owned receive scripts and
	// recipient cursors.
	IncomingCursorStore IncomingCursorStore

	// IncomingPageSize is the per-script page size for event polling.
	IncomingPageSize int32

	// IncomingPollInterval controls background incoming worker cadence.
	IncomingPollInterval time.Duration

	// IncomingPollJitter adds random jitter to poll interval
	// per cycle.
	IncomingPollJitter time.Duration

	// UnrollResolver resolves stored unroll package chains by outpoint.
	UnrollResolver UnrollPackageResolver
}
