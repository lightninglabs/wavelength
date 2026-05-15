package darepod

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestListReceiveScriptsRequiresWalletReady verifies the receive-list RPC
// returns the shared wallet readiness error before touching the indexer.
func TestListReceiveScriptsRequiresWalletReady(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	_, err := r.ListReceiveScripts(
		context.Background(), &daemonrpc.ListReceiveScriptsRequest{},
	)

	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "wallet is not ready")
}

// TestAddressFromTaprootPkScript verifies native taproot scripts are rendered
// as network-correct bech32m addresses.
func TestAddressFromTaprootPkScript(t *testing.T) {
	t.Parallel()

	addr, pkScript := testTaprootReceiveScript(t)

	encoded, err := addressFromTaprootPkScript(
		pkScript, &chaincfg.TestNet3Params,
	)
	require.NoError(t, err)
	require.Equal(t, addr.EncodeAddress(), encoded)
}

// TestAddressFromTaprootPkScriptRejectsNonTaproot verifies the receive-target
// formatter fails closed for scripts that cannot be represented as a v1
// taproot address.
func TestAddressFromTaprootPkScriptRejectsNonTaproot(t *testing.T) {
	t.Parallel()

	_, err := addressFromTaprootPkScript(
		[]byte{txscript.OP_0}, &chaincfg.TestNet3Params,
	)
	require.ErrorContains(t, err, "not a native taproot")
}

// TestReceiveTargetFromRegisteredScriptRejectsNil verifies defensive receive
// target formatting rejects missing indexer records before dereferencing them.
func TestReceiveTargetFromRegisteredScriptRejectsNil(t *testing.T) {
	t.Parallel()

	_, err := receiveTargetFromRegisteredScript(
		nil, &chaincfg.TestNet3Params,
	)
	require.ErrorContains(t, err, "receive script is nil")
}

// TestReceiveTargetFromRegisteredScript verifies registered receive-script
// metadata is preserved while deriving the user-facing address.
func TestReceiveTargetFromRegisteredScript(t *testing.T) {
	t.Parallel()

	addr, pkScript := testTaprootReceiveScript(t)
	target, err := receiveTargetFromRegisteredScript(
		&arkrpc.RegisteredReceiveScript{
			PkScript:       pkScript,
			ExpiresAtUnixS: 123,
			Label:          "coffee",
		}, &chaincfg.TestNet3Params,
	)
	require.NoError(t, err)

	require.Equal(t, addr.EncodeAddress(), target.GetAddress())
	require.Equal(t, hex.EncodeToString(pkScript), target.GetPkScriptHex())
	require.Equal(t, "coffee", target.GetLabel())
	require.EqualValues(t, 123, target.GetExpiresAtUnixS())
}

// testTaprootReceiveScript returns a deterministic testnet taproot address and
// pkScript pair for receive-target tests.
func testTaprootReceiveScript(t *testing.T) (*btcutil.AddressTaproot, []byte) {
	t.Helper()

	key, err := hex.DecodeString(
		"00010203040506070809000102030405060708090001020304050607080" +
			"90001",
	)
	require.NoError(t, err)

	addr, err := btcutil.NewAddressTaproot(key, &chaincfg.TestNet3Params)
	require.NoError(t, err)

	pkScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	return addr, pkScript
}
