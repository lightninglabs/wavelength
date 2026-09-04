package vtxo

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// TestAuthenticateBatchExpiryUsesEarliestCommitment verifies multi-parent
// expiry selection and per-commitment lookup deduplication.
func TestAuthenticateBatchExpiryUsesEarliestCommitment(t *testing.T) {
	t.Parallel()

	first, firstConfirmation := testExpiryAncestry(t, 50, 100)
	second, secondConfirmation := testExpiryAncestry(t, 20, 120)
	duplicate := first
	duplicate.InputIndices = []uint32{1}

	confirmations := map[chainhash.Hash]CommitmentConfirmation{
		first.CommitmentTxID:  firstConfirmation,
		second.CommitmentTxID: secondConfirmation,
	}
	resolveCalls := make(map[chainhash.Hash]int)
	expiry, err := AuthenticateBatchExpiry(
		t.Context(), []Ancestry{first, duplicate, second},
		func(_ context.Context, txid chainhash.Hash, _ []byte,
			_ uint32) (CommitmentConfirmation, error) {

			resolveCalls[txid]++

			return confirmations[txid], nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, int32(140), expiry)
	require.Equal(t, 1, resolveCalls[first.CommitmentTxID])
	require.Equal(t, 1, resolveCalls[second.CommitmentTxID])
}

// TestAuthenticateBatchExpiryRejectsInvalidEvidence verifies every untrusted
// proof boundary and transient resolver classification.
func TestAuthenticateBatchExpiryRejectsInvalidEvidence(t *testing.T) {
	t.Parallel()

	fragment, confirmation := testExpiryAncestry(t, 50, 100)

	t.Run("output mismatch", func(t *testing.T) {
		bad := confirmation
		bad.Tx = bad.Tx.Copy()
		bad.Tx.TxOut[0].Value++

		_, err := AuthenticateBatchExpiry(
			t.Context(), []Ancestry{fragment},
			func(context.Context, chainhash.Hash, []byte, uint32) (
				CommitmentConfirmation, error) {

				return bad, nil
			},
		)
		require.ErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
		require.ErrorContains(t, err, "does not match commitment")
	})

	t.Run("sweep mismatch", func(t *testing.T) {
		bad := fragment
		bad.CommitmentSweepDelay++

		_, err := AuthenticateBatchExpiry(
			t.Context(), []Ancestry{bad},
			func(context.Context, chainhash.Hash, []byte, uint32) (
				CommitmentConfirmation, error) {

				return confirmation, nil
			},
		)
		require.ErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
		require.ErrorContains(t, err, "committed sweep root")
	})

	t.Run("tree root substitution", func(t *testing.T) {
		bad := fragment
		otherKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		bad.TreePath = &tree.Tree{
			Root: &tree.Node{
				CoSigners: []*btcec.PublicKey{
					otherKey.PubKey(),
				},
			},
			BatchOutpoint: fragment.TreePath.BatchOutpoint,
			BatchOutput:   fragment.TreePath.BatchOutput,
			SweepTapscriptRoot: fragment.TreePath.
				SweepTapscriptRoot,
		}

		_, err = AuthenticateBatchExpiry(
			t.Context(), []Ancestry{bad},
			func(context.Context, chainhash.Hash, []byte, uint32) (
				CommitmentConfirmation, error) {

				return confirmation, nil
			},
		)
		require.ErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
		require.ErrorContains(
			t, err, "batch output does not match tree root",
		)
	})

	t.Run("overflow", func(t *testing.T) {
		badConfirmation := confirmation
		badConfirmation.BlockHeight = math.MaxInt32 - 10

		_, err := AuthenticateBatchExpiry(
			t.Context(), []Ancestry{fragment},
			func(context.Context, chainhash.Hash, []byte, uint32) (
				CommitmentConfirmation, error) {

				return badConfirmation, nil
			},
		)
		require.ErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
		require.ErrorContains(t, err, "overflows")
	})

	t.Run("resolver error remains retryable", func(t *testing.T) {
		resolverErr := errors.New("chain backend unavailable")
		_, err := AuthenticateBatchExpiry(
			t.Context(), []Ancestry{fragment},
			func(context.Context, chainhash.Hash, []byte, uint32) (
				CommitmentConfirmation, error) {

				return CommitmentConfirmation{}, resolverErr
			},
		)
		require.ErrorIs(t, err, resolverErr)
		require.NotErrorIs(t, err, ErrInvalidBatchExpiryEvidence)
	})

	t.Run("incomplete confirmation remains retryable", func(t *testing.T) {
		tests := []CommitmentConfirmation{
			{
				BlockHeight: confirmation.BlockHeight,
			},
			{
				Tx: confirmation.Tx,
			},
		}
		for _, incomplete := range tests {
			_, err := AuthenticateBatchExpiry(
				t.Context(), []Ancestry{fragment},
				func(context.Context, chainhash.Hash, []byte,
					uint32) (CommitmentConfirmation,
					error) {

					return incomplete, nil
				},
			)
			require.Error(t, err)
			require.NotErrorIs(
				t, err, ErrInvalidBatchExpiryEvidence,
			)
		}
	})
}

// testExpiryAncestry builds internally consistent tree and chain evidence.
func testExpiryAncestry(t *testing.T, delay uint32,
	height int32) (Ancestry, CommitmentConfirmation) {

	t.Helper()

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
	tx.AddTxIn(
		&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{byte(delay)},
			},
		},
	)
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(delay) * 100,
		PkScript: batchScript,
	})
	txid := tx.TxHash()

	return Ancestry{
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
			CommitmentHeight:     height,
			CommitmentSweepDelay: delay,
			CommitmentSweepKey:   sweepKey.PubKey(),
		}, CommitmentConfirmation{
			Tx:          tx,
			BlockHeight: height,
		}
}
