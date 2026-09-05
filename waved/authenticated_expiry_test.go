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
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

type testChainFuture = actor.Future[chainsource.ChainSourceResp]

// TestBatchExpiryAuthenticatorTimesOut verifies that a confirmation backend
// which never resolves releases the durable caller for retry and unregisters
// its one-shot watch.
func TestBatchExpiryAuthenticatorTimesOut(t *testing.T) {
	t.Parallel()

	chainSource := &blockingExpiryChainSource{
		unregistered: make(chan struct{}, 1),
	}
	authenticate := batchExpiryAuthenticatorWithTimeout(
		chainSource, 10*time.Millisecond,
	)

	_, err := authenticate(t.Context(), []vtxo.Ancestry{
		testExpiryAuthenticationAncestry(t),
	})
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
	}
	authenticate := batchExpiryAuthenticatorWithTimeout(
		chainSource, 10*time.Millisecond,
	)

	_, err := authenticate(t.Context(), []vtxo.Ancestry{
		testExpiryAuthenticationAncestry(t),
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	select {
	case <-chainSource.unregistered:
	case <-time.After(time.Second):
		t.Fatal("timed-out registration was not unregistered")
	}
}

type blockingExpiryChainSource struct {
	unregistered        chan struct{}
	registrationBlocked bool
}

func (b *blockingExpiryChainSource) ID() string {
	return "blocking-expiry-chain-source"
}

func (b *blockingExpiryChainSource) Tell(_ context.Context,
	msg chainsource.ChainSourceMsg) error {

	if _, ok := msg.(*chainsource.UnregisterConfRequest); ok {
		select {
		case b.unregistered <- struct{}{}:
		default:
		}
	}

	return nil
}

func (b *blockingExpiryChainSource) TryTell(ctx context.Context,
	msg chainsource.ChainSourceMsg) error {

	return b.Tell(ctx, msg)
}

func (b *blockingExpiryChainSource) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg) testChainFuture {

	response := actor.NewPromise[chainsource.ChainSourceResp]()
	if _, ok := msg.(*chainsource.RegisterConfRequest); !ok {
		response.Complete(
			fn.Err[chainsource.ChainSourceResp](
				fmt.Errorf("unexpected request %T", msg),
			),
		)

		return response.Future()
	}
	if b.registrationBlocked {
		return response.Future()
	}

	confirmation := actor.NewPromise[chainsource.ConfirmationEvent]()
	response.Complete(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.RegisterConfResponse{
				Future: confirmation.Future(),
			},
		),
	)

	return response.Future()
}

func testExpiryAuthenticationAncestry(t *testing.T) vtxo.Ancestry {
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
	}
}
