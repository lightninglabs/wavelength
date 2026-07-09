package darepod

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestExitPolicyKindMirrorsRecoveryConstants pins the string-typed actormsg
// exit-policy enum to the canonical vhtlcrecovery constants. The two are kept
// in sync by hand (actormsg cannot import vhtlcrecovery without a cycle), so a
// drift would silently break the round-trip through the ForceUnroll path.
func TestExitPolicyKindMirrorsRecoveryConstants(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, vhtlcrecovery.ExitPolicyKindClaim,
		string(actormsg.ExitPolicyVHTLCClaim),
	)
	require.Equal(
		t, vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver,
		string(actormsg.ExitPolicyVHTLCRefundWithoutReceiver),
	)
}

// TestUnrollStartTrigger verifies the string-typed UnrollTrigger that rides the
// ForceUnroll path maps back onto the right unroll.StartTrigger, and that an
// empty or unknown trigger falls back to critical expiry (preserving the
// auto-expiry default and the historical manual-exit behavior).
func TestUnrollStartTrigger(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, unroll.TriggerCriticalExpiry,
		unrollStartTrigger(actormsg.UnrollTriggerCriticalExpiry),
	)
	require.Equal(
		t, unroll.TriggerManual,
		unrollStartTrigger(actormsg.UnrollTriggerManual),
	)
	require.Equal(
		t, unroll.TriggerFraudSpend,
		unrollStartTrigger(actormsg.UnrollTriggerFraudSpend),
	)
	require.Equal(
		t, unroll.TriggerCriticalExpiry,
		unrollStartTrigger(
			actormsg.UnrollTrigger("not-a-real-trigger"),
		),
	)
}

// TestEnsureUnrollFromExpiring verifies the chain-resolver bridge threads the
// trigger and optional exit policy from a VTXO ExpiringNotification into the
// registry EnsureUnrollRequest, and that a None policy keeps the registry on
// its standard timeout policy.
func TestEnsureUnrollFromExpiring(t *testing.T) {
	t.Parallel()

	outpoint := wire.OutPoint{Hash: chainhash.Hash{0xaa}, Index: 3}

	// A vHTLC refund carries an explicit trigger and exit policy: both must
	// survive into the registry request.
	withPolicy := ensureUnrollFromExpiring(vtxo.ExpiringNotification{
		VTXO:    &vtxo.Descriptor{Outpoint: outpoint},
		Trigger: actormsg.UnrollTriggerManual,
		ExitPolicy: fn.Some(actormsg.ExitPolicy{
			Kind: actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
			Ref:  actormsg.ExitPolicyRef("recovery-42"),
		}),
	})
	require.Equal(t, outpoint, withPolicy.Outpoint)
	require.Equal(t, unroll.TriggerManual, withPolicy.Trigger)
	require.Equal(
		t, unroll.ExitPolicyKind(
			actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
		),
		withPolicy.ExitPolicyKind,
	)
	require.Equal(t, "recovery-42", withPolicy.ExitPolicyRef)

	// A critical-expiry notification carries no policy: the registry
	// request must leave the policy identity empty (standard timeout).
	autoExpiry := ensureUnrollFromExpiring(vtxo.ExpiringNotification{
		VTXO: &vtxo.Descriptor{Outpoint: outpoint},
	})
	require.Equal(t, unroll.TriggerCriticalExpiry, autoExpiry.Trigger)
	require.Empty(t, autoExpiry.ExitPolicyKind)
	require.Empty(t, autoExpiry.ExitPolicyRef)
}
