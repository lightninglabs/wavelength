package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
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
			Hash: chainhash.Hash{
				0x01,
			},
			Index: 0,
		},
		Amount: btcutil.Amount(50000),
		PkScript: []byte{
			0x51,
			0x20,
		},
		OwnerKey: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
		OperatorKey: operatorKey.PubKey(),
		Expiry:      10,
		Ancestry: []types.Ancestry{{
			TreePath: &tree.Tree{
				Root: &tree.Node{},
			},
		}},
	}

	msg := &round.VTXOCreatedNotification{
		RoundID: "round-1",
		CommitmentTxID: chainhash.Hash{
			0x02,
		},
		BatchExpiry:   1000,
		CreatedHeight: 700,
		VTXOs: []*round.ClientVTXO{
			cv,
		},
	}

	result := clientVTXOToDescriptor(cv, msg)
	desc, err := result.Unpack()
	require.NoError(t, err)
	require.Equal(t, 0, desc.ChainDepth)
	require.Equal(t, "round-1", desc.RoundID)
}

// TestClientVTXOToDescriptorBuildsStandardTapScriptFromPolicyTemplate
// verifies that round-created standard VTXOs rebuild their tapscript from the
// semantic policy template instead of accidentally treating the pkScript bytes
// as encoded policy data.
func TestClientVTXOToDescriptorBuildsStandardTapScriptFromPolicyTemplate(
	t *testing.T) {

	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		clientKey.PubKey(), operatorKey.PubKey(), 144,
	)
	require.NoError(t, err)

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	require.NoError(t, err)

	pkScript, err := template.PkScript()
	require.NoError(t, err)

	cv := &round.ClientVTXO{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x03,
			},
			Index: 1,
		},
		Amount:         btcutil.Amount(75_000),
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		OwnerKey: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
		OperatorKey: operatorKey.PubKey(),
		Expiry:      144,
	}

	msg := &round.VTXOCreatedNotification{
		RoundID: "round-2",
		CommitmentTxID: chainhash.Hash{
			0x04,
		},
		BatchExpiry:   1001,
		CreatedHeight: 701,
		VTXOs: []*round.ClientVTXO{
			cv,
		},
	}

	result := clientVTXOToDescriptor(cv, msg)
	desc, err := result.Unpack()
	require.NoError(t, err)
	require.NotNil(t, desc.TapScript)
}
