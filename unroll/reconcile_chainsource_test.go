package unroll

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// reconcileMockBackend is a minimal ChainBackend test double tailored
// for the reconciler concurrency test. It records every RegisterConf
// call so the test can assert per-probe caller-ID uniqueness, and
// pre-arms a positive confirmation reply that fires immediately when
// the txid matches the configured target.
type reconcileMockBackend struct {
	confirmTxid   chainhash.Hash
	confirmHeight int32

	// registers records (callerID-like txid, pkScript) pairs as seen
	// from successive RegisterConf calls. The caller-ID is not visible
	// at this layer (chainsource consumes it before spawning the sub-
	// actor), so the test instead asserts that the sub-actor IDs the
	// system spawned are distinct via System.Has on the service-key.
	mu        sync.Mutex
	registers []struct {
		Txid     chainhash.Hash
		PkScript []byte
	}

	// bestHeight is reported by BestBlock.
	bestHeight int32

	// epochCh / epochCancel back RegisterBlocks.
	epochCh     chan *chainsource.BlockEpoch
	epochCancel atomic.Int32
}

func newReconcileMockBackend() *reconcileMockBackend {
	return &reconcileMockBackend{
		bestHeight: 200,
		epochCh:    make(chan *chainsource.BlockEpoch, 1),
	}
}

func (m *reconcileMockBackend) EstimateFee(context.Context, uint32) (
	btcutil.Amount, error) {

	return 1000, nil
}

func (m *reconcileMockBackend) BestBlock(context.Context) (int32,
	chainhash.Hash, error) {

	return m.bestHeight, chainhash.Hash{}, nil
}

func (m *reconcileMockBackend) TestMempoolAccept(context.Context,
	...*wire.MsgTx) ([]chainsource.MempoolAcceptResult, error) {

	return nil, nil
}

func (m *reconcileMockBackend) BroadcastTx(context.Context, *wire.MsgTx,
	string) error {

	return nil
}

func (m *reconcileMockBackend) SubmitPackage(context.Context, []*wire.MsgTx,
	*wire.MsgTx) error {

	return nil
}

// RegisterConf records the registration and arms a one-shot positive
// confirmation when the requested txid matches the configured target.
// Every call returns a fresh ConfRegistration with its own channels so
// concurrent registrants do not contend on a shared buffered channel.
func (m *reconcileMockBackend) RegisterConf(_ context.Context,
	txid *chainhash.Hash, pkScript []byte, _, _ uint32, _ bool) (
	*chainsource.ConfRegistration, error) {

	m.mu.Lock()
	m.registers = append(m.registers, struct {
		Txid     chainhash.Hash
		PkScript []byte
	}{
		Txid:     *txid,
		PkScript: append([]byte(nil), pkScript...),
	})
	m.mu.Unlock()

	confCh := make(chan *chainsource.TxConfirmation, 1)
	reorged := make(chan uint64, 1)
	done := make(chan struct{}, 1)

	if txid != nil && *txid == m.confirmTxid {
		blockTx := wire.NewMsgTx(2)
		blockHash := chainhash.Hash{0x42}
		confCh <- &chainsource.TxConfirmation{
			BlockHash:   &blockHash,
			BlockHeight: uint32(m.confirmHeight),
			Tx:          blockTx,
		}
	}

	return &chainsource.ConfRegistration{
		Confirmed: confCh,
		Reorged:   reorged,
		Done:      done,
		Cancel:    func() {},
	}, nil
}

func (m *reconcileMockBackend) RegisterSpend(context.Context, *wire.OutPoint,
	[]byte, uint32) (*chainsource.SpendRegistration, error) {

	return &chainsource.SpendRegistration{
		Spend:   make(chan *chainsource.SpendDetail, 1),
		Reorged: make(chan uint64, 1),
		Done:    make(chan struct{}, 1),
		Cancel:  func() {},
	}, nil
}

func (m *reconcileMockBackend) RegisterBlocks(context.Context) (
	*chainsource.BlockRegistration, error) {

	return &chainsource.BlockRegistration{
		Epochs: m.epochCh,
		Cancel: func() {
			m.epochCancel.Add(1)
		},
	}, nil
}

func (m *reconcileMockBackend) Start() error { return nil }
func (m *reconcileMockBackend) Stop() error  { return nil }

// TestChainSourceReconcilerConcurrentProbesDoNotCollide exercises the
// invariant that two reconcilers probing the same shared proof-graph
// txid against a single chainsource actor do NOT collide on chainsource
// service keys when each reconciler is built with a target-specific
// caller-ID prefix.
//
// Chainsource keys its per-probe sub-actors on
// (CallerID, txid/pkScript, TargetConfs) (see handleRegisterConf in
// chainsource/chainsource.go). Two reconcilers built with the same
// static prefix probing the same txid would map to identical service
// keys; the second Spawn would collide with the first sub-actor's
// registration and drop or merge the second probe. Production wiring
// in darepod/server.go bakes the per-actor target outpoint into the
// caller-ID prefix exactly to avoid this; this test pins that
// invariant for the chainsource-backed reconciler.
func TestChainSourceReconcilerConcurrentProbesDoNotCollide(t *testing.T) {
	t.Parallel()

	proof := buildLinearProof(t)
	roots := proof.RootTxids()
	require.Len(t, roots, 1, "linear proof should have one root")
	rootTxid := roots[0]

	backend := newReconcileMockBackend()
	backend.confirmTxid = rootTxid
	backend.confirmHeight = 150

	system := actor.NewActorSystem()
	defer func() {
		_ = system.Shutdown(t.Context())
	}()

	chainSource := chainsource.NewChainSourceActor(
		chainsource.ChainSourceConfig{
			Backend: backend,
			System:  system,
		},
	)
	chainRef := chainsource.ChainSourceKey.Spawn(
		system, "chainsource-reconcile", chainSource,
	)

	target1 := proof.TargetOutpoint()
	target2 := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
		},
		Index: 7,
	}

	mkReconciler := func(target wire.OutPoint) ChainReconciler {
		return NewChainSourceReconciler(ChainSourceReconcilerConfig{
			ChainSource: chainRef,
			Proof:       proof,
			CallerID: fmt.Sprintf(
				"unroll-reconcile-%s", target,
			),
			ProbeTimeout: 5 * time.Second,
		})
	}

	rec1 := mkReconciler(target1)
	rec2 := mkReconciler(target2)

	type probeResult struct {
		anchor fn.Option[ConfirmedAnchor]
		err    error
	}

	results := make(chan probeResult, 2)
	probe := func(r ChainReconciler) {
		ctx, cancel := context.WithTimeout(
			t.Context(), 10*time.Second,
		)
		defer cancel()

		anchor, err := r.ConfirmedTx(ctx, rootTxid)
		results <- probeResult{anchor: anchor, err: err}
	}

	go probe(rec1)
	go probe(rec2)

	for range 2 {
		select {
		case res := <-results:
			require.NoError(
				t, res.err,
				"concurrent reconciler probe failed",
			)
			require.True(
				t, res.anchor.IsSome(),
				"reconciler did not see confirmation; a "+
					"static caller-ID collision would "+
					"silently swallow the second probe",
			)
			require.Equal(
				t, backend.confirmHeight,
				res.anchor.UnsafeFromSome().Height,
			)

		case <-time.After(15 * time.Second):
			t.Fatal(
				"timed out waiting for concurrent " +
					"reconciler probes; static " +
					"caller-ID would cause one probe " +
					"to wait forever",
			)
		}
	}

	// Both reconcilers must have hit the backend with a RegisterConf
	// for the shared txid — confirming no internal short-circuit.
	backend.mu.Lock()
	defer backend.mu.Unlock()
	require.Len(
		t, backend.registers, 2,
		"expected both reconciler probes to reach the backend",
	)
	for _, r := range backend.registers {
		require.Equal(t, rootTxid, r.Txid)
	}
}
