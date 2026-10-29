package waved

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

type testChainFuture = actor.Future[chainsource.ChainSourceResp]

// TestIndexedExpiryReadBoundsSilentConfirmation proves a stale positive height
// hint cannot turn a read into the durable 30-second confirmation wait. The
// valid output survives without a deadline and the one-shot watch is removed.
func TestIndexedExpiryReadBoundsSilentConfirmation(t *testing.T) {
	t.Parallel()

	indexed, _ := expiryfixture.Round(
		t, 10_000, []byte{0x51}, 50, 100, 1,
	)
	chainSource := &blockingExpiryChainSource{
		unregistered: make(chan struct{}, 1),
	}
	start := time.Now()
	verified, err := authenticateIndexedExpiry(
		t.Context(), indexed,
		NewBatchExpiryAuthenticator(chainSource, 1),
	)
	require.NoError(t, err)
	require.Zero(t, verified.batchExpiry)
	require.Positive(t, verified.treeDepth)
	require.Less(t, time.Since(start), 5*time.Second)
	select {
	case <-chainSource.unregistered:
	default:
		t.Fatal("read left a confirmation watch registered")
	}
}

// TestBatchExpiryAuthenticatorTimesOut verifies that a confirmation backend
// which never resolves releases the durable caller for retry and unregisters
// its one-shot watch.
func TestBatchExpiryAuthenticatorTimesOut(t *testing.T) {
	t.Parallel()

	chainSource := &blockingExpiryChainSource{
		unregistered: make(chan struct{}, 1),
		bestHeight:   1_000,
	}
	authenticate := batchExpiryAuthenticatorWithTimeout(
		chainSource, 500, 10*time.Millisecond,
	)

	ancestry, _ := testExpiryAuthenticationAncestry(t)
	ancestry.CommitmentHeight = 0
	_, err := authenticate(t.Context(), []vtxo.Ancestry{ancestry})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	select {
	case <-chainSource.unregistered:
	case <-time.After(time.Second):
		t.Fatal("confirmation watch was not unregistered")
	}
}

// TestBatchExpiryAuthenticatorRegistrationTimeoutUnregisters proves cleanup
// is armed before the registration Ask resolves. The chain-source actor may
// still create the watch after the caller's deadline.
func TestBatchExpiryAuthenticatorRegistrationTimeoutUnregisters(t *testing.T) {
	t.Parallel()

	chainSource := &blockingExpiryChainSource{
		unregistered:        make(chan struct{}, 1),
		registrationBlocked: true,
		bestHeight:          1_000,
	}
	authenticate := batchExpiryAuthenticatorWithTimeout(
		chainSource, 500, 10*time.Millisecond,
	)

	ancestry, _ := testExpiryAuthenticationAncestry(t)
	ancestry.CommitmentHeight = 0
	_, err := authenticate(t.Context(), []vtxo.Ancestry{ancestry})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	select {
	case <-chainSource.unregistered:
	case <-time.After(time.Second):
		t.Fatal("timed-out registration was not unregistered")
	}
}

// TestBatchExpiryAuthenticatorScanFloor pins the shared registration boundary:
// unknown heights use a network floor only after the tip reaches it, while real
// hints pass through unchanged and expiry comes from the confirmed height.
func TestBatchExpiryAuthenticatorScanFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		network    string
		hint       int32
		tip        int32
		wantHint   uint32
		wantTipAsk int
	}{
		{
			name: "mainnet", network: "mainnet", tip: 1_000_000,
			wantHint:   defaultMainnetLegacyProofScanFloor,
			wantTipAsk: 1,
		},
		{
			name: "testnet3", network: "testnet", tip: 5_000_000,
			wantHint: testnet3LegacyProofScanFloor, wantTipAsk: 1,
		},
		{
			name: "testnet4", network: "testnet4", tip: 200_000,
			wantHint: testnet4LegacyProofScanFloor, wantTipAsk: 1,
		},
		{
			name: "signet", network: "signet", tip: 400_000,
			wantHint: signetLegacyProofScanFloor, wantTipAsk: 1,
		},
		{
			name: "tip at floor", network: "mainnet",
			tip:        int32(defaultMainnetLegacyProofScanFloor),
			wantHint:   defaultMainnetLegacyProofScanFloor,
			wantTipAsk: 1,
		},
		{
			name: "tip below floor", network: "mainnet",
			tip:      int32(defaultMainnetLegacyProofScanFloor) - 1,
			wantHint: 1, wantTipAsk: 1,
		},
		{
			name: "zero tip", network: "mainnet",
			wantHint: 1, wantTipAsk: 1,
		},
		{
			name: "negative tip", network: "mainnet", tip: -1,
			wantHint: 1, wantTipAsk: 1,
		},
		{
			name:     "regtest",
			network:  "regtest",
			wantHint: 1,
		},
		{
			name:     "simnet",
			network:  "simnet",
			wantHint: 1,
		},
		{
			name:     "unknown network",
			network:  "unknown",
			wantHint: 1,
		},
		{
			name: "known hint below floor", network: "mainnet",
			hint: 100, wantHint: 100,
		},
		{
			name: "known block one", network: "mainnet",
			hint: 1, wantHint: 1,
		},
		{
			name: "known hint above tip", network: "mainnet",
			hint: 1_000_000, tip: 100, wantHint: 1_000_000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ancestry, tx := testExpiryAuthenticationAncestry(t)
			ancestry.CommitmentHeight = tc.hint
			chainSource := &blockingExpiryChainSource{
				unregistered: make(chan struct{}, 1),
				bestHeight:   tc.tip,
				confirmation: &chainsource.ConfirmationEvent{
					Tx: tx, BlockHeight: 1_000_000,
				},
			}
			cfg := &Config{Network: tc.network}
			authenticate := NewBatchExpiryAuthenticator(
				chainSource, cfg.legacyProofScanFloor(),
			)
			evidence, err := authenticate(
				t.Context(), []vtxo.Ancestry{ancestry},
			)
			require.NoError(t, err)
			require.Equal(t, int32(1_000_144), evidence.BatchExpiry)
			require.Equal(
				t, int32(1_000_000),
				evidence.EarliestCommitmentHeight,
			)
			require.Len(t, chainSource.registered, 1)
			request := chainSource.registered[0]
			require.Equal(t, tc.wantHint, request.HeightHint)
			require.Equal(t, ancestry.CommitmentTxID, *request.Txid)
			require.Equal(t, tx.TxOut[0].PkScript, request.PkScript)
			require.Equal(t, uint32(1), request.TargetConfs)
			require.Equal(
				t, tc.wantTipAsk,
				chainSource.bestHeightRequests,
			)
			require.Len(t, chainSource.unregistered, 1)
		})
	}
}

// TestBatchExpiryAuthenticatorZeroFloor proves an unset deployment floor cannot
// reintroduce a genesis scan or require a tip query on a local network.
func TestBatchExpiryAuthenticatorZeroFloor(t *testing.T) {
	t.Parallel()

	ancestry, tx := testExpiryAuthenticationAncestry(t)
	ancestry.CommitmentHeight = 0
	chainSource := &blockingExpiryChainSource{
		unregistered: make(chan struct{}, 1),
		confirmation: &chainsource.ConfirmationEvent{
			Tx: tx, BlockHeight: 100,
		},
	}
	authenticate := NewBatchExpiryAuthenticator(chainSource, 0)
	evidence, err := authenticate(t.Context(), []vtxo.Ancestry{ancestry})
	require.NoError(t, err)
	require.Equal(t, int32(244), evidence.BatchExpiry)
	require.Equal(t, int32(100), evidence.EarliestCommitmentHeight)
	require.Len(t, chainSource.registered, 1)
	require.Equal(t, uint32(1), chainSource.registered[0].HeightHint)
	require.Zero(t, chainSource.bestHeightRequests)
	require.Len(t, chainSource.unregistered, 1)
}

// TestBatchExpiryAuthenticatorTipFailure keeps failed or stuck tip lookups
// retryable within the existing budget, without creating a confirmation watch.
func TestBatchExpiryAuthenticatorTipFailure(t *testing.T) {
	t.Parallel()

	lookupErr := fmt.Errorf("tip unavailable")
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked=%v", blocked), func(t *testing.T) {
			t.Parallel()

			ancestry, _ := testExpiryAuthenticationAncestry(t)
			ancestry.CommitmentHeight = 0
			chainSource := &blockingExpiryChainSource{
				bestHeightErr:     lookupErr,
				bestHeightBlocked: blocked,
			}
			authenticate := batchExpiryAuthenticatorWithTimeout(
				chainSource, 500, 10*time.Millisecond,
			)
			_, err := authenticate(
				t.Context(), []vtxo.Ancestry{ancestry},
			)
			if blocked {
				require.ErrorIs(
					t, err, context.DeadlineExceeded,
				)
			} else {
				require.ErrorIs(t, err, lookupErr)
			}
			require.Empty(t, chainSource.registered)
		})
	}
}

// TestBatchExpiryAuthenticatorCallerCancellation proves a floored watch still
// observes caller cancellation and cleanup uses a live, independent context.
func TestBatchExpiryAuthenticatorCallerCancellation(t *testing.T) {
	t.Parallel()

	ancestry, _ := testExpiryAuthenticationAncestry(t)
	ancestry.CommitmentHeight = 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	chainSource := &blockingExpiryChainSource{
		unregistered:     make(chan struct{}, 1),
		bestHeight:       1_000,
		cancelOnRegister: cancel,
	}
	authenticate := NewBatchExpiryAuthenticator(chainSource, 500)
	_, err := authenticate(ctx, []vtxo.Ancestry{ancestry})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, chainSource.registered, 1)
	require.Equal(t, uint32(500), chainSource.registered[0].HeightHint)
	require.Len(t, chainSource.unregistered, 1)
}

// TestBatchExpiryAuthenticatorFlooredEvidence rejects a different confirmed
// transaction even when a zero hint was successfully replaced by the floor.
func TestBatchExpiryAuthenticatorFlooredEvidence(t *testing.T) {
	t.Parallel()

	ancestry, tx := testExpiryAuthenticationAncestry(t)
	ancestry.CommitmentHeight = 0
	tx.TxOut[0].Value++
	chainSource := &blockingExpiryChainSource{
		unregistered: make(chan struct{}, 1),
		bestHeight:   1_000,
		confirmation: &chainsource.ConfirmationEvent{
			Tx: tx, BlockHeight: 100,
		},
	}
	authenticate := NewBatchExpiryAuthenticator(chainSource, 500)
	_, err := authenticate(t.Context(), []vtxo.Ancestry{ancestry})
	require.ErrorIs(t, err, vtxo.ErrInvalidBatchExpiryEvidence)
	require.Len(t, chainSource.registered, 1)
	require.Equal(t, uint32(500), chainSource.registered[0].HeightHint)
	require.Len(t, chainSource.unregistered, 1)
}

type blockingExpiryChainSource struct {
	bestHeight          int32
	bestHeightErr       error
	bestHeightBlocked   bool
	bestHeightRequests  int
	registered          []*chainsource.RegisterConfRequest
	confirmation        *chainsource.ConfirmationEvent
	cancelOnRegister    context.CancelFunc
	unregistered        chan struct{}
	registrationBlocked bool
}

// ID supplies a stable identity for the confirmation actor stub.
func (b *blockingExpiryChainSource) ID() string {
	return "blocking-expiry-chain-source"
}

// Tell records watch cleanup only when its independent context is still live.
func (b *blockingExpiryChainSource) Tell(ctx context.Context,
	msg chainsource.ChainSourceMsg) error {

	if _, ok := msg.(*chainsource.UnregisterConfRequest); ok {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case b.unregistered <- struct{}{}:
		default:
		}
	}

	return nil
}

// TryTell delegates nonblocking test delivery to Tell.
func (b *blockingExpiryChainSource) TryTell(ctx context.Context,
	msg chainsource.ChainSourceMsg) error {

	return b.Tell(ctx, msg)
}

// Ask scripts the tip query and confirmation registration, including blocked
// responses and cancellation at the registration boundary.
func (b *blockingExpiryChainSource) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg) testChainFuture {

	response := actor.NewPromise[chainsource.ChainSourceResp]()
	if _, ok := msg.(*chainsource.BestHeightRequest); ok {
		b.bestHeightRequests++
		if b.bestHeightBlocked {
			return response.Future()
		}
		if b.bestHeightErr != nil {
			response.Complete(
				fn.Err[chainsource.ChainSourceResp](
					b.bestHeightErr,
				),
			)
		} else {
			response.Complete(
				fn.Ok[chainsource.ChainSourceResp](
					&chainsource.BestHeightResponse{
						Height: b.bestHeight,
					},
				),
			)
		}

		return response.Future()
	}

	request, ok := msg.(*chainsource.RegisterConfRequest)
	if !ok {
		response.Complete(
			fn.Err[chainsource.ChainSourceResp](
				fmt.Errorf("unexpected request %T", msg),
			),
		)

		return response.Future()
	}
	b.registered = append(b.registered, request)
	if b.cancelOnRegister != nil {
		b.cancelOnRegister()
	}
	if b.registrationBlocked {
		return response.Future()
	}

	confirmation := actor.NewPromise[chainsource.ConfirmationEvent]()
	if b.confirmation != nil {
		confirmation.Complete(fn.Ok(*b.confirmation))
	}
	response.Complete(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RegisterConfResponse{
				Future: confirmation.Future(),
			},
		),
	)

	return response.Future()
}

// testExpiryAuthenticationAncestry returns sweep evidence and its matching
// commitment transaction to test chain authentication and watch cleanup.
func testExpiryAuthenticationAncestry(t *testing.T) (vtxo.Ancestry,
	*wire.MsgTx) {

	t.Helper()

	const delay = uint32(144)
	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey.PubKey(), delay,
	)
	require.NoError(t, err)
	sweepRoot := sweepLeaf.TapHash()
	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	cosigners := []*btcec.PublicKey{ownerKey.PubKey()}
	finalKey, err := tree.ComputeFinalKey(cosigners, sweepRoot[:])
	require.NoError(t, err)
	batchScript, err := txscript.PayToTaprootScript(finalKey)
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{1}},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    10_000,
		PkScript: batchScript,
	})
	txid := tx.TxHash()

	return vtxo.Ancestry{
		TreePath: &tree.Tree{
			Root: &tree.Node{
				CoSigners: cosigners,
			},
			BatchOutpoint: wire.OutPoint{
				Hash: txid,
			},
			BatchOutput:        tx.TxOut[0],
			SweepTapscriptRoot: sweepRoot[:],
		},
		CommitmentTxID:       txid,
		CommitmentHeight:     100,
		CommitmentSweepDelay: delay,
		CommitmentSweepKey:   sweepKey.PubKey(),
	}, tx
}
