package darepo

import (
	"context"
	"fmt"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/oorpb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/oor"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
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
	sessionStore.SetOperatorKey(s.terms.OperatorKey)

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
	vtxoRecordStore.SetOperatorKey(s.terms.OperatorKey)

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
	driver := oor.NewDriver(oor.DriverCfg{
		Locker:            s.vtxoLocker,
		Store:             vtxoRecordStore,
		SessionStore:      sessionStore,
		RecipientEvents:   recipientEvents,
		RecipientNotifier: s.newOORRecipientNotifier(),
		OperatorSigner:    s.walletController,
		OperatorKey:       s.terms.OperatorKey,
		Logger:            oorLog,
	})

	// Build the OOR actor configuration using the same checkpoint
	// policy exposed to clients via GetInfo/round terms.
	oorCfg := oor.ActorCfg{
		Log: fn.Some(oorLog),
		CheckpointPolicy: scripts.CheckpointPolicy{
			OperatorKey: s.terms.OperatorKey.PubKey,
			CSVDelay:    s.terms.VTXOExitDelay,
		},
		OutboxHandler: driver,
		DeliveryStore: deliveryStore,
		SessionStore:  sessionStore,
		ClientsConn:   s.clientBridge,
	}

	// Register the OOR actor with the actor system via its
	// service key, matching the pattern used by the rounds and
	// batch watcher actors. This ensures the actor is managed
	// by the system lifecycle and is discoverable via the
	// receptionist.
	s.oorActor = oor.NewActor(oorCfg)
	oorKey := oor.NewServiceKey()
	s.oorRef = oorKey.Spawn(
		s.actorSystem, oor.OORActorServiceKeyName,
		s.oorActor,
	)

	if err := s.oorActor.Start(ctx); err != nil {
		return fmt.Errorf("start OOR actor: %w", err)
	}

	// Register fire-and-forget dispatch routes for OOR RPCs on
	// the shared event router. Each route deserializes the
	// envelope body, converts the proto to a domain actor
	// message, and Tell's the OOR actor.
	s.registerOORRoutes(oorKey)

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

// registerOORRoutes delegates to the exported RegisterOORRoutes.
func (s *Server) registerOORRoutes(
	oorKey actor.ServiceKey[oor.OORDurableMsg, oor.ActorResp]) {

	RegisterOORRoutes(s.eventRouter, oorKey)
}

// RegisterOORRoutes adds fire-and-forget dispatch routes for OOR
// RPCs (SubmitPackage, FinalizePackage) to the given EventRouter.
// Each route deserializes the envelope body, converts the proto to
// a domain actor message, and Tell's the OOR actor.
//
// OOR RPCs are async: the client submits a request, may go offline,
// and receives the response later via the outbox event path.
//
// This is exported so the systest package can register the same
// routes on its own event router without duplicating definitions.
func RegisterOORRoutes(router *clientconn.EventRouter,
	oorKey actor.ServiceKey[oor.OORDurableMsg, oor.ActorResp]) {

	svc := oorpb.ServiceName

	// SubmitPackage: client submits an OOR transfer package
	// containing the Ark PSBT, checkpoint PSBTs, and signing
	// descriptors. AddEnvelopeRoute extracts the client ID
	// from the envelope sender for response routing.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			oor.OORDurableMsg, oor.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitPackage",
			NewEvent: func() proto.Message {
				return &oorpb.SubmitPackageRequest{}
			},
			Key: oorKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				oor.OORDurableMsg, error) {

				req, ok := p.(*oorpb.SubmitPackageRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T",
						p,
					)
				}

				arkPSBT, checkpointPSBTs,
					descs, err :=
					oorpb.ParseSubmitPackageRequest(
						req,
					)
				if err != nil {
					return nil, fmt.Errorf(
						"parse submit package: %w",
						err,
					)
				}

				vtxoDescs := make(
					[]oor.VTXOSigningDescriptor,
					len(descs),
				)
				for i, d := range descs {
					vtxoDescs[i] =
						oor.VTXOSigningDescriptor{
							Outpoint:  d.Outpoint,
							OwnerKey:  d.OwnerKey,
							ExitDelay: d.ExitDelay,
						}
				}

				return &oor.SubmitOORRequest{
					ClientID: clientconn.ClientID(
						env.Sender,
					),
					ArkPSBT:                arkPSBT,
					CheckpointPSBTs:        checkpointPSBTs,
					VTXOSigningDescriptors: vtxoDescs,
				}, nil
			},
		},
	)

	// FinalizePackage: client submits final checkpoint
	// signatures for an existing OOR session. Uses
	// AddEnvelopeRoute for client ID extraction.
	clientconn.AddEnvelopeRoute(
		router,
		clientconn.EnvelopeRouteConfig[
			oor.OORDurableMsg, oor.ActorResp,
		]{
			Service: svc,
			Method:  "FinalizePackage",
			NewEvent: func() proto.Message {
				return &oorpb.FinalizePackageRequest{}
			},
			Key: oorKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				oor.OORDurableMsg, error) {

				req, ok := p.(*oorpb.FinalizePackageRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T",
						p,
					)
				}

				sessionHash, finalCheckpoints, err :=
					oorpb.ParseFinalizePackageRequest(
						req,
					)
				if err != nil {
					return nil, fmt.Errorf(
						"parse finalize pkg: %w",
						err,
					)
				}

				return &oor.FinalizeOORRequest{
					ClientID: clientconn.ClientID(
						env.Sender,
					),
					SessionID: oor.SessionID(
						sessionHash,
					),
					FinalCheckpointPSBTs: finalCheckpoints,
				}, nil
			},
		},
	)
}
