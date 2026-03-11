package darepo

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/oor"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// setupOORSubsystem initializes the OOR transfer coordinator actor.
//
// The OOR actor drives out-of-round transfer sessions through their
// FSM lifecycle: submit, co-sign, finalize, and notify. It uses a
// DurableActor for crash-safe session recovery via DB-authoritative
// state.
//
// This must be called after the indexer subsystem so that the shared
// DB store and bridge are available.
func (s *Server) setupOORSubsystem(ctx context.Context) error {
	clk := clock.NewDefaultClock()
	oorLog := subLogger(s.cfg.Loggers, oor.Subsystem)

	// Use the shared db.Store and vtxoLocker rather than creating
	// redundant wrappers. This ensures a single locker instance
	// governs VTXO exclusion across rounds and OOR.

	// Create the DB-backed session store for crash-safe session
	// persistence.
	sessionStore := oor.NewDBSessionStore(
		s.db, clk, oorLog,
	)

	// Create the DB-backed delivery store for durable actor
	// mailbox checkpoints.
	deliveryStore, err := db.NewActorDeliveryStoreFromDB(
		s.db, clk, oorLog,
	)
	if err != nil {
		return fmt.Errorf("create OOR delivery store: %w", err)
	}

	// Create the VTXO record store for input lock/status
	// tracking during OOR sessions.
	vtxoRecordStore := s.db.NewVTXORecordStore()

	// Create the DB-backed recipient event store adapter that
	// satisfies oor.RecipientEventStore (the oor package's
	// DBRecipientEventStore wraps the raw db store with session
	// ID resolution).
	recipientEvents := oor.NewDBRecipientEventStore(
		s.db, clk, oorLog,
	)

	// Build the in-process outbox driver with all DB-backed
	// stores. The driver handles locking, signing, persistence,
	// and notification for each outbox event type.
	//
	// TODO(roasbeef): Wire the operator key and signer from
	// LND once key management is in place.
	driver := oor.NewDriver(oor.DriverCfg{
		Locker:          s.vtxoLocker,
		Store:           vtxoRecordStore,
		SessionStore:    sessionStore,
		RecipientEvents: recipientEvents,
	})

	// Build the OOR actor configuration. The checkpoint policy
	// will be populated from server config once operator key
	// derivation is wired.
	//
	// TODO(roasbeef): Wire CheckpointPolicy from operator key +
	// CSV delay config.
	oorCfg := oor.ActorCfg{
		Log:              fn.Some(oorLog),
		CheckpointPolicy: scripts.CheckpointPolicy{},
		OutboxHandler:    driver,
		DeliveryStore:    deliveryStore,
		SessionStore:     sessionStore,
	}

	s.oorActor = oor.NewActor(oorCfg)
	if err := s.oorActor.Start(ctx); err != nil {
		return fmt.Errorf("start OOR actor: %w", err)
	}

	// Create the OOR operator that provides mailbox RPC dispatchers
	// for the per-client ingress loops. The local mailbox edge
	// client is shared with other subsystems.
	edgeClient, err := newLocalMailboxClient(s.mailboxStore)
	if err != nil {
		return fmt.Errorf("build OOR edge client: %w", err)
	}

	s.oorOperator, err = oor.NewOOROperator(
		oor.OOROperatorConfig{
			Edge:     edgeClient,
			OORActor: s.oorActor,
		},
	)
	if err != nil {
		return fmt.Errorf("create OOR operator: %w", err)
	}

	s.log.InfoS(ctx, "OOR subsystem initialized")

	return nil
}

// stopOORSubsystem releases OOR-related resources by stopping the
// durable actor runtime.
func (s *Server) stopOORSubsystem(ctx context.Context) {
	if s.oorActor != nil {
		s.oorActor.Stop()

		s.log.InfoS(ctx, "OOR subsystem stopped")
	}
}

// OORDispatchers returns the OOR operator's DispatcherMap for merging
// into per-client PerClientConfig.Dispatchers during client
// registration.
//
// Returns nil if the OOR subsystem has not been initialized.
func (s *Server) OORDispatchers() clientconn.DispatcherMap {
	if s.oorOperator == nil {
		return nil
	}

	return s.oorOperator.Dispatchers()
}
