package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestClientVTXOToDescriptorChainDepthZero verifies that a round-created
// VTXO descriptor has ChainDepth 0, since round VTXOs are anchored
// directly by the on-chain commitment with no OOR hops.
func TestClientVTXOToDescriptorChainDepthZero(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	cv := &round.ClientVTXO{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{0x01},
			Index: 0,
		},
		Amount:   btcutil.Amount(50000),
		PkScript: []byte{0x51, 0x20},
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
		OperatorKey: operatorKey.PubKey(),
		Expiry:      10,
		TreePath: &tree.Tree{
			Root: &tree.Node{},
		},
	}

	msg := &round.VTXOCreatedNotification{
		RoundID:        "round-1",
		CommitmentTxID: chainhash.Hash{0x02},
		BatchExpiry:    1000,
		CreatedHeight:  700,
		VTXOs:          []*round.ClientVTXO{cv},
	}

	result := clientVTXOToDescriptor(cv, msg)
	desc, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(t, 0, desc.ChainDepth)
	require.Equal(t, "round-1", desc.RoundID)
}
