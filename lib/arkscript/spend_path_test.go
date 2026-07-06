package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

// makeSpendPathFromPolicy is a small helper that builds a SpendPath for the
// collab leaf of a VTXO policy and returns the expected pkScript for that
// output. It centralises the setup that every binding test needs: a real
// compiled policy, a real NUMS-tweaked P2TR script, and a control block
// whose inclusion proof actually commits to the leaf.
func makeSpendPathFromPolicy(t *testing.T,
	policy *VTXOPolicy) (*SpendPath, []byte) {

	t.Helper()

	info, err := policy.CollabSpendInfo()
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(policy.OutputKey())
	require.NoError(t, err)

	return &SpendPath{
		SpendInfo:        info,
		RequiredSequence: 0xffffffff,
	}, pkScript
}

// TestVerifyBindsToPkScript exercises the binding check that prevents a
// caller from producing a control block for one taproot output and having
// the operator sign as if it had authorised spending a different output.
// Each subtest covers one of the rejection paths in VerifyBindsToPkScript
// plus the happy path.
func TestVerifyBindsToPkScript(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(101)
	operatorKey, _ := testutils.CreateKey(102)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 144)
	require.NoError(t, err)

	// A happy-path binding against the policy's own pkScript is the
	// baseline: a control block whose inclusion proof commits to the
	// witness script under the NUMS-tweaked output key must verify
	// against the P2TR script derived from that output key.
	t.Run("binds to real policy pkScript", func(t *testing.T) {
		t.Parallel()

		spendPath, pkScript := makeSpendPathFromPolicy(t, policy)

		require.NoError(t, spendPath.VerifyBindsToPkScript(pkScript))
	})

	// Calling VerifyBindsToPkScript on a nil SpendPath must surface the
	// "spend path must be provided" error from Validate rather than
	// panicking or silently accepting.
	t.Run("nil spend path rejected", func(t *testing.T) {
		t.Parallel()

		var s *SpendPath
		err := s.VerifyBindsToPkScript([]byte{0x51})
		require.ErrorContains(t, err, "spend path must be provided")
	})

	// A SpendPath with a nil embedded SpendInfo must fail the
	// "spend info must be provided" precondition — without the witness
	// script and control block there is nothing to bind.
	t.Run("nil spend info rejected", func(t *testing.T) {
		t.Parallel()

		s := &SpendPath{RequiredSequence: 0xffffffff}
		err := s.VerifyBindsToPkScript([]byte{0x51})
		require.ErrorContains(t, err, "spend info must be provided")
	})

	// A SpendPath whose WitnessScript is empty must fail the
	// "witness script must be provided" precondition.
	t.Run("empty witness script rejected", func(t *testing.T) {
		t.Parallel()

		spendPath, pkScript := makeSpendPathFromPolicy(t, policy)
		spendPath.WitnessScript = nil

		err := spendPath.VerifyBindsToPkScript(pkScript)
		require.ErrorContains(t, err, "witness script must be provided")
	})

	// A SpendPath whose ControlBlock is empty must fail the
	// "control block must be provided" precondition.
	t.Run("empty control block rejected", func(t *testing.T) {
		t.Parallel()

		spendPath, pkScript := makeSpendPathFromPolicy(t, policy)
		spendPath.ControlBlock = nil

		err := spendPath.VerifyBindsToPkScript(pkScript)
		require.ErrorContains(t, err, "control block must be provided")
	})

	// An empty pkScript must be refused — the binding check has nothing
	// to compare against, and silently accepting would subvert the point
	// of the check.
	t.Run("empty pk script rejected", func(t *testing.T) {
		t.Parallel()

		spendPath, _ := makeSpendPathFromPolicy(t, policy)

		err := spendPath.VerifyBindsToPkScript(nil)
		require.ErrorContains(t, err, "pk script must be provided")
	})

	// A malformed control block (fewer bytes than a valid taproot control
	// block) must fail at the parse step.
	t.Run("malformed control block rejected", func(t *testing.T) {
		t.Parallel()

		spendPath, pkScript := makeSpendPathFromPolicy(t, policy)
		spendPath.ControlBlock = []byte{0xc0, 0x01, 0x02}

		err := spendPath.VerifyBindsToPkScript(pkScript)
		require.ErrorContains(t, err, "parse control block")
	})

	// Every Ark taproot output commits to the NUMS internal key so that
	// no key-path spend is possible. A control block whose internal key
	// is not NUMS must be rejected with a dedicated error before any
	// downstream tweak math runs, even if its inclusion proof would
	// otherwise produce the expected output key.
	t.Run("non-NUMS internal key rejected", func(t *testing.T) {
		t.Parallel()

		spendPath, pkScript := makeSpendPathFromPolicy(t, policy)

		// Swap the first 32 bytes of the control block (the internal
		// key) with an arbitrary public key. The rest of the proof is
		// kept intact; the rejection must come from the NUMS check.
		attackerKey, _ := testutils.CreateKey(103)
		attackerX := schnorr.SerializePubKey(attackerKey)

		mutated := append([]byte(nil), spendPath.ControlBlock...)
		copy(mutated[1:33], attackerX)
		spendPath.ControlBlock = mutated

		err := spendPath.VerifyBindsToPkScript(pkScript)
		require.ErrorContains(t, err, "not the Ark NUMS point")
	})

	// A control block that would legitimately bind to some pkScript
	// (because it was produced for a real Ark output) must still be
	// rejected when the caller presents a pkScript from a DIFFERENT
	// policy. This is the core signature-oracle defence: the operator
	// must not be tricked into signing for an output whose leaf set
	// we haven't proven ownership of.
	t.Run("wrong pk script rejected", func(t *testing.T) {
		t.Parallel()

		spendPath, _ := makeSpendPathFromPolicy(t, policy)

		// Derive a pkScript from an unrelated policy (different owner
		// and exit delay). It is a well-formed Ark P2TR script but is
		// not the one our control block commits to.
		otherOwner, _ := testutils.CreateKey(201)
		otherPolicy, err := NewVTXOPolicy(
			otherOwner, operatorKey, 200,
		)
		require.NoError(t, err)

		otherPkScript, err := txscript.PayToTaprootScript(
			otherPolicy.OutputKey(),
		)
		require.NoError(t, err)

		err = spendPath.VerifyBindsToPkScript(otherPkScript)
		require.ErrorContains(
			t, err, "does not commit to declared pkScript",
		)
	})

	// The pkScript bytes may be a syntactically valid 34-byte P2TR
	// script whose output key is completely unrelated to anything we
	// control. The binding check must still reject.
	t.Run("arbitrary P2TR pk script rejected", func(t *testing.T) {
		t.Parallel()

		spendPath, _ := makeSpendPathFromPolicy(t, policy)

		// Generate an unrelated public key and build a P2TR script
		// directly from it; no taproot tweak math was applied, so
		// the operator cannot possibly have authorised it.
		unrelated, _ := btcec.NewPrivateKey()
		bogus, err := txscript.PayToTaprootScript(unrelated.PubKey())
		require.NoError(t, err)

		err = spendPath.VerifyBindsToPkScript(bogus)
		require.ErrorContains(
			t, err, "does not commit to declared pkScript",
		)
	})
}
