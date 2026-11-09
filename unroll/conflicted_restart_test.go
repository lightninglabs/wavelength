package unroll

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// sweptSourceChain replays a historical source spend at registration, as the
// lightweight backend does when an exit resumes after the operator swept.
type sweptSourceChain struct {
	*fakeChainSourceRef
	source     wire.OutPoint
	blocksMu   sync.Mutex
	blocks     map[string]actor.TellOnlyRef[chainsource.BlockEpoch]
	broadcasts atomic.Int32
}

// Ask retains all block subscribers and delivers historical spend evidence.
func (c *sweptSourceChain) Ask(ctx context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	if _, ok := msg.(*chainsource.BroadcastTxRequest); ok {
		c.broadcasts.Add(1)
	}
	if req, ok := msg.(*chainsource.SubscribeBlocksRequest); ok {
		c.blocksMu.Lock()
		c.blocks[req.CallerID] = req.NotifyActor.UnwrapOr(nil)
		c.blocksMu.Unlock()
	}
	future := c.fakeChainSourceRef.Ask(ctx, msg)
	if req, ok := msg.(*chainsource.RegisterSpendRequest); ok &&
		req.Outpoint != nil && *req.Outpoint == c.source {

		// The durable callback may precede the registry's Resume
		// message. Registration belongs to the actor, not this request
		// context.
		req.NotifyActor.WhenSome(func(ref spendEventRef) {
			_ = ref.Tell(
				context.WithoutCancel(ctx),
				chainsource.SpendEvent{
					Outpoint:       c.source,
					SpendingTxid:   chainhash.Hash{0xaa},
					SpendingHeight: 119,
				},
			)
		})
	}

	return future
}

// emitBlock sends an epoch to every registered component, including txconfirm.
func (c *sweptSourceChain) emitBlock(t *testing.T, height int32) {
	t.Helper()
	c.blocksMu.Lock()
	refs := make(
		[]actor.TellOnlyRef[chainsource.BlockEpoch], 0, len(c.blocks),
	)
	for _, ref := range c.blocks {
		refs = append(refs, ref)
	}
	c.blocksMu.Unlock()
	for _, ref := range refs {
		// A terminal child's stale test registration may already be
		// stopped.
		_ = ref.Tell(
			t.Context(), chainsource.BlockEpoch{
				Height: height,
			},
		)
	}
}

// reclaimRound captures only the client-to-round boundary; it does not pretend
// to execute the operator's issuance protocol.
type reclaimRound struct {
	requests chan actormsg.RoundReceivable
}

// ID identifies the test round boundary.
func (r *reclaimRound) ID() string { return "reclaim-round" }

// Tell captures automatic refresh requests.
func (r *reclaimRound) Tell(ctx context.Context,
	msg actormsg.RoundReceivable) error {

	select {
	case r.requests <- msg:
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryTell implements the nonblocking notification surface.
func (r *reclaimRound) TryTell(ctx context.Context,
	msg actormsg.RoundReceivable) error {

	select {
	case r.requests <- msg:
		return nil

	case <-ctx.Done():
		return ctx.Err()

	default:
		return actor.ErrMailboxFull
	}
}

// anchoredMergeProof gives the two-root fixture real CPFP anchor semantics so
// the production broadcaster retries its unfunded packages instead of treating
// them as terminal direct-broadcast errors.
func anchoredMergeProof(t *testing.T) *recovery.Proof {
	t.Helper()
	old := buildMergeProof(t)
	target, _ := old.Node(old.TargetOutpoint().Hash)
	targetTx := target.Tx.Copy()
	var nodes []*recovery.Node
	for _, txid := range old.RootTxids() {
		node, _ := old.Node(txid)
		tx := node.Tx.Copy()
		tx.Version = 3
		tx.AddTxOut(
			&wire.TxOut{
				PkScript: arkscript.AnchorPkScript,
			},
		)
		for _, in := range targetTx.TxIn {
			if in.PreviousOutPoint.Hash == txid {
				in.PreviousOutPoint.Hash = tx.TxHash()
			}
		}
		nodes = append(
			nodes, &recovery.Node{
				Kind: recovery.NodeKindTree,
				Tx:   tx,
			},
		)
	}
	nodes = append(
		nodes, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   targetTx,
		},
	)
	proof, err := recovery.NewProof(
		wire.OutPoint{
			Hash: targetTx.TxHash(),
		},
		2,
		nodes...,
	)
	require.NoError(t, err)

	return proof
}

// TestSweptSourceRestoresPersistedExit exercises the upgrade boundary using
// production delivery, VTXO and registry stores, durable unroll actors, the
// shared broadcaster and a fresh VTXO manager with no actor for the exit.
func TestSweptSourceRestoresPersistedExit(t *testing.T) {
	t.Run(
		"exclusive",
		func(t *testing.T) { testSweptSourceRestart(t, false, false) },
	)
	t.Run(
		"sharedRoot",
		func(t *testing.T) { testSweptSourceRestart(t, true, false) },
	)
}

// otherExitInterest represents another exit that still needs a shared root.
type otherExitInterest struct{}

// ID is distinct from the restored unroll actor's subscriber identity.
func (otherExitInterest) ID() string { return "other-exit" }

// Tell accepts broadcaster notifications for the other exit.
func (otherExitInterest) Tell(context.Context, txconfirm.Notification) error {
	return nil
}

// TryTell never blocks.
func (otherExitInterest) TryTell(context.Context,
	txconfirm.Notification) error {

	return nil
}

// testSweptSourceRestart also checks that cleanup respects shared ownership.
func testSweptSourceRestart(t *testing.T, shared, branch bool) {
	t.Helper()
	proof := anchoredMergeProof(t)
	target := proof.TargetOutpoint()
	desc := testDescriptor(t, target, proof.CSVDelay())
	desc.RoundID = "swept-source-restart"
	desc.BatchExpiry = 110
	desc.CreatedHeight = 1
	desc.Status = vtxo.VTXOStatusUnilateralExit
	policy, err := arkscript.EncodeStandardVTXOTemplate(
		desc.ClientKey.PubKey, desc.OperatorKey, proof.CSVDelay(),
	)
	require.NoError(t, err)
	desc.PolicyTemplate = policy
	sqlDB := db.NewTestDB(t)
	clk := clock.NewDefaultClock()
	stores := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	vtxos := stores.NewVTXOStore(clk)
	require.NoError(t, vtxos.SaveVTXO(t.Context(), desc))
	require.NoError(
		t,
		vtxos.UpdateVTXOStatus(
			t.Context(), target, vtxo.VTXOStatusUnilateralExit,
		),
	)
	delivery, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clk, btclog.Disabled,
	)
	require.NoError(t, err)
	exitStore := stores.NewUnilateralExitStore(clk)
	registryStore := &DBRegistryStore{UEStore: exitStore}
	actorID := actorIDForTarget(target)
	// No conflict record is encoded: this is the pre-1065 checkpoint shape.
	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion, Height: 100, Started: true,
		Trigger: TriggerCriticalExpiry,
		State:   unrollplan.State{InFlightTxids: proof.RootTxids()},
	})
	require.NoError(t, err)
	require.NoError(
		t,
		delivery.SaveCheckpoint(
			t.Context(), actor.CheckpointParams{
				ActorID:   actorID,
				StateType: checkpointStateType,
				StateData: raw,
				Version:   checkpointVersion,
			},
		),
	)
	require.NoError(
		t,
		registryStore.UpsertRecord(
			t.Context(), RegistryRecord{
				TargetOutpoint: target,
				ActorID:        actorID,
				Phase:          PhaseMaterializing,
				Trigger:        TriggerCriticalExpiry,
			},
		),
	)
	sweptOutpoint := sourceOutpoint
	if branch {
		sweptOutpoint = wire.OutPoint{Hash: proof.RootTxids()[0]}
	}
	chain := &sweptSourceChain{
		fakeChainSourceRef: &fakeChainSourceRef{
			bestHeight: 120,
		},
		source: sweptOutpoint,
		blocks: make(
			map[string]actor.TellOnlyRef[chainsource.BlockEpoch],
		),
	}
	system := actor.NewActorSystem()
	t.Cleanup(
		func() {
			require.NoError(
				t,
				system.Shutdown(
					context.Background(),
				),
			)
		},
	)
	roundRef := &reclaimRound{
		requests: make(chan actormsg.RoundReceivable, 8),
	}
	manager := vtxo.NewManager(&vtxo.ManagerConfig{
		Store: vtxos, ActorSystem: system, ChainSource: chain,
		ChainParams:  &chaincfg.RegressionNetParams,
		ExpiryConfig: vtxo.DefaultExpiryConfig(), RoundActor: roundRef,
	})
	managerKey := actor.NewServiceKey[vtxo.ManagerMsg, vtxo.ManagerResp](
		"reclaim-manager",
	)
	managerRef := managerKey.Spawn(system, "reclaim-manager", manager)
	require.NoError(t, manager.Start(t.Context(), managerRef))
	count, err := managerRef.
		Ask(t.Context(), &vtxo.GetActiveVTXOCountRequest{}).
		Await(t.Context()).
		Unpack()
	require.NoError(t, err)
	active, ok := count.(*vtxo.GetActiveVTXOCountResponse)
	require.True(t, ok)
	require.Zero(t, active.Count)
	txBehavior := txconfirm.NewTxBroadcasterActor(txconfirm.Config{
		ChainSource: chain, FeeBumpIntervalBlocks: 1,
	})
	txActor := actor.NewActor(actor.ActorConfig[
		txconfirm.Msg,
		txconfirm.Resp,
	]{
		ID: "restart-txconfirm", Behavior: txBehavior, MailboxSize: 64,
	})
	txBehavior.SetSelfRef(txActor.TellRef())
	txActor.Start()
	t.Cleanup(txActor.Stop)
	var sharedTxid chainhash.Hash
	if shared {
		for _, txid := range proof.RootTxids() {
			node, _ := proof.Node(txid)
			if node.Tx.TxIn[0].PreviousOutPoint == sourceOutpoint {
				continue
			}
			sharedTxid = txid
			_, err := txActor.Ref().Ask(
				t.Context(), &txconfirm.EnsureConfirmedReq{
					Tx:         node.Tx,
					Subscriber: otherExitInterest{},
				},
			).Await(t.Context()).Unpack()
			require.NoError(t, err)
		}
	}
	observer := fn.Some[actor.TellOnlyRef[vtxo.ManagerMsg]](managerRef)
	registry := NewUnrollRegistryActor(
		RegistryConfig{
			Store:            registryStore,
			DeliveryStore:    delivery,
			ProofAssembler:   &mockProofAssembler{proof: proof},
			VTXOStore:        vtxos,
			TxConfirmRef:     txActor.Ref(),
			ChainSource:      chain,
			Wallet:           &fakeSweepWallet{},
			VTXOExitObserver: observer,
		},
	)
	t.Cleanup(registry.Stop)
	require.NoError(t, registry.RestoreNonTerminal(t.Context()))
	require.Eventually(t, func() bool {
		record, err := registryStore.GetRecord(t.Context(), target)

		return err == nil && record != nil && record.ConflictedFailure
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		coin, err := vtxos.GetVTXO(t.Context(), target)

		return err == nil && coin.Status == vtxo.VTXOStatusExpired
	}, 5*time.Second, 10*time.Millisecond)
	// Replay through the real manager and its now-expired child, as when
	// terminal handoff is re-driven after a crash. No error or fresh status
	// transition should prevent the following epoch from reclaiming.
	_, err = managerRef.Ask(t.Context(), &vtxo.ExitOutcomeNotification{
		Outpoint: target,
		Outcome:  vtxo.ExitOutcomeConflicted,
		Reason:   "replayed source conflict",
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	// The expired actor is not spendable but must survive to request
	// refresh.
	live, err := vtxos.ListLiveVTXOs(t.Context())
	require.NoError(t, err)
	require.Empty(t, live)
	chain.emitBlock(t, 121)
	select {
	case msg := <-roundRef.requests:
		req, ok := msg.(*round.RefreshVTXORequest)
		require.True(t, ok, "unexpected round request %T", msg)
		require.Equal(t, target, req.VTXOOutpoint)
		require.True(t, req.Automatic)

	case <-time.After(5 * time.Second):
		t.Fatal("restored conflicted exit did not request refresh")
	}
	// Cleanup must remove only this unroll's broadcaster interests. A root
	// with another owner must remain tracked and must not be
	// wallet-removed.
	cleanupRoots := proof.RootTxids()
	if branch {
		// The branch spend confirms its parent. Deliver the matching
		// chain confirmation to the broadcaster too; only the other
		// root is abandoned and eligible for wallet removal.
		chain.emitConfirmed(t, cleanupRoots[0], 119)
		cleanupRoots = cleanupRoots[1:]
	}
	wantRemoved := len(cleanupRoots)
	if shared {
		wantRemoved--
	}
	require.Eventually(t, func() bool {
		return len(chain.removedTxSnapshot()) == wantRemoved
	}, 5*time.Second, 10*time.Millisecond)
	// Drain the broadcaster after detached cleanup before probing
	// ownership.
	for _, txid := range cleanupRoots {
		require.Eventually(t, func() bool {
			resp, err := txActor.Ref().Ask(
				t.Context(), &txconfirm.CancelInterestReq{
					Txid:         txid,
					SubscriberID: "not-a-subscriber",
				},
			).Await(t.Context()).Unpack()
			if err != nil {
				return false
			}
			want := 0
			if shared && txid == sharedTxid {
				want = 1
			}

			result, ok := resp.(*txconfirm.CancelInterestResp)

			return ok && result.RemainingSubscribers == want
		}, 5*time.Second, 10*time.Millisecond)
	}
	before := chain.broadcasts.Load()
	chain.emitBlock(t, 122)
	for _, txid := range cleanupRoots {
		resp, err := txActor.Ref().Ask(
			t.Context(), &txconfirm.CancelInterestReq{
				Txid: txid, SubscriberID: "not-a-subscriber",
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)
		result, ok := resp.(*txconfirm.CancelInterestResp)
		require.True(t, ok)
		require.False(
			t, result.StoppedTracking,
			"conflicted root still tracked after terminal cleanup",
		)
	}
	if shared {
		require.NotContains(t, chain.removedTxSnapshot(), sharedTxid)
		require.Equal(
			t, before+1, chain.broadcasts.Load(),
			"only the other exit's root should retry",
		)
	} else {
		require.Equal(
			t, before, chain.broadcasts.Load(),
			"abandoned roots retried after reclaim",
		)
	}
	registry.Stop()
	checkpoint, err := delivery.LoadCheckpoint(t.Context(), actorID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	decoded, err := decodeCheckpoint(checkpoint.StateData)
	require.NoError(t, err)
	require.True(t, decoded.Conflicted)

	// Terminal conflict rows must not resume on subsequent daemon starts.
	records, err := registryStore.ListNonTerminalRecords(t.Context())
	require.NoError(t, err)
	require.Empty(t, records)
}

// TestSweptBranchRestoresPersistedExit proves a confirmed branch sweep reaches
// expired reclaim through the real delivery, registry and VTXO stores.
func TestSweptBranchRestoresPersistedExit(t *testing.T) {
	testSweptSourceRestart(t, false, true)
}
