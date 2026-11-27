package db

import (
	"os"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestManualExitOptionsSurviveStatusReload checks the durable handoff before
// the exit registry sees the status transition, and a subsequent safe retry.
func TestManualExitOptionsSurviveStatusReload(t *testing.T) {
	t.Parallel()

	store, rounds, _ := newVTXOStoreForTest(t)
	ctx := t.Context()
	roundID := testRoundIDDB("manual-exit-options")
	testRound := createTestRound(t, roundID)
	trees := make(map[round.SignerKey]*tree.Tree)
	require.NoError(
		t,
		rounds.CommitState(
			ctx, testRound, &round.InputSigSentState{
				RoundID:     testRound.RoundID,
				ClientTrees: trees,
			},
		),
	)
	desc := createTestVTXODescriptor(t, roundID, 7)
	require.NoError(t, store.SaveVTXO(ctx, desc))

	options := &actormsg.ManualExitOptions{
		SweepScript: []byte{
			0x51,
			0x20,
			0xde,
			0xad,
		},
		FeeRateSatPerVByte: 7,
	}
	require.NoError(
		t, store.UpdateVTXOStatusWithExitOptions(
			ctx, desc.Outpoint, options, false,
		),
	)
	saved, err := store.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusUnilateralExit, saved.Status)
	require.Equal(t, options, saved.ManualExitOptions)

	require.NoError(
		t, store.UpdateVTXOStatus(
			ctx, desc.Outpoint, vtxo.VTXOStatusLive,
		),
	)
	require.NoError(
		t, store.UpdateVTXOStatus(
			ctx, desc.Outpoint, vtxo.VTXOStatusUnilateralExit,
		),
	)
	saved, err = store.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Nil(t, saved.ManualExitOptions)
}

// TestManualExitMigrationAfterFeeSplit preserves a pending signed split when
// upgrading from the public schema 33.
func TestManualExitMigrationAfterFeeSplit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	database := NewTestDBWithVersion(t, 33)
	candidate := []byte("pending signed split")
	_, err := database.SaveWalletFeeSplit(ctx, candidate)
	require.NoError(t, err)

	require.NoError(t, database.ExecuteMigrations(TargetLatest))
	saved, err := database.LoadWalletFeeSplit(ctx)
	require.NoError(t, err)
	require.Equal(t, candidate, saved)
}

// TestManualExitMigrationFromMobile33 recreates the wallet schema used by
// early mobile builds and checks that upgrading supplies the missing journal.
func TestManualExitMigrationFromMobile33(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	database := NewTestSqliteDBWithVersion(t, 33)
	// The original mobile migration has the same table and constraints.
	migration, err := os.ReadFile(
		"sqlc/migrations/000034_manual_exit_options.up.sql",
	)
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, "DROP TABLE wallet_fee_split")
	require.NoError(t, err)

	stores := NewStore(
		database.DB, database.Queries, database.Backend(),
		btclog.Disabled,
	)
	clk := clock.NewDefaultClock()
	rounds := stores.NewRoundStore(&chaincfg.RegressionNetParams, clk)
	store := stores.NewVTXOStore(clk)
	roundID := testRoundIDDB("manual-exit-migration")
	testRound := createTestRound(t, roundID)
	trees := make(map[round.SignerKey]*tree.Tree)
	require.NoError(
		t,
		rounds.CommitState(
			ctx, testRound, &round.InputSigSentState{
				RoundID:     roundID,
				ClientTrees: trees,
			},
		),
	)
	desc := createTestVTXODescriptor(t, roundID, 17)
	require.NoError(t, store.SaveVTXO(ctx, desc))
	options := &actormsg.ManualExitOptions{
		SweepScript: []byte{
			0x51,
		}, FeeRateSatPerVByte: 7,
	}
	require.NoError(
		t, store.UpdateVTXOStatusWithExitOptions(
			ctx, desc.Outpoint, options, false,
		),
	)

	require.NoError(t, database.ExecuteMigrations(TargetLatest))
	saved, err := store.GetVTXO(ctx, desc.Outpoint)
	require.NoError(t, err)
	require.Equal(t, options, saved.ManualExitOptions)
	_, err = database.SaveWalletFeeSplit(ctx, []byte("new split"))
	require.NoError(t, err)
}
