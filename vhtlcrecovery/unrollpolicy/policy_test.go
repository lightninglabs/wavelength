package unrollpolicy

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestClaimExitSpendPolicyBuildsPreimageSpend verifies claim recovery builds a
// transaction that uses the receiver key, waits the claim CSV delay, and places
// the swap preimage into the script-path witness.
func TestClaimExitSpendPolicyBuildsPreimageSpend(t *testing.T) {
	t.Parallel()

	job, preimage, _, receiverSigner := testPolicyJob(
		t, vhtlcrecovery.ActionClaim,
	)

	policy, err := NewClaimExitSpendPolicy(job, preimage)
	require.NoError(t, err)
	require.Equal(
		t, unroll.ExitPolicyKind(vhtlcrecovery.ExitPolicyKindClaim),
		policy.Kind(),
	)
	require.Equal(t, uint32(job.UnilateralClaimDelay), policy.CSVDelay())

	targetOutput := testPolicyTargetOutput(t, job)
	spendTx, err := policy.BuildSpendTx(
		t.Context(), unroll.ExitSpendRequest{
			TargetOutpoint:      job.VTXOOutpoint,
			TargetOutput:        targetOutput,
			DestinationPkScript: []byte{txscript.OP_TRUE},
			FeeRateSatPerVByte:  2,
			Signer:              receiverSigner,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t,
		blockchain.LockTimeToSequence(
			false, uint32(job.UnilateralClaimDelay),
		),
		spendTx.TxIn[0].Sequence,
	)
	require.Equal(t, uint32(0), spendTx.LockTime)
	require.Len(t, spendTx.TxIn[0].Witness, 4)
	require.True(t, bytes.Equal(preimage[:], spendTx.TxIn[0].Witness[1]))
}

// TestClaimExitSpendPolicyRejectsWrongPreimage verifies the constructor fails
// closed if the swap store returns a preimage that does not match the recovery
// row's durable preimage hash.
func TestClaimExitSpendPolicyRejectsWrongPreimage(t *testing.T) {
	t.Parallel()

	job, _, _, _ := testPolicyJob(t, vhtlcrecovery.ActionClaim)
	wrongPreimage, err := lntypes.MakePreimage(
		bytes.Repeat(
			[]byte{0x99}, 32,
		),
	)
	require.NoError(t, err)

	_, err = NewClaimExitSpendPolicy(job, wrongPreimage)
	require.ErrorContains(t, err, "preimage does not match")
}

// TestRefundWithoutReceiverExitSpendPolicyBuildsCSVCLTVSpend verifies sender
// refund-without-receiver recovery builds a transaction with both Ark CSV
// sequence and vHTLC refund CLTV locktime requirements.
func TestRefundWithoutReceiverExitSpendPolicyBuildsCSVCLTVSpend(t *testing.T) {
	t.Parallel()

	job, _, senderSigner, _ := testPolicyJob(
		t, vhtlcrecovery.ActionRefundWithoutReceiver,
	)

	policy, err := NewRefundWithoutReceiverExitSpendPolicy(job)
	require.NoError(t, err)
	require.Equal(
		t, unroll.ExitPolicyKind(
			vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver,
		),
		policy.Kind(),
	)
	require.Equal(
		t, uint32(job.UnilateralRefundWithoutReceiverDelay),
		policy.CSVDelay(),
	)

	targetOutput := testPolicyTargetOutput(t, job)
	spendTx, err := policy.BuildSpendTx(
		t.Context(), unroll.ExitSpendRequest{
			TargetOutpoint:      job.VTXOOutpoint,
			TargetOutput:        targetOutput,
			DestinationPkScript: []byte{txscript.OP_TRUE},
			FeeRateSatPerVByte:  2,
			CurrentHeight:       job.RefundLocktime,
			Signer:              senderSigner,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t,
		blockchain.LockTimeToSequence(
			false, uint32(
				job.UnilateralRefundWithoutReceiverDelay,
			),
		),
		spendTx.TxIn[0].Sequence,
	)
	require.Equal(t, uint32(job.RefundLocktime), spendTx.LockTime)
	require.Len(t, spendTx.TxIn[0].Witness, 3)
}

// TestRefundWithoutReceiverExitSpendPolicyStallsBeforeLocktime verifies the
// CLTV maturity guard: BuildSpendTx must refuse to construct a refund tx whose
// nLockTime exceeds the caller-supplied chain height, surfacing the typed
// ErrExitSpendNotMatured sentinel so the actor can stall instead of burning
// retries on a tx the mempool would reject as non-final.
func TestRefundWithoutReceiverExitSpendPolicyStallsBeforeLocktime(
	t *testing.T) {

	t.Parallel()

	job, _, senderSigner, _ := testPolicyJob(
		t, vhtlcrecovery.ActionRefundWithoutReceiver,
	)

	policy, err := NewRefundWithoutReceiverExitSpendPolicy(job)
	require.NoError(t, err)

	_, err = policy.BuildSpendTx(
		t.Context(), unroll.ExitSpendRequest{
			TargetOutpoint:      job.VTXOOutpoint,
			TargetOutput:        testPolicyTargetOutput(t, job),
			DestinationPkScript: []byte{txscript.OP_TRUE},
			FeeRateSatPerVByte:  2,
			CurrentHeight:       job.RefundLocktime - 1,
			Signer:              senderSigner,
		},
	)
	require.ErrorIs(t, err, unroll.ErrExitSpendNotMatured)
}

// TestRefundWithoutReceiverExitSpendPolicyRequiredLockTime verifies the
// policy advertises the refund locktime so the unroll FSM can gate broadcast.
func TestRefundWithoutReceiverExitSpendPolicyRequiredLockTime(t *testing.T) {
	t.Parallel()

	job, _, _, _ := testPolicyJob(
		t, vhtlcrecovery.ActionRefundWithoutReceiver,
	)

	policy, err := NewRefundWithoutReceiverExitSpendPolicy(job)
	require.NoError(t, err)

	require.Equal(
		t, uint32(job.RefundLocktime), policy.RequiredLockTime(),
	)
}

// TestClaimExitSpendPolicyRequiredLockTimeZero verifies the claim path
// reports RequiredLockTime=0: the unilateral claim leaf is gated only by CSV
// plus preimage knowledge, not by absolute locktime.
func TestClaimExitSpendPolicyRequiredLockTimeZero(t *testing.T) {
	t.Parallel()

	job, preimage, _, _ := testPolicyJob(
		t, vhtlcrecovery.ActionClaim,
	)

	policy, err := NewClaimExitSpendPolicy(job, preimage)
	require.NoError(t, err)

	require.Zero(t, policy.RequiredLockTime())
}

// TestVHTLCExitSpendPolicyRejectsWrongTarget verifies the policy refuses to
// spend a materialized output whose pkScript does not match the vHTLC recovered
// from durable recovery columns.
func TestVHTLCExitSpendPolicyRejectsWrongTarget(t *testing.T) {
	t.Parallel()

	job, _, _, _ := testPolicyJob(
		t, vhtlcrecovery.ActionRefundWithoutReceiver,
	)
	policy, err := NewRefundWithoutReceiverExitSpendPolicy(job)
	require.NoError(t, err)

	err = policy.ValidateTarget(&wire.TxOut{
		Value:    job.VTXOAmountSat,
		PkScript: []byte{txscript.OP_TRUE},
	})
	require.ErrorContains(t, err, "pkscript does not match")
}

// TestVHTLCExitSpendPolicyRejectsFeeAboveCap verifies the policy enforces the
// recovery row's max-fee-rate guard before constructing a signed spend.
func TestVHTLCExitSpendPolicyRejectsFeeAboveCap(t *testing.T) {
	t.Parallel()

	job, _, senderSigner, _ := testPolicyJob(
		t, vhtlcrecovery.ActionRefundWithoutReceiver,
	)
	policy, err := NewRefundWithoutReceiverExitSpendPolicy(job)
	require.NoError(t, err)

	_, err = policy.BuildSpendTx(
		t.Context(), unroll.ExitSpendRequest{
			TargetOutpoint:      job.VTXOOutpoint,
			TargetOutput:        testPolicyTargetOutput(t, job),
			DestinationPkScript: []byte{txscript.OP_TRUE},
			FeeRateSatPerVByte:  11,
			Signer:              senderSigner,
		},
	)
	require.ErrorContains(t, err, "exceeds cap")
}

// TestExitSpendPolicyResolver verifies the resolver reconstructs both vHTLC
// policy families from durable `(kind, ref)` identity and rejects mismatched
// request kinds.
func TestExitSpendPolicyResolver(t *testing.T) {
	t.Parallel()

	job, preimage, _, _ := testPolicyJob(t, vhtlcrecovery.ActionClaim)
	resolver := ExitSpendPolicyResolver{
		Jobs: fakeRecoveryJobLoader{
			job.ID: job,
		},
		Preimage: fakePreimageResolver{
			preimage: preimage,
		},
	}

	policy, err := resolver.ResolveExitSpendPolicy(
		t.Context(), unroll.ExitSpendPolicyRequest{
			Kind: vhtlcrecovery.ExitPolicyKindClaim,
			Ref:  job.ID,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, unroll.ExitPolicyKind(vhtlcrecovery.ExitPolicyKindClaim),
		policy.Kind(),
	)

	refundJob, _, _, _ := testPolicyJob(
		t, vhtlcrecovery.ActionRefundWithoutReceiver,
	)
	refundResolver := ExitSpendPolicyResolver{
		Jobs: fakeRecoveryJobLoader{
			refundJob.ID: refundJob,
		},
	}
	refundPolicy, err := refundResolver.ResolveExitSpendPolicy(
		t.Context(), unroll.ExitSpendPolicyRequest{
			Kind: vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver,
			Ref:  refundJob.ID,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, unroll.ExitPolicyKind(
			vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver,
		),
		refundPolicy.Kind(),
	)

	_, err = resolver.ResolveExitSpendPolicy(
		t.Context(), unroll.ExitSpendPolicyRequest{
			Kind: vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver,
			Ref:  job.ID,
		},
	)
	require.ErrorContains(t, err, "does not match request kind")
}

// TestExitSpendPolicyResolverUsesDurableClaimPreimage verifies cross-process
// recovery can build a claim policy from the preimage stored on the recovery
// row without requiring an in-process swap preimage resolver.
func TestExitSpendPolicyResolverUsesDurableClaimPreimage(t *testing.T) {
	t.Parallel()

	job, preimage, _, _ := testPolicyJob(t, vhtlcrecovery.ActionClaim)
	job.ClaimPreimage = preimage[:]
	resolver := ExitSpendPolicyResolver{
		Jobs: fakeRecoveryJobLoader{
			job.ID: job,
		},
	}

	policy, err := resolver.ResolveExitSpendPolicy(
		t.Context(), unroll.ExitSpendPolicyRequest{
			Kind: vhtlcrecovery.ExitPolicyKindClaim,
			Ref:  job.ID,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, unroll.ExitPolicyKind(vhtlcrecovery.ExitPolicyKindClaim),
		policy.Kind(),
	)
}

// testPolicyJob returns a fully populated recovery job and matching test
// signers for the requested recovery action. The row mirrors the named SQL
// columns the production adapter reconstructs.
func testPolicyJob(t *testing.T, action string) (vhtlcrecovery.RecoveryJob,
	lntypes.Preimage, input.Signer, input.Signer) {

	t.Helper()

	sender, senderSigner := testutils.CreateKey(31)
	receiver, receiverSigner := testutils.CreateKey(32)
	server, _ := testutils.CreateKey(33)

	preimage, err := lntypes.MakePreimage(bytes.Repeat([]byte{0x42}, 32))
	require.NoError(t, err)
	preimageHash := preimage.Hash()
	senderKey := sender.SerializeCompressed()
	receiverKey := receiver.SerializeCompressed()
	serverKey := server.SerializeCompressed()

	policyKind, err := vhtlcrecovery.ExitPolicyKindForAction(action)
	require.NoError(t, err)

	return vhtlcrecovery.RecoveryJob{
		ID:        "recovery-policy",
		RequestID: "request-policy",
		SwapID:    []byte("swap-policy"),
		Direction: vhtlcrecovery.DirectionReceive,
		Action:    action,
		State:     vhtlcrecovery.StateArmed,
		VTXOOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x44,
			},
			Index: 2,
		},
		VTXOAmountSat:                        500_000,
		SenderPubkey:                         senderKey,
		ReceiverPubkey:                       receiverKey,
		ServerPubkey:                         serverKey,
		RefundLocktime:                       500_000,
		UnilateralClaimDelay:                 12,
		UnilateralRefundDelay:                24,
		UnilateralRefundWithoutReceiverDelay: 36,
		PreimageHash:                         preimageHash[:],
		SignerKeyFamily:                      6,
		SignerKeyIndex:                       9,
		DestinationScript: []byte{
			txscript.OP_TRUE,
		},
		MaxFeeRateSatPerKWeight: 2_500,
		ExitPolicyKind:          policyKind,
	}, preimage, senderSigner, receiverSigner
}

// testPolicyTargetOutput reconstructs the vHTLC policy for a recovery job and
// returns the on-chain output that ValidateTarget should accept.
func testPolicyTargetOutput(t *testing.T,
	job vhtlcrecovery.RecoveryJob) *wire.TxOut {

	t.Helper()

	policy, err := policyFromJob(job)
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	return &wire.TxOut{
		Value:    job.VTXOAmountSat,
		PkScript: pkScript,
	}
}

type fakeRecoveryJobLoader map[string]vhtlcrecovery.RecoveryJob

// GetRecovery implements RecoveryJobLoader for resolver tests.
func (f fakeRecoveryJobLoader) GetRecovery(_ context.Context, id string) (
	*vhtlcrecovery.RecoveryJob, error) {

	job, ok := f[id]
	if !ok {
		return nil, errTestNotFound
	}

	return &job, nil
}

type fakePreimageResolver struct {
	preimage lntypes.Preimage
}

// ResolvePreimage implements PreimageResolver and enforces the same
// hash-matching contract as the production swap store adapter should.
func (f fakePreimageResolver) ResolvePreimage(_ context.Context, _ []byte,
	preimageHash lntypes.Hash) (lntypes.Preimage, error) {

	if !f.preimage.Matches(preimageHash) {
		return lntypes.Preimage{}, errTestNotFound
	}

	return f.preimage, nil
}

var errTestNotFound = &testError{"not found"}

type testError struct {
	msg string
}

// Error returns the stable test error string.
func (e *testError) Error() string {
	return e.msg
}
