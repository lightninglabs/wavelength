package oor

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/input"
)

// OutboxHandlerConfig contains the parameters for constructing the
// standard OOR outbox handler chain:
//
//	LocalPersistenceOutboxHandler → SigningOutboxHandler
//
// This config is used by both the production darepod server and
// the systest harness to ensure identical outbox handling.
type OutboxHandlerConfig struct {
	// Signer signs checkpoint and Ark PSBTs.
	Signer input.Signer

	// Store provides VTXO lifecycle persistence.
	Store vtxo.VTXOStore

	// PackageStore persists finalized OOR package artifacts.
	PackageStore PackagePersistence

	// OperatorKey is the operator's public key for checkpoint
	// leaf construction.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the VTXO exit CSV delay.
	ExitDelay uint32

	// TimeoutActor schedules retry timers. When nil, retry
	// requests return RetryDueEvent immediately.
	TimeoutActor *timeout.Actor

	// NotifyIncomingVTXOs is called after incoming VTXOs are
	// durably materialized.
	NotifyIncomingVTXOs IncomingVTXONotifier

	// CompleteSpend routes OOR spend completion through the
	// VTXO manager.
	CompleteSpend SpendCompleter

	// ResolveIncomingClientKey resolves the wallet key for each
	// incoming recipient output.
	ResolveIncomingClientKey IncomingClientKeyResolver

	// ResolveIncomingMetadata resolves lineage metadata for
	// incoming VTXOs.
	ResolveIncomingMetadata IncomingMetadataResolver
}

// NewOutboxHandler constructs the standard two-layer outbox handler
// chain from the given configuration. The returned handler processes
// all outbox events: signing, persistence, transport, and incoming
// materialization.
//
// Both the production darepod and systest should call this function
// to ensure identical outbox handling.
func NewOutboxHandler(
	cfg OutboxHandlerConfig) *LocalPersistenceOutboxHandler {

	signingHandler := &SigningOutboxHandler{
		Signer:       cfg.Signer,
		TimeoutActor: cfg.TimeoutActor,
	}

	return &LocalPersistenceOutboxHandler{
		Next:                    signingHandler,
		Store:                   cfg.Store,
		PackageStore:            cfg.PackageStore,
		OperatorKey:             cfg.OperatorKey,
		ExitDelay:               cfg.ExitDelay,
		NotifyIncomingVTXOs:     cfg.NotifyIncomingVTXOs,
		CompleteSpend:           cfg.CompleteSpend,
		ResolveIncomingClientKey: cfg.ResolveIncomingClientKey,
		ResolveIncomingMetadata: cfg.ResolveIncomingMetadata,
	}
}
