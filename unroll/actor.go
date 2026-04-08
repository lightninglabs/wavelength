package unroll

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/baselib/protofsm"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/unrollplan"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// Config configures one durable per-target VTXO unroll actor.
type Config struct {
	// TargetOutpoint is the VTXO being unrolled.
	TargetOutpoint wire.OutPoint

	// ActorID is the durable actor mailbox ID. When empty it
	// falls back to a deterministic ID derived from the target.
	ActorID string

	// DeliveryStore provides durable mailbox and checkpoint persistence.
	DeliveryStore actor.DeliveryStore

	// ProofAssembler resolves the immutable local proof for the target.
	ProofAssembler ProofAssembler

	// VTXOStore loads the descriptor used for final sweep signing.
	VTXOStore vtxo.VTXOStore

	// TxConfirmRef is the shared tx-confirmation actor.
	TxConfirmRef actor.ActorRef[txconfirm.Msg, txconfirm.Resp]

	// ChainSource provides fee estimation for sweep construction.
	ChainSource actor.ActorRef[
		chainsource.ChainSourceMsg, chainsource.ChainSourceResp,
	]

	// Wallet provides sweep destination derivation and
	// timeout-path signing.
	Wallet SweepWallet

	// Log is an optional logger.
	Log fn.Option[btclog.Logger]

	// MaxSweepFeeRateSatPerVByte clamps pathological fee estimates.
	MaxSweepFeeRateSatPerVByte int64

	// RegistryRef receives terminal notifications from this actor when set.
	RegistryRef actor.TellOnlyRef[RegistryMsg]
}

// VTXOUnrollActor wraps one durable per-target unroll actor.
type VTXOUnrollActor struct {
	ref     actor.ActorRef[Msg, Resp]
	durable *actor.DurableActor[Msg, Resp]
	stop    func()
}

// Ref returns the public actor reference.
func (a *VTXOUnrollActor) Ref() actor.ActorRef[Msg, Resp] {
	return a.ref
}

// Stop stops the underlying durable actor.
func (a *VTXOUnrollActor) Stop() {
	if a == nil {
		return
	}

	if a.stop != nil {
		a.stop()
		return
	}

	if a.durable != nil {
		a.durable.Stop()
	}
}

// NewVTXOUnrollActor creates and starts one durable VTXO unroll actor.
func NewVTXOUnrollActor(cfg Config) (*VTXOUnrollActor, error) {
	if cfg.ActorID == "" {
		cfg.ActorID = actorIDForTarget(cfg.TargetOutpoint)
	}

	behavior := &behavior{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
	}
	if err := behavior.restoreCheckpoint(context.Background()); err != nil {
		return nil, err
	}

	durableCfg := actor.DefaultDurableActorConfig[Msg, Resp](
		cfg.ActorID, behavior, cfg.DeliveryStore, newCodec(),
	)
	durableCfg.Log = cfg.Log

	durable := actor.NewDurableActor(durableCfg)
	behavior.selfRef = durable.TellRef()
	durable.Start()

	return &VTXOUnrollActor{
		ref:     durable.Ref(),
		durable: durable,
		stop:    durable.Stop,
	}, nil
}

// behavior is the durable actor behavior for one target outpoint.
type behavior struct {
	cfg     Config
	log     btclog.Logger
	selfRef actor.TellOnlyRef[Msg]

	proof   *recovery.Proof
	planner *unrollplan.Planner
	desc    *vtxo.Descriptor
	session *Session
	pending *actorCheckpoint

	sweepTx          *wire.MsgTx
	blockSubActive   bool
	spendWatchActive bool
	terminalNotified bool
}

// Receive processes one durable actor message.
func (b *behavior) Receive(ctx context.Context, msg Msg) fn.Result[Resp] {
	switch m := msg.(type) {
	case *StartUnrollRequest:
		return b.handleEvent(ctx, &StartEvent{
			Height:  m.Height,
			Trigger: m.Trigger,
		})

	case *ResumeUnrollRequest:
		return b.handleEvent(ctx, &ResumeEvent{
			Height: m.Height,
		})

	case *HeightObservedMsg:
		return b.handleEvent(ctx, &HeightUpdatedEvent{
			Height: m.Height,
		})

	case *TxConfirmedMsg:
		return b.handleEvent(ctx, &TxConfirmedEvent{
			Txid:   m.Txid,
			Height: m.Height,
		})

	case *TxFailedMsg:
		return b.handleEvent(ctx, &TxFailedEvent{
			Txid:   m.Txid,
			Reason: b.failureReasonForTx(m.Txid, m.Reason),
		})

	case *SpendObservedMsg:
		return b.handleSpendObserved(ctx, m)

	case *GetStateRequest:
		return fn.Ok[Resp](b.stateResponse())

	default:
		return fn.Err[Resp](fmt.Errorf(
			"unknown unroll message: %T", msg,
		))
	}
}

// OnStop stops any loaded protofsm session.
func (b *behavior) OnStop(context.Context) error {
	ctx := context.Background()

	b.unsubscribeBlocks(ctx)
	b.unregisterSpendWatch(ctx)

	if b.session != nil && b.session.FSM != nil {
		b.session.FSM.Stop()
	}

	return nil
}

// handleEvent feeds one domain event into the internal protofsm, persists the
// resulting checkpoint, and routes any emitted outbox work.
func (b *behavior) handleEvent(ctx context.Context,
	event Event) fn.Result[Resp] {

	if err := b.ensureLoaded(ctx); err != nil {
		return fn.Err[Resp](err)
	}

	if err := b.driveEvent(ctx, event); err != nil {
		return fn.Err[Resp](err)
	}

	return fn.Ok[Resp](&AckResp{})
}

// driveEvent feeds one event into the protofsm, persists the resulting state,
// and routes any actor-boundary outbox work.
func (b *behavior) driveEvent(ctx context.Context, event Event) error {
	if b.session == nil || b.session.FSM == nil {
		return fmt.Errorf("session not initialized")
	}

	outbox, err := b.session.FSM.AskEvent(ctx, event).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	if err := b.persistCheckpoint(ctx); err != nil {
		return err
	}

	if err := b.routeOutbox(ctx, outbox); err != nil {
		return err
	}

	b.notifyRegistryIfTerminal(ctx)

	return nil
}

// startSweep builds the final timeout sweep and submits it to txconfirm.
func (b *behavior) startSweep(ctx context.Context) error {
	sweepTx, err := buildSweepTx(
		ctx, b.cfg.Wallet, b.cfg.ChainSource, b.proof, b.desc,
		b.cfg.MaxSweepFeeRateSatPerVByte,
	)
	if err != nil {
		return b.driveEvent(ctx, &SweepBuildFailedEvent{
			Reason: err.Error(),
		})
	}

	b.sweepTx = sweepTx
	sweepTxid := sweepTx.TxHash()

	_, err = b.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx: sweepTx,
		ConfirmationPkScript: append(
			[]byte(nil), sweepTx.TxOut[0].PkScript...,
		),
		Label:      "unroll-sweep-" + b.cfg.TargetOutpoint.String(),
		Subscriber: b.notificationRef(),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	return b.driveEvent(ctx, &SweepBroadcastedEvent{Txid: sweepTxid})
}

// ensureNodeConfirmed submits one ready proof node to txconfirm when needed.
func (b *behavior) ensureNodeConfirmed(ctx context.Context,
	txid chainhash.Hash, node *recovery.Node) error {

	if node == nil {
		return fmt.Errorf("proof node %s missing", txid)
	}

	resp, err := b.cfg.TxConfirmRef.Ask(ctx, &txconfirm.EnsureConfirmedReq{
		Tx: node.Tx,
		ConfirmationPkScript: append(
			[]byte(nil), node.Tx.TxOut[0].PkScript...,
		),
		Label:      "unroll-node-" + txid.String(),
		Subscriber: b.notificationRef(),
	}).Await(ctx).Unpack()
	if err != nil {
		return err
	}

	ensureResp, ok := resp.(*txconfirm.EnsureConfirmedResp)
	if !ok {
		return fmt.Errorf("unexpected txconfirm response %T", resp)
	}

	if ensureResp.State == txconfirm.TxStateFailed {
		return b.driveEvent(ctx, &TxFailedEvent{
			Txid: txid,
			Reason: b.failureReasonForTx(
				txid, "txconfirm returned failed state",
			),
		})
	}

	return nil
}

// stateResponse builds the current state response for callers and tests.
func (b *behavior) stateResponse() *GetStateResp {
	state, err := b.currentState()
	if err != nil {
		return &GetStateResp{
			Phase:      PhaseFailed,
			FailReason: err.Error(),
		}
	}

	job := stateJob(state)
	sweepTxid := effectiveSweepTxid(job.PlannerState, b.sweepTx)
	resp := &GetStateResp{
		Started:      !isIdleState(state),
		Trigger:      stateTrigger(state),
		Height:       stateHeight(state),
		Phase:        phaseFromState(state),
		PlannerState: copyPlannerState(job.PlannerState),
		FailReason:   job.FailReason,
	}

	if sweepTxid != nil {
		txid := *sweepTxid
		resp.SweepTxid = &txid
	}

	return resp
}

// ensureLoaded loads the immutable proof, descriptor, and planner.
func (b *behavior) ensureLoaded(ctx context.Context) error {
	if b.proof == nil {
		proof, err := b.cfg.ProofAssembler.EnsureProof(
			ctx, b.cfg.TargetOutpoint,
		)
		if err != nil {
			return err
		}

		b.proof = proof
	}

	if b.desc == nil {
		desc, err := b.cfg.VTXOStore.GetVTXO(ctx, b.cfg.TargetOutpoint)
		if err != nil {
			return err
		}

		b.desc = desc
	}

	if b.planner == nil {
		planner, err := unrollplan.NewPlanner(b.proof)
		if err != nil {
			return err
		}

		b.planner = planner
	}

	if b.session == nil {
		initialState := State(&Idle{})
		if b.pending != nil && b.pending.Started {
			initialState = stateFromCheckpoint(b.pending)
		}

		session, err := NewSession(
			ctx, b.proof, b.planner, initialState, b.log,
		)
		if err != nil {
			return err
		}

		b.session = session
	}

	if err := b.ensureBlockSubscription(ctx); err != nil {
		return err
	}

	if err := b.ensureSpendWatch(ctx); err != nil {
		return err
	}

	state, err := b.currentState()
	if err != nil {
		return err
	}

	return stateJob(state).PlannerState.Validate(b.proof)
}

// notificationRef maps txconfirm notifications into the durable actor mailbox.
func (b *behavior) notificationRef() actor.TellOnlyRef[txconfirm.Notification] {
	return txconfirm.MapNotification(
		b.selfRef,
		func(msg txconfirm.Notification) Msg {
			switch m := msg.(type) {
			case *txconfirm.TxConfirmed:
				return &TxConfirmedMsg{
					Txid:     m.Txid,
					Height:   m.BlockHeight,
					NumConfs: m.NumConfs,
				}

			case *txconfirm.TxFailed:
				return &TxFailedMsg{
					Txid:   m.Txid,
					Reason: m.Reason,
				}

			default:
				return &TxFailedMsg{
					Reason: fmt.Sprintf(
						"unknown txconfirm "+
							"notification %T",
						msg,
					),
				}
			}
		},
	)
}

// restoreCheckpoint restores durable state from the delivery store.
func (b *behavior) restoreCheckpoint(ctx context.Context) error {
	if b.cfg.DeliveryStore == nil {
		return fmt.Errorf("delivery store must be provided")
	}

	checkpoint, err := b.cfg.DeliveryStore.LoadCheckpoint(
		ctx, b.cfg.ActorID,
	)
	if err != nil {
		return err
	}

	if checkpoint == nil {
		return nil
	}

	decoded, err := decodeCheckpoint(checkpoint.StateData)
	if err != nil {
		return err
	}

	if decoded.Version != checkpointVersion {
		return fmt.Errorf(
			"unknown checkpoint version %d",
			decoded.Version,
		)
	}

	b.pending = decoded
	b.sweepTx = copyTx(decoded.SweepTx)

	return nil
}

// ensureBlockSubscription starts the actor's shared block epoch subscription on
// first use so CSV waits advance in the live daemon.
func (b *behavior) ensureBlockSubscription(ctx context.Context) error {
	if b.blockSubActive {
		return nil
	}

	notifyRef := chainsource.MapBlockEpoch(
		b.selfRef,
		func(epoch chainsource.BlockEpoch) Msg {
			return &HeightObservedMsg{
				Height: epoch.Height,
			}
		},
	)

	_, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.SubscribeBlocksRequest{
			CallerID:    b.blockCallerID(),
			NotifyActor: fn.Some(notifyRef),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("subscribe blocks: %w", err)
	}

	b.blockSubActive = true

	return nil
}

// unsubscribeBlocks cancels the actor's block subscription when it stops.
func (b *behavior) unsubscribeBlocks(ctx context.Context) {
	if !b.blockSubActive {
		return
	}

	err := b.cfg.ChainSource.Tell(
		ctx, &chainsource.UnsubscribeBlocksRequest{
			CallerID: b.blockCallerID(),
		},
	)
	if err == nil {
		b.blockSubActive = false
	}
}

// blockCallerID returns the stable chain subscription ID for this actor.
func (b *behavior) blockCallerID() string {
	return fmt.Sprintf("unroll.%s", b.cfg.TargetOutpoint.String())
}

// ensureSpendWatch registers a one-shot spend watch on the target outpoint so
// the actor detects external spends early.
func (b *behavior) ensureSpendWatch(ctx context.Context) error {
	if b.spendWatchActive {
		return nil
	}

	if b.proof == nil || b.desc == nil {
		return nil
	}

	targetOutpoint := b.proof.TargetOutpoint()
	targetNode, ok := b.proof.Node(targetOutpoint.Hash)
	if !ok {
		return fmt.Errorf("target tx %s not in proof",
			targetOutpoint.Hash)
	}

	pkScript := targetNode.Tx.TxOut[targetOutpoint.Index].PkScript

	notifyRef := chainsource.MapSpendEvent(
		b.selfRef,
		func(event chainsource.SpendEvent) Msg {
			return &SpendObservedMsg{
				SpendingTxid:   event.SpendingTxid,
				SpendingHeight: event.SpendingHeight,
			}
		},
	)

	_, err := b.cfg.ChainSource.Ask(
		ctx, &chainsource.RegisterSpendRequest{
			CallerID:    b.spendCallerID(),
			Outpoint:    &targetOutpoint,
			PkScript:    pkScript,
			HeightHint:  uint32(b.desc.CreatedHeight),
			NotifyActor: fn.Some(notifyRef),
		},
	).Await(ctx).Unpack()
	if err != nil {
		return fmt.Errorf("register spend watch: %w", err)
	}

	b.spendWatchActive = true

	return nil
}

// unregisterSpendWatch cancels the actor's target spend watch on stop.
func (b *behavior) unregisterSpendWatch(ctx context.Context) {
	if !b.spendWatchActive {
		return
	}

	targetOutpoint := b.cfg.TargetOutpoint
	err := b.cfg.ChainSource.Tell(
		ctx, &chainsource.UnregisterSpendRequest{
			CallerID: b.spendCallerID(),
			Outpoint: &targetOutpoint,
		},
	)
	if err == nil {
		b.spendWatchActive = false
	}
}

// spendCallerID returns the stable spend-watch registration ID.
func (b *behavior) spendCallerID() string {
	return fmt.Sprintf("unroll-spend.%s",
		b.cfg.TargetOutpoint.String())
}

// handleSpendObserved processes a chainsource spend notification on the target
// outpoint. If the spending tx is our own sweep or a known proof node, the
// event is benign. Otherwise the target was spent externally and the job must
// terminate.
func (b *behavior) handleSpendObserved(ctx context.Context,
	msg *SpendObservedMsg) fn.Result[Resp] {

	if err := b.ensureLoaded(ctx); err != nil {
		return fn.Err[Resp](err)
	}

	// If the spending tx is a known proof node, this is expected
	// materialization traffic handled by txconfirm. Just update height.
	if b.proof != nil {
		if _, ok := b.proof.Node(msg.SpendingTxid); ok {
			return b.handleEvent(ctx, &HeightUpdatedEvent{
				Height: msg.SpendingHeight,
			})
		}
	}

	// Check if the spending tx is our active sweep.
	state, err := b.currentState()
	if err == nil {
		job := stateJob(state)
		if job.PlannerState.Sweep.Txid != nil &&
			*job.PlannerState.Sweep.Txid == msg.SpendingTxid {

			return b.handleEvent(ctx, &HeightUpdatedEvent{
				Height: msg.SpendingHeight,
			})
		}
	}

	// External spend on our target VTXO: terminate.
	reason := fmt.Sprintf(
		"target %s spent externally by tx %s at height %d",
		b.cfg.TargetOutpoint, msg.SpendingTxid,
		msg.SpendingHeight,
	)

	return b.handleEvent(ctx, &FailEvent{Reason: reason})
}

// persistCheckpoint writes the current durable actor checkpoint.
func (b *behavior) persistCheckpoint(ctx context.Context) error {
	state, err := b.currentState()
	if err != nil {
		return err
	}

	checkpoint := checkpointFromState(state, b.sweepTx)
	raw, err := encodeCheckpoint(checkpoint)
	if err != nil {
		return err
	}

	err = b.cfg.DeliveryStore.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   b.cfg.ActorID,
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	if err != nil {
		return err
	}

	b.pending = checkpoint

	return nil
}

// routeOutbox executes one or more actor-boundary side effects emitted by the
// protofsm.
func (b *behavior) routeOutbox(ctx context.Context,
	outbox []OutboxEvent) error {

	for i := range outbox {
		switch evt := outbox[i].(type) {
		case *EnsureReadyTransactions:
			for _, txid := range evt.Txids {
				node, ok := b.proof.Node(txid)
				if !ok {
					return fmt.Errorf("proof node "+
						"%s missing", txid)
				}

				err := b.ensureNodeConfirmed(ctx, txid, node)
				if err != nil {
					return err
				}
			}

		case *ReissueInFlightTransactions:
			for _, txid := range evt.Txids {
				node, ok := b.proof.Node(txid)
				if !ok {
					continue
				}

				pkScript := append(
					[]byte(nil),
					node.Tx.TxOut[0].PkScript...,
				)
				_, err := b.cfg.TxConfirmRef.Ask(
					ctx, &txconfirm.EnsureConfirmedReq{
						Tx:                   node.Tx,
						ConfirmationPkScript: pkScript,
						Label: "unroll-node-" +
							txid.String(),
						Subscriber: b.notificationRef(),
					},
				).Await(ctx).Unpack()
				if err != nil {
					return err
				}
			}

		case *RequestSweepBuild:
			if err := b.startSweep(ctx); err != nil {
				return err
			}

		case *ReissueSweepConfirmation:
			if b.sweepTx == nil {
				continue
			}

			_, err := b.cfg.TxConfirmRef.Ask(
				ctx, &txconfirm.EnsureConfirmedReq{
					Tx: b.sweepTx,
					ConfirmationPkScript: append(
						[]byte(nil),
						b.sweepTx.TxOut[0].PkScript...,
					),
					Label: "unroll-sweep-" +
						b.cfg.TargetOutpoint.String(),
					Subscriber: b.notificationRef(),
				},
			).Await(ctx).Unpack()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// currentState returns the current concrete protofsm state.
func (b *behavior) currentState() (State, error) {
	if b.session != nil && b.session.FSM != nil {
		rawState, err := b.session.FSM.CurrentState()
		if err != nil {
			if errors.Is(err, protofsm.ErrStateMachineShutdown) &&
				b.pending != nil {

				return stateFromCheckpoint(b.pending), nil
			}

			return nil, err
		}

		state, ok := rawState.(State)
		if !ok {
			return nil, fmt.Errorf(
				"unexpected unroll state %T", rawState,
			)
		}

		return state, nil
	}

	if b.pending != nil && b.pending.Started {
		return stateFromCheckpoint(b.pending), nil
	}

	return &Idle{}, nil
}

// failureReasonForTx annotates txconfirm failures with proof-vs-sweep context.
func (b *behavior) failureReasonForTx(txid chainhash.Hash,
	reason string) string {

	if b.pending != nil && b.pending.State.Sweep.Txid != nil &&
		*b.pending.State.Sweep.Txid == txid {

		return fmt.Sprintf("sweep tx %s failed: %s", txid, reason)
	}

	state, err := b.currentState()
	if err == nil {
		job := stateJob(state)
		if job.PlannerState.Sweep.Txid != nil &&
			*job.PlannerState.Sweep.Txid == txid {

			return fmt.Sprintf(
				"sweep tx %s failed: %s",
				txid, reason,
			)
		}
	}

	return fmt.Sprintf(
		"proof tx %s failed: %s", txid, reason,
	)
}

// notifyRegistryIfTerminal forwards terminal state to the optional registry.
func (b *behavior) notifyRegistryIfTerminal(ctx context.Context) {
	if b.cfg.RegistryRef == nil || b.terminalNotified {
		return
	}

	state, err := b.currentState()
	if err != nil {
		b.log.WarnS(ctx, "Failed to inspect unroll terminal state", err)
		return
	}

	phase := phaseFromState(state)
	if phase != PhaseCompleted && phase != PhaseFailed {
		return
	}

	job := stateJob(state)
	msg := &UnrollTerminatedMsg{
		Outpoint:   b.cfg.TargetOutpoint,
		ActorID:    b.cfg.ActorID,
		Phase:      phase,
		FailReason: job.FailReason,
	}

	if sweepTxid := effectiveSweepTxid(
		job.PlannerState, b.sweepTx,
	); sweepTxid != nil {
		msg.SweepTxid = sweepTxid
	}

	if err := b.cfg.RegistryRef.Tell(ctx, msg); err != nil {
		b.log.WarnS(ctx, "Failed to notify unroll registry", err)
		return
	}

	b.terminalNotified = true
}

// actorIDForTarget derives a deterministic actor ID for one target outpoint.
func actorIDForTarget(target wire.OutPoint) string {
	return "unroll-" + target.String()
}

// ActorIDForTarget derives the durable actor ID for one target outpoint.
func ActorIDForTarget(target wire.OutPoint) string {
	return actorIDForTarget(target)
}

// copyPlannerState deep-copies one planner state for durable use.
func copyPlannerState(state unrollplan.State) unrollplan.State {
	copyState := unrollplan.State{
		ConfirmedTxids: append(
			[]chainhash.Hash(nil), state.ConfirmedTxids...,
		),
		InFlightTxids: append(
			[]chainhash.Hash(nil), state.InFlightTxids...,
		),
		Sweep: state.Sweep,
	}

	if state.TargetConfirmHeight != nil {
		copyState.TargetConfirmHeight = copyHeight(
			*state.TargetConfirmHeight,
		)
	}

	if state.Sweep.Txid != nil {
		txid := *state.Sweep.Txid
		copyState.Sweep.Txid = &txid
	}

	if state.Sweep.ConfirmHeight != nil {
		copyState.Sweep.ConfirmHeight = copyHeight(
			*state.Sweep.ConfirmHeight,
		)
	}

	sortHashes(copyState.ConfirmedTxids)
	sortHashes(copyState.InFlightTxids)

	return copyState
}

// copyTx deep-copies one transaction when present.
func copyTx(tx *wire.MsgTx) *wire.MsgTx {
	if tx == nil {
		return nil
	}

	return tx.Copy()
}

// copyHeight returns a heap-independent pointer to one block height.
func copyHeight(height int32) *int32 {
	heightCopy := height
	return &heightCopy
}

// removeHash removes one hash when present.
func removeHash(hashes []chainhash.Hash,
	hash chainhash.Hash) []chainhash.Hash {

	result := make([]chainhash.Hash, 0, len(hashes))
	for _, current := range hashes {
		if current == hash {
			continue
		}

		result = append(result, current)
	}

	return result
}

// containsHash reports whether one hash is present in the slice.
func containsHash(hashes []chainhash.Hash, hash chainhash.Hash) bool {
	for _, current := range hashes {
		if current == hash {
			return true
		}
	}

	return false
}

// sortHashes sorts hashes deterministically by string form.
func sortHashes(hashes []chainhash.Hash) {
	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i].String() < hashes[j].String()
	})
}

// copyHash returns a heap-independent pointer copy of one hash.
func copyHash(hash *chainhash.Hash) *chainhash.Hash {
	if hash == nil {
		return nil
	}

	hashCopy := *hash

	return &hashCopy
}

// appendUniqueSorted appends missing hashes and returns deterministic order.
func appendUniqueSorted(hashes []chainhash.Hash,
	newHashes ...chainhash.Hash) []chainhash.Hash {

	result := append([]chainhash.Hash(nil), hashes...)
	for _, hash := range newHashes {
		if containsHash(result, hash) {
			continue
		}

		result = append(result, hash)
	}

	sortHashes(result)

	return result
}
