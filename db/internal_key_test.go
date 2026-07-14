package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newInternalKeyExecutorForTest builds a transaction executor over the raw
// sqlc.Queries so tests can drive the internal-key registry helpers directly.
func newInternalKeyExecutorForTest(
	t *testing.T) *TransactionExecutor[*sqlc.Queries] {

	db := NewTestDB(t)

	return NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)
}

// testKeyDesc builds a deterministic-enough KeyDescriptor for registry tests.
func testKeyDesc(t *testing.T, family keychain.KeyFamily,
	index uint32) keychain.KeyDescriptor {

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return keychain.KeyDescriptor{
		PubKey: priv.PubKey(),
		KeyLocator: keychain.KeyLocator{
			Family: family,
			Index:  index,
		},
	}
}

// TestRegisterInternalKeyRoundTrip checks that registering a key returns an id
// that hydrates back into the same descriptor, that re-registering the same
// triple is idempotent, and that distinct triples get distinct ids.
func TestRegisterInternalKeyRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	exec := newInternalKeyExecutorForTest(t)

	descA := testKeyDesc(t, 7, 42)
	descB := testKeyDesc(t, 7, 43)

	var idA, idAAgain, idB int64
	err := exec.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		var err error
		idA, err = RegisterInternalKeyTx(ctx, q, 1000, descA)
		if err != nil {
			return err
		}

		// Re-registering the identical triple must be idempotent and
		// return the existing id.
		idAAgain, err = RegisterInternalKeyTx(ctx, q, 2000, descA)
		if err != nil {
			return err
		}

		// A distinct triple must get a distinct id.
		idB, err = RegisterInternalKeyTx(ctx, q, 3000, descB)

		return err
	})
	require.NoError(t, err)

	require.Equal(t, idA, idAAgain)
	require.NotEqual(t, idA, idB)

	// Hydrating each id must reconstruct the original descriptor.
	err = exec.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		gotA, err := InternalKeyDescByIDTx(ctx, q, idA)
		if err != nil {
			return err
		}
		require.True(t, descA.PubKey.IsEqual(gotA.PubKey))
		require.Equal(t, descA.KeyLocator, gotA.KeyLocator)

		gotB, err := InternalKeyDescByIDTx(ctx, q, idB)
		if err != nil {
			return err
		}
		require.True(t, descB.PubKey.IsEqual(gotB.PubKey))
		require.Equal(t, descB.KeyLocator, gotB.KeyLocator)

		return nil
	})
	require.NoError(t, err)
}

// TestRegisterInternalKeyNilPubKey checks that a descriptor with no public key
// is rejected.
func TestRegisterInternalKeyNilPubKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	exec := newInternalKeyExecutorForTest(t)

	err := exec.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		_, regErr := RegisterInternalKeyTx(
			ctx, q, 1000, keychain.KeyDescriptor{},
		)

		return regErr
	})
	require.ErrorIs(t, err, ErrInternalKeyPubKeyMissing)
}

// TestInternalKeyDescByIDNotFound checks that hydrating an unknown id returns
// ErrInternalKeyNotFound.
func TestInternalKeyDescByIDNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	exec := newInternalKeyExecutorForTest(t)

	err := exec.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		_, getErr := InternalKeyDescByIDTx(ctx, q, 9999)

		return getErr
	})
	require.ErrorIs(t, err, ErrInternalKeyNotFound)
}
