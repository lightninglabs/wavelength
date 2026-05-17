package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func TestWalletEffectStoreClaimRetryDone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	effects := NewWalletEffectStore(store, clock.NewDefaultClock())

	txid := chainhash.HashH([]byte("wallet-effect-store"))
	err := effects.InsertWalletEffect(ctx, wallet.WalletEffectInsert{
		ID:             "effect-1",
		EffectType:     wallet.WalletEffectRecordLedgerSweepFee,
		IdempotencyKey: "effect-1",
		Txid:           txid[:],
		FeeSat: sql.NullInt64{
			Int64: 123,
			Valid: true,
		},
		BlockHeight: sql.NullInt32{
			Int32: 44,
			Valid: true,
		},
	})
	require.NoError(t, err)

	claimed, err := effects.ClaimDueWalletEffects(
		ctx, "worker-a", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, "effect-1", claimed[0].ID)
	require.True(t, claimed[0].ClaimToken.Valid)

	claimedAgain, err := effects.ClaimDueWalletEffects(
		ctx, "worker-b", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, claimedAgain)

	require.NoError(
		t,
		effects.ReleaseWalletEffectForRetry(
			ctx, claimed[0].ID, claimed[0].ClaimToken.String,
			time.Nanosecond, assertErr("transient"),
		),
	)

	require.Eventually(t, func() bool {
		claimedRetry, err := effects.ClaimDueWalletEffects(
			ctx, "worker-a", 10, time.Minute,
		)
		if err != nil || len(claimedRetry) != 1 {
			return false
		}

		err = effects.MarkWalletEffectDone(
			ctx, claimedRetry[0].ID,
			claimedRetry[0].ClaimToken.String,
		)

		return err == nil
	}, time.Second, 10*time.Millisecond)

	claimedDone, err := effects.ClaimDueWalletEffects(
		ctx, "worker-c", 10, time.Minute,
	)
	require.NoError(t, err)
	require.Empty(t, claimedDone)
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
