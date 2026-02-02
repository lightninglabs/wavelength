package vtxo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// VTXOActorRef is the actor reference type for VTXO actors. Uses
// actormsg.VTXOActorMsg as the message type to enable both round and vtxo
// packages to use the same service key for registration and lookup.
type VTXOActorRef = actor.ActorRef[
	actormsg.VTXOActorMsg, actormsg.VTXOActorResp,
]

// ManagerConfig holds configuration for the VTXO Manager.
type ManagerConfig struct {
	Store       VTXOStore
	Wallet      VTXOWallet
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]
	ActorSystem  *actor.ActorSystem
	ChainParams  *chaincfg.Params
	ExpiryConfig *ExpiryConfig
	Logger       btclog.Logger

	// RoundActor receives refresh requests and forfeit signatures from VTXOs.
	// Passed through to spawned VTXO actors. Uses actormsg.RoundReceivable to
	// avoid import cycles.
	RoundActor actor.TellOnlyRef[actormsg.RoundReceivable]

	// ChainResolver receives expiring notifications for unilateral exit.
	// Passed through to spawned VTXO actors.
	ChainResolver actor.TellOnlyRef[ExpiringNotification]
}

// Manager coordinates VTXO actor lifecycle - spawning new actors when VTXOs
// are created and recovering persisted actors on startup. Each VTXO actor
// manages its own block epoch subscription and communicates directly with the
// round actor via service keys.
type Manager struct {
	cfg *ManagerConfig

	// managerRef is the manager's own actor ref, used for creating mapped
	// refs that VTXO actors can use to send termination notifications.
	managerRef actor.TellOnlyRef[ManagerMsg]

	// actors tracks active VTXO actors by outpoint.
	actors map[wire.OutPoint]VTXOActorRef
}

// NewManager creates a new VTXO Manager.
func NewManager(cfg *ManagerConfig) *Manager {
	if cfg.ExpiryConfig == nil {
		cfg.ExpiryConfig = DefaultExpiryConfig()
	}

	return &Manager{
		cfg:    cfg,
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}
}

// Start initializes the manager by recovering persisted VTXOs. The selfRef
// parameter is the manager's own actor reference, used for VTXO actors to
// send termination notifications.
func (m *Manager) Start(ctx context.Context,
	selfRef actor.TellOnlyRef[ManagerMsg]) error {

	m.managerRef = selfRef

	vtxos, err := m.cfg.Store.ListLiveVTXOs(ctx)
	if err != nil {
		return fmt.Errorf("list live vtxos: %w", err)
	}

	for _, vtxo := range vtxos {
		ref, err := m.spawnVTXOActor(ctx, vtxo)
		if err != nil {
			m.cfg.Logger.ErrorS(
				ctx, "Failed to recover VTXO actor", err,
				slog.String("outpoint", vtxo.Outpoint.String()),
			)

			continue
		}

		m.actors[vtxo.Outpoint] = ref

		m.cfg.Logger.InfoS(ctx, "Recovered VTXO actor",
			slog.String("outpoint", vtxo.Outpoint.String()),
			slog.String("status", vtxo.Status.String()))
	}

	m.cfg.Logger.InfoS(ctx, "VTXO manager started",
		slog.Int("recovered", len(m.actors)))

	return nil
}

// Stop gracefully shuts down the manager.
func (m *Manager) Stop(ctx context.Context) {
	m.cfg.Logger.InfoS(ctx, "VTXO manager stopped")
}

// Receive processes incoming messages.
func (m *Manager) Receive(ctx context.Context,
	msg ManagerMsg) fn.Result[ManagerResp] {

	switch req := msg.(type) {
	case *round.VTXOCreatedNotification:
		return m.handleVTXOCreated(ctx, req)

	case *round.VTXOTerminatedMsg:
		return m.handleVTXOTerminated(ctx, req)

	case *GetActiveVTXOCountRequest:
		return fn.Ok[ManagerResp](&GetActiveVTXOCountResponse{
			Count: len(m.actors),
		})

	default:
		return fn.Err[ManagerResp](
			fmt.Errorf("unknown message: %T", msg),
		)
	}
}

// handleVTXOCreated spawns a new VTXO actor for each created VTXO.
func (m *Manager) handleVTXOCreated(ctx context.Context,
	msg *round.VTXOCreatedNotification) fn.Result[ManagerResp] {

	for _, clientVTXO := range msg.VTXOs {
		outpoint := clientVTXO.Outpoint

		if _, exists := m.actors[outpoint]; exists {
			m.cfg.Logger.WarnS(ctx,
				"VTXO actor already exists", nil,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		result := clientVTXOToDescriptor(clientVTXO, msg)
		descriptor, err := result.Unpack()
		if err != nil {
			m.cfg.Logger.ErrorS(ctx,
				"Failed to build descriptor", err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		if err := m.cfg.Store.SaveVTXO(ctx, descriptor); err != nil {
			m.cfg.Logger.ErrorS(ctx, "Failed to save VTXO", err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		ref, err := m.spawnVTXOActor(ctx, descriptor)
		if err != nil {
			m.cfg.Logger.ErrorS(ctx,
				"Failed to spawn VTXO actor", err,
				slog.String("outpoint", outpoint.String()),
			)

			continue
		}

		m.actors[outpoint] = ref

		m.cfg.Logger.InfoS(ctx, "Spawned VTXO actor",
			slog.String("outpoint", outpoint.String()),
			slog.Int64("amount", int64(clientVTXO.Amount)),
			slog.Int("batch_expiry", int(msg.BatchExpiry)))
	}

	return fn.Ok[ManagerResp](&VTXOCreatedResp{})
}

// handleVTXOTerminated removes a VTXO actor from tracking when it reaches
// a terminal state (Forfeited, Failed, etc.).
func (m *Manager) handleVTXOTerminated(ctx context.Context,
	msg *round.VTXOTerminatedMsg) fn.Result[ManagerResp] {

	delete(m.actors, msg.Outpoint)

	m.cfg.Logger.InfoS(ctx, "VTXO actor terminated",
		slog.String("outpoint", msg.Outpoint.String()),
		slog.String("final_state", msg.FinalState),
		slog.String("reason", msg.Reason))

	return fn.Ok[ManagerResp](&VTXOTerminatedResp{})
}

// spawnVTXOActor creates a new VTXO FSM actor.
func (m *Manager) spawnVTXOActor(ctx context.Context,
	vtxo *Descriptor) (VTXOActorRef, error) {

	actorID := fmt.Sprintf("vtxo.%s", vtxo.Outpoint.String())
	serviceKey := VTXOActorServiceKey(vtxo.Outpoint)

	actorCfg := &VTXOActorConfig{
		VTXO:          vtxo,
		Store:         m.cfg.Store,
		Wallet:        m.cfg.Wallet,
		ChainSource:   m.cfg.ChainSource,
		ChainParams:   m.cfg.ChainParams,
		ExpiryConfig:  m.cfg.ExpiryConfig,
		Logger:        m.cfg.Logger,
		RoundActor:    m.cfg.RoundActor,
		ChainResolver: m.cfg.ChainResolver,
		Manager:       m.managerRef,
	}

	vtxoActor := NewVTXOActor(ctx, actorCfg)
	ref := serviceKey.Spawn(m.cfg.ActorSystem, actorID, vtxoActor)

	// Start the actor to subscribe to block epochs. ActorRef embeds
	// TellOnlyRef so we can use it directly.
	if err := vtxoActor.Start(ctx, ref); err != nil {
		// Stop the spawned actor to prevent a zombie actor in the
		// system.
		m.cfg.ActorSystem.StopAndRemoveActor(actorID)

		return ref, fmt.Errorf("start vtxo actor: %w", err)
	}

	return ref, nil
}

// clientVTXOToDescriptor converts a round.ClientVTXO to a Descriptor using
// metadata from the VTXOCreatedNotification. TreeDepth and TapScript are
// computed from the VTXO data since each VTXO may be at a different depth.
func clientVTXOToDescriptor(cv *round.ClientVTXO,
	msg *round.VTXOCreatedNotification) fn.Result[*Descriptor] {

	// Compute tree depth from the VTXO's path. Each VTXO may be at a
	// different depth in the commitment tree.
	var treeDepth int
	if cv.TreePath != nil {
		treeDepth = cv.TreePath.Depth()
	}

	// Construct the TapScript from the client and operator keys. This is
	// the standard VTXO tapscript with collaborative and timeout paths.
	tapscript, err := scripts.VTXOTapScript(
		cv.ClientKey.PubKey, cv.OperatorKey, cv.Expiry,
	)
	if err != nil {
		return fn.Err[*Descriptor](
			fmt.Errorf("build tapscript: %w", err),
		)
	}

	return fn.Ok(&Descriptor{
		Outpoint:       cv.Outpoint,
		Amount:         cv.Amount,
		PkScript:       cv.PkScript,
		ClientKey:      cv.ClientKey,
		OperatorKey:    cv.OperatorKey,
		TapScript:      tapscript,
		TreePath:       cv.TreePath,
		RoundID:        msg.RoundID,
		CommitmentTxID: msg.CommitmentTxID,
		BatchExpiry:    msg.BatchExpiry,
		RelativeExpiry: cv.Expiry,
		TreeDepth:      treeDepth,
		CreatedHeight:  msg.CreatedHeight,
		Status:         VTXOStatusLive,
	})
}
