package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newTestTransferInput creates a minimally valid transfer input for unit
// tests.
func newTestTransferInput(t *testing.T, ownerKey *btcec.PrivateKey,
	operatorKey *btcec.PublicKey, outpoint wire.OutPoint,
	amount btcutil.Amount) TransferInput {

	t.Helper()

	exitDelay := uint32(10)

	tapscript, err := scripts.VTXOTapScript(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	tapKey, err := scripts.VTXOTapKey(
		ownerKey.PubKey(), operatorKey, exitDelay,
	)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	return TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: outpoint,
			Amount:   amount,
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				PubKey: ownerKey.PubKey(),
			},
			OperatorKey: operatorKey,
			TapScript:   tapscript,
		},
		OwnerLeafScript: []byte{0x51},
	}
}

// newTestTaprootPkScript returns a valid P2TR pkScript for tests.
func newTestTaprootPkScript(t *testing.T,
	key *btcec.PublicKey) []byte {

	t.Helper()

	pkScript, err := txscript.PayToTaprootScript(key)
	require.NoError(t, err)

	return pkScript
}
