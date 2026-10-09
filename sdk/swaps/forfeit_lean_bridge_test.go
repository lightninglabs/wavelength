package swaps

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestForfeitSigningLeanBridge replays Lean admission decisions through the
// production publication and exact-binding gates.
func TestForfeitSigningLeanBridge(t *testing.T) {
	t.Parallel()

	vectorsPath := filepath.Join(
		"..", "..", "formal", "lean", "forfeitsigning", "vectors.tsv",
	)
	vectors, err := os.Open(vectorsPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, vectors.Close())
	})

	scanner := bufio.NewScanner(vectors)
	caseCount := 0
	for scanner.Scan() {
		caseCount++
		fields := strings.Split(scanner.Text(), "\t")
		require.Len(t, fields, 2)

		name := fields[0]
		expected := fields[1]
		switch expected {
		case "accept", "reject":
		default:
			t.Fatalf("unknown Lean bridge decision %q", expected)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			accepted := replayLeanForfeitSigningCase(t, name)
			require.Equal(t, expected == "accept", accepted)
		})
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, 8, caseCount)
}

// replayLeanForfeitSigningCase maps one opaque Lean identity mutation to its
// concrete production representation.
func replayLeanForfeitSigningCase(t *testing.T, name string) bool {
	t.Helper()

	paymentHash := lntypes.Hash{0x31, 0x32, 0x33}
	payload := testReceiveForfeitSignaturePayload(t, paymentHash)
	binding := receiveForfeitBinding{
		paymentHash:   paymentHash,
		vhtlcOutpoint: payload.VHTLCOutpoint,
		vhtlcAmount:   payload.VHTLCAmountSat,
		vhtlcPkScript: bytes.Clone(payload.VHTLCPkScript),
		policy:        bytes.Clone(payload.VHTLCPolicyTemplate),
	}
	payload.VHTLCPkScript = bytes.Clone(payload.VHTLCPkScript)
	payload.VHTLCPolicyTemplate = bytes.Clone(
		payload.VHTLCPolicyTemplate,
	)

	published := true
	switch name {
	case "unpublished":
		published = false

	case "exact":
	case "wrong_payment_hash":
		payload.PaymentHash[0]++

	case "wrong_outpoint":
		payload.VHTLCOutpoint = "different:0"

	case "wrong_amount":
		payload.VHTLCAmountSat++

	case "wrong_script":
		payload.VHTLCPkScript[0] ^= 0x01

	case "wrong_policy":
		// Reorder two valid leaves so decoding and the derived payment
		// hash and script still succeed. Exact policy binding is the
		// only reason this payload should be rejected.
		template, err := arkscript.DecodePolicyTemplate(
			payload.VHTLCPolicyTemplate,
		)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(template.Leaves), 2)

		template.Leaves[0], template.Leaves[1] =
			template.Leaves[1], template.Leaves[0]
		payload.VHTLCPolicyTemplate, err = template.Encode()
		require.NoError(t, err)
		require.NotEqual(
			t, binding.policy, payload.VHTLCPolicyTemplate,
		)

		policyHash, err := paymentHashFromVHTLCTemplate(
			payload.VHTLCPolicyTemplate,
		)
		require.NoError(t, err)
		require.Equal(t, binding.paymentHash, policyHash)

		pkScript, err := template.PkScript()
		require.NoError(t, err)
		require.Equal(t, binding.vhtlcPkScript, pkScript)

	case "wrong_policy_payment_hash":
		differentHash := lntypes.Hash{0xaa, 0xbb, 0xcc}
		differentPayload := testReceiveForfeitSignaturePayload(
			t, differentHash,
		)
		payload.VHTLCPolicyTemplate = bytes.Clone(
			differentPayload.VHTLCPolicyTemplate,
		)
		binding.policy = bytes.Clone(payload.VHTLCPolicyTemplate)

	default:
		t.Fatalf("unknown Lean bridge case %q", name)
	}

	gate := newReceiveForfeitBindingGate()
	if published {
		gate.binding.Store(&binding)
	}

	loaded, err := gate.load()
	if err != nil {
		return false
	}

	return validateOutSwapForfeitSignaturePayload(loaded, payload) == nil
}
