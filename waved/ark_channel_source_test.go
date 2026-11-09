package waved

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestValidateRecoveryPackageRoots distinguishes an expected package boundary
// at a transported round tree from an actually missing OOR ancestor.
func TestValidateRecoveryPackageRoots(t *testing.T) {
	t.Parallel()

	treePath, treeOutput := sourceWatcherTree(t, 42)
	ancestry := []vtxo.Ancestry{
		{
			TreePath: treePath,
		},
	}
	require.NoError(
		t,
		validateRecoveryPackageRoots(
			ancestry, []wire.OutPoint{treeOutput},
		),
	)
	wrongIndex := treeOutput
	wrongIndex.Index++
	require.ErrorContains(
		t,
		validateRecoveryPackageRoots(
			ancestry, []wire.OutPoint{wrongIndex},
		),
		"missing root",
	)
	require.ErrorContains(
		t,
		validateRecoveryPackageRoots(
			ancestry, []wire.OutPoint{sourceWatcherOutpoint(43)},
		),
		"missing root",
	)
}

// TestRecoveryDescriptorUsesBackingDelay proves a parent spend cannot race
// recovery proof assembly against selection of the pre-signed channel path.
func TestRecoveryDescriptorUsesBackingDelay(t *testing.T) {
	t.Parallel()

	clientKey := testKeyDescriptor(t, 31)
	operatorKey := testKeyDescriptor(t, 32)
	terms := arkchannel.Terms{
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay: 144,
		},
	}
	copy(
		terms.VTXO.ClientArkKey[:],
		clientKey.PubKey.SerializeCompressed(),
	)
	copy(
		terms.VTXO.ArkOperatorKey[:],
		operatorKey.PubKey.SerializeCompressed(),
	)
	treePath, target := sourceWatcherTree(t, 33)
	rawTree, err := db.SerializeTree(treePath)
	require.NoError(t, err)

	desc, err := arkChannelRecoveryDescriptor(
		terms, arkchannel.VTXOBinding{
			OutPoint: target,
			Amount:   100_000,
		}, arkchannel.RecoveryDescriptor{
			Ancestry: []arkchannel.RecoveryAncestry{
				{TreePath: rawTree},
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, terms.VTXO.ChannelDelay, desc.RelativeExpiry)
	require.Equal(t, vtxo.VTXOStatusRecoveryOnly, desc.Status)

	backingDelay, err := channelSourceCSVDelay(
		terms, unrollbridge.ExitPolicyKind,
	)
	require.NoError(t, err)
	require.Equal(t, terms.VTXO.ChannelDelay, backingDelay)
	_, err = channelSourceCSVDelay(terms, "cooperative_close")
	require.ErrorContains(t, err, "unsupported")
}

// TestChannelOperatorKeyAcceptsOppositeParity verifies that Ark operator
// identity follows the x-only script key rather than compressed-key parity.
func TestChannelOperatorKeyAcceptsOppositeParity(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	terms := arkchannel.Terms{}
	copy(
		terms.VTXO.ArkOperatorKey[:],
		privKey.PubKey().SerializeCompressed(),
	)

	oppositeBytes := privKey.PubKey().SerializeCompressed()
	if oppositeBytes[0] == 0x02 {
		oppositeBytes[0] = 0x03
	} else {
		oppositeBytes[0] = 0x02
	}
	oppositeKey, err := btcec.ParsePubKey(oppositeBytes)
	require.NoError(t, err)
	require.False(t, privKey.PubKey().IsEqual(oppositeKey))

	operatorKey, err := channelOperatorKey(
		terms.VTXO.ArkOperatorKey, []*vtxo.Descriptor{
			{OperatorKey: oppositeKey},
		},
	)
	require.NoError(t, err)
	require.True(t, operatorKey.IsEqual(privKey.PubKey()))
}

// TestChannelOperatorKeyRejectsDifferentKey verifies that unrelated Ark
// operator keys cannot reuse the persisted ancestry.
func TestChannelOperatorKeyRejectsDifferentKey(t *testing.T) {
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	rootKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	terms := arkchannel.Terms{}
	copy(
		terms.VTXO.ArkOperatorKey[:],
		operatorKey.PubKey().SerializeCompressed(),
	)

	_, err = channelOperatorKey(
		terms.VTXO.ArkOperatorKey, []*vtxo.Descriptor{
			{OperatorKey: rootKey.PubKey()},
		},
	)
	require.ErrorContains(t, err, "operator does not match roots")
}
