package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/db/actordelivery"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/clock"
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
			OperatorKey:    operatorKey,
			TapScript:      tapscript,
			RelativeExpiry: exitDelay,
		},
		OwnerLeafScript: newTestCollabLeaf(
			t, ownerKey.PubKey(), operatorKey,
		),
	}
}

// newTestCollabLeaf builds the 2-of-2 collaborative multisig leaf script
// for checkpoint outputs in tests. This matches the protocol spec where
// the checkpoint collab path requires both owner and operator signatures.
func newTestCollabLeaf(t *testing.T, ownerKey,
	operatorKey *btcec.PublicKey) []byte {

	t.Helper()

	leaf, err := scripts.MultiSigCollabTapLeaf(ownerKey, operatorKey)
	require.NoError(t, err)

	return leaf.Script
}

// newTestTaprootPkScript returns a valid P2TR pkScript for tests.
func newTestTaprootPkScript(t *testing.T,
	key *btcec.PublicKey) []byte {

	t.Helper()

	pkScript, err := txscript.PayToTaprootScript(key)
	require.NoError(t, err)

	return pkScript
}

// newTestDeliveryStore creates a tx-aware delivery store for durable actor
// tests.
func newTestDeliveryStore(t *testing.T) actor.DeliveryStore {
	t.Helper()

	sqlDB := db.NewTestDB(t)
	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB,
		sqlDB.Backend(),
		clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.NoError(t, err)

	txAwareStore, ok := store.(*actordelivery.TxAwareActorDeliveryStore)
	require.True(t, ok)

	// Tests don't need the durable actor's outer transaction wrapper.
	return txAwareStore.Store
}
