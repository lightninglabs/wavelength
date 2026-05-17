package oor

import (
	"context"

	"github.com/btcsuite/btcd/btcutil/psbt"
)

// StoredClientSession is the SQL-store boundary shape used to rebuild one OOR
// FSM without depending on generic checkpoint blobs.
type StoredClientSession struct {
	Direction SessionDirection

	Outgoing *OutgoingSnapshot
	Incoming *IncomingSnapshot
}

// OORClientSessionStore is the OOR-owned durable state boundary.
// Implementations should persist session rows and artifact rows
// transactionally; the actor keeps only in-memory FSM handles.
type OORClientSessionStore interface {
	LoadActiveSessions(ctx context.Context) ([]StoredClientSession, error)

	FindOutgoingByIdempotencyKey(ctx context.Context,
		idempotencyKey string) (SessionID, bool, error)

	SaveOutgoingSession(ctx context.Context,
		snapshot *OutgoingSnapshot) error

	SaveIncomingSession(ctx context.Context,
		snapshot *IncomingSnapshot) error

	SavePendingIncomingHint(ctx context.Context,
		req *ResolveIncomingTransferRequest) error
}

// OORClientIncomingEffectStore is the optional SQL boundary for incoming
// metadata responses that emit materialization work without changing the
// receive FSM state. Implementations persist the metadata facts and the
// materialize effect in one transaction so restart does not depend on an
// in-memory outbox chain.
type OORClientIncomingEffectStore interface {
	SaveIncomingMetadataEffect(ctx context.Context, sessionID SessionID,
		matches []IncomingMetadataMatch) error

	BuildMaterializeIncomingVTXOsRequest(ctx context.Context,
		sessionID SessionID) (*MaterializeIncomingVTXOsRequest, error)
}

// OORClientSigningArtifactStore is the optional SQL artifact boundary used by
// the signing outbox handler. Persisting signed artifacts before emitting
// follow-up FSM events makes a restart between signing and FSM advancement
// replay from SQL rather than asking the signer to produce the same artifacts
// again.
type OORClientSigningArtifactStore interface {
	LoadArkSignedArtifact(ctx context.Context, sessionID SessionID) (
		*psbt.Packet, bool, error)

	SaveArkSignedArtifact(ctx context.Context, sessionID SessionID,
		ark *psbt.Packet) error

	LoadFinalCheckpointArtifacts(ctx context.Context, sessionID SessionID,
		expectedCount int) ([]*psbt.Packet, bool, error)

	SaveFinalCheckpointArtifacts(ctx context.Context, sessionID SessionID,
		checkpoints []*psbt.Packet) error
}
