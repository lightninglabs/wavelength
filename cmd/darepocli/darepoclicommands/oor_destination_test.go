package darepoclicommands

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// TestBuildOORRecipientOutputAddress verifies address destinations are passed
// through unchanged.
func TestBuildOORRecipientOutputAddress(t *testing.T) {
	t.Parallel()

	output, err := buildOORRecipientOutput(
		"tb1ptestdestination", "", 12_345,
	)
	require.NoError(t, err)

	addr, ok := output.Destination.(*daemonrpc.Output_Address)
	require.True(t, ok)
	require.Equal(t, "tb1ptestdestination", addr.Address)
	require.EqualValues(t, 12_345, output.AmountSat)
}

// TestBuildOORRecipientOutputPubKey verifies x-only pubkey destinations are
// decoded and normalized.
func TestBuildOORRecipientOutputPubKey(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHex := schnorr.SerializePubKey(privKey.PubKey())

	output, err := buildOORRecipientOutput(
		"", hex.EncodeToString(pubKeyHex), 9_999,
	)
	require.NoError(t, err)

	pubKey, ok := output.Destination.(*daemonrpc.Output_Pubkey)
	require.True(t, ok)
	require.Equal(t, pubKeyHex, pubKey.Pubkey)
	require.EqualValues(t, 9_999, output.AmountSat)
}

// TestBuildOORRecipientOutputValidation verifies the helper rejects ambiguous
// or malformed destinations.
func TestBuildOORRecipientOutputValidation(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		address     string
		pubKeyHex   string
		amount      int64
		errContains string
	}{
		{
			name:        "missing destination",
			amount:      1,
			errContains: "exactly one",
		},
		{
			name:        "multiple destinations",
			address:     "tb1paddr",
			pubKeyHex:   "00",
			amount:      1,
			errContains: "exactly one",
		},
		{
			name:        "non-positive amount",
			address:     "tb1paddr",
			errContains: "amount must be positive",
		},
		{
			name:        "short pubkey",
			pubKeyHex:   "00",
			amount:      1,
			errContains: "32-byte x-only key",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := buildOORRecipientOutput(
				testCase.address, testCase.pubKeyHex,
				testCase.amount,
			)
			require.ErrorContains(t, err, testCase.errContains)
		})
	}
}

// TestMethodRegistrySendOORSchema verifies the schema advertises the public
// OOR send destination forms.
func TestMethodRegistrySendOORSchema(t *testing.T) {
	t.Parallel()

	method := findSchemaMethod(t, "ark.send.oor")
	paramNames := make([]string, 0, len(method.Params))
	for _, param := range method.Params {
		paramNames = append(paramNames, param.Name)
	}

	require.Contains(t, paramNames, "to")
	require.Contains(t, paramNames, "pubkey")
	require.Contains(t, paramNames, "amount")
	require.Contains(t, paramNames, "idempotency_key")
}

// TestMethodRegistryOORReceiveSchema verifies the public schema still exposes
// OOR receive allocation.
func TestMethodRegistryOORReceiveSchema(t *testing.T) {
	t.Parallel()

	method := findSchemaMethod(t, "ark.oor.receive")
	require.Equal(t, "NewReceiveScriptRequest", method.RequestType)
	require.Equal(t, "NewReceiveScriptResponse", method.ResponseType)
}

// TestOORGetAcceptsSnakeCaseSessionID verifies the oor get command resolves
// both the registered kebab flag (--session-id) and the snake_case spelling
// (--session_id) used by the JSON output and daemon logs to the same value
// (issue #900).
func TestOORGetAcceptsSnakeCaseSessionID(t *testing.T) {
	t.Parallel()

	for _, spelling := range []string{"--session-id", "--session_id"} {
		spelling := spelling

		t.Run(spelling, func(t *testing.T) {
			t.Parallel()

			cmd := newOORGetCmd()
			err := cmd.Flags().Parse([]string{spelling, "sess-1"})
			require.NoError(t, err)

			got, err := cmd.Flags().GetString("session-id")
			require.NoError(t, err)
			require.Equal(t, "sess-1", got)
		})
	}
}

// findSchemaMethod locates a method entry in the CLI schema registry.
func findSchemaMethod(t *testing.T, methodName string) schemaMethod {
	t.Helper()

	for _, method := range methodRegistry() {
		if method.Method == methodName {
			return method
		}
	}

	t.Fatalf("method %q not found", methodName)

	return schemaMethod{}
}
