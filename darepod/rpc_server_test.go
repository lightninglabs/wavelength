package darepod

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// TestResolveRecipientOwnerKey covers destination resolution for
// directed sends.
func TestResolveRecipientOwnerKey(t *testing.T) {
	t.Parallel()

	// Generate a test key.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	xOnlyBytes := schnorr.SerializePubKey(privKey.PubKey())

	t.Run("pubkey valid", func(t *testing.T) {
		t.Parallel()

		out := &daemonrpc.Output{
			Destination: &daemonrpc.Output_Pubkey{
				Pubkey: xOnlyBytes,
			},
		}

		key, err := resolveRecipientOwnerKey(out)
		require.NoError(t, err)
		require.True(t,
			key.IsEqual(privKey.PubKey()),
			"resolved key should match input",
		)
	})

	t.Run("pubkey wrong length", func(t *testing.T) {
		t.Parallel()

		out := &daemonrpc.Output{
			Destination: &daemonrpc.Output_Pubkey{
				Pubkey: []byte{0x01, 0x02},
			},
		}

		_, err := resolveRecipientOwnerKey(out)
		require.Error(t, err)
		require.Contains(t, err.Error(), "pubkey must be")
	})

	t.Run("address rejected", func(t *testing.T) {
		t.Parallel()

		out := &daemonrpc.Output{
			Destination: &daemonrpc.Output_Address{
				Address: "bcrt1pxxx",
			},
		}

		_, err := resolveRecipientOwnerKey(out)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not supported")
	})

	t.Run("pk_script rejected", func(t *testing.T) {
		t.Parallel()

		out := &daemonrpc.Output{
			Destination: &daemonrpc.Output_PkScript{
				PkScript: []byte{0x51, 0x20},
			},
		}

		_, err := resolveRecipientOwnerKey(out)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not supported")
	})
}
