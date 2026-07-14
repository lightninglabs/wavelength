package oor

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/timeout"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/input"
)

// OutboxHandlerConfig contains the parameters for constructing the
// standard OOR outbox handler chain:
//
//	LocalPersistenceOutboxHandler → SigningOutboxHandler
//
// This config is used by both the production waved server and
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

	// TimeoutActor is a TellOnlyRef into the timeout actor's mailbox.
	// Retry scheduling Tells through this ref so all state mutation
	// happens single-threadedly inside the actor's Receive. When nil,
	// retry requests return RetryDueEvent immediately.
	TimeoutActor actor.TellOnlyRef[timeout.Msg]

	// NotifyIncomingVTXOs is called after incoming VTXOs are
	// durably materialized.
	NotifyIncomingVTXOs IncomingVTXONotifier

	// ResolveIncomingClientKey resolves the wallet key for each
	// incoming recipient output.
	ResolveIncomingClientKey IncomingClientKeyResolver

	// ResolveIncomingMetadata resolves lineage metadata for
	// incoming VTXOs.
	ResolveIncomingMetadata IncomingMetadataResolver
}

// NewOutboxHandler constructs the standard two-layer outbox handler chain from
// the given configuration. The returned handler processes local signing,
// persistence, and incoming materialization effects. Durable actor transport
// events are intercepted by OOR before this handler and sent via serverconn.
//
// Both the production waved and systest should call this function
// to ensure identical outbox handling.
func NewOutboxHandler(cfg OutboxHandlerConfig) *LocalPersistenceOutboxHandler {
	signingHandler := &SigningOutboxHandler{
		Signer:       cfg.Signer,
		TimeoutActor: cfg.TimeoutActor,
	}

	return &LocalPersistenceOutboxHandler{
		Next:                     signingHandler,
		Store:                    cfg.Store,
		PackageStore:             cfg.PackageStore,
		OperatorKey:              cfg.OperatorKey,
		ExitDelay:                cfg.ExitDelay,
		NotifyIncomingVTXOs:      cfg.NotifyIncomingVTXOs,
		ResolveIncomingClientKey: cfg.ResolveIncomingClientKey,
		ResolveIncomingMetadata:  cfg.ResolveIncomingMetadata,
	}
}
