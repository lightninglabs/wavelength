package db

import (
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newUnilateralExitStoreForTest creates a unilateral-exit store backed by a
// fresh test database.
func newUnilateralExitStoreForTest(
	t *testing.T) *UnilateralExitPersistenceStore {

	t.Helper()

	db := NewTestDB(t)

	exitDB := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) UnilateralExitStore {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewUnilateralExitPersistenceStore(
		exitDB, clock.NewDefaultClock(),
	)
}

// exitStoreFromTestDB builds a unilateral-exit store over an already-open test
// database handle. It lets a test instantiate two independent stores over the
// same underlying DB to simulate a daemon restart (the second store holds no
// in-memory state carried over from the first).
func exitStoreFromTestDB(base *BaseDB) *UnilateralExitPersistenceStore {
	exitDB := NewTransactionExecutor(
		base,
		func(tx *sql.Tx) UnilateralExitStore {
			return base.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewUnilateralExitPersistenceStore(
		exitDB, clock.NewDefaultClock(),
	)
}

// TestExitFundingAddressStableAcrossRestart verifies that a persisted exit
// funding address is stable for the same target VTXO outpoint across a
// simulated daemon restart: the second store must return the already-derived
// address without deriving a new one, and a different outpoint must still get
// its own address (wavelength#893).
func TestExitFundingAddressStableAcrossRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	testDB := NewTestDB(t)
	store := exitStoreFromTestDB(testDB.BaseDB)

	target := wire.OutPoint{
		Hash: chainhash.Hash{
			0xaa,
			0x01,
		},
		Index: 3,
	}

	// First derivation persists a fresh address.
	derived := 0
	newAddress := func(context.Context) (string, error) {
		derived++

		return "bcrt1qfunding", nil
	}

	addr, err := store.FundingAddress(ctx, target, newAddress)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qfunding", addr)
	require.Equal(t, 1, derived)

	// A second poll on the same store returns the cached address without
	// re-deriving.
	addr, err = store.FundingAddress(ctx, target, newAddress)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qfunding", addr)
	require.Equal(t, 1, derived)

	// Simulate a restart: a brand-new store over the same DB must return
	// the persisted address, NOT a freshly derived one. If it derived a
	// new address the user would be asked to fund a second deposit for the
	// same VTXO, which is the bug being fixed.
	restarted := exitStoreFromTestDB(testDB.BaseDB)
	failIfDerived := func(context.Context) (string, error) {
		t.Fatal(
			"restart derived a new funding address instead of " +
				"reusing the persisted one",
		)

		return "", nil
	}

	addr, err = restarted.FundingAddress(ctx, target, failIfDerived)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qfunding", addr)

	// A different outpoint still gets its own distinct address.
	other := wire.OutPoint{
		Hash: chainhash.Hash{
			0xbb,
			0x02,
		},
		Index: 7,
	}
	otherAddress := func(context.Context) (string, error) {
		return "bcrt1qother", nil
	}

	addr, err = restarted.FundingAddress(ctx, other, otherAddress)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qother", addr)
}

// TestUnilateralExitStoreListNonTerminalJobs verifies that restore queries only
// return non-terminal manager-facing job rows.
func TestUnilateralExitStoreListNonTerminalJobs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUnilateralExitStoreForTest(t)

	pendingTarget := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
			0x01,
		},
		Index: 1,
	}
	completedTarget := wire.OutPoint{
		Hash: chainhash.Hash{
			0x22,
			0x02,
		},
		Index: 2,
	}
	failedTarget := wire.OutPoint{
		Hash: chainhash.Hash{
			0x33,
			0x03,
		},
		Index: 3,
	}

	err := store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: pendingTarget,
		ActorID:        "job-pending",
		Status:         UnilateralExitJobStatusMaterializing,
		Trigger:        UnilateralExitJobTriggerManual,
		CreatedAt:      time.Unix(10, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: completedTarget,
		ActorID:        "job-completed",
		Status:         UnilateralExitJobStatusCompleted,
		Trigger:        UnilateralExitJobTriggerRestart,
		CreatedAt:      time.Unix(20, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: failedTarget,
		ActorID:        "job-failed",
		Status:         UnilateralExitJobStatusFailed,
		Trigger:        UnilateralExitJobTriggerCriticalExpiry,
		LastError:      "boom",
		CreatedAt:      time.Unix(30, 0),
	})
	require.NoError(t, err)

	jobs, err := store.ListNonTerminalJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, pendingTarget, jobs[0].TargetOutpoint)
	require.Equal(t, "job-pending", jobs[0].ActorID)
	require.Equal(t, UnilateralExitJobStatusMaterializing,
		jobs[0].Status)
}

// TestUnilateralExitStoreUpsertPersistsSweepTxid verifies that terminal
// control-plane updates preserve the sweep txid on conflict updates.
func TestUnilateralExitStoreUpsertPersistsSweepTxid(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUnilateralExitStoreForTest(t)
	target := wire.OutPoint{
		Hash: chainhash.Hash{
			0x44,
			0x04,
		},
		Index: 4,
	}
	sweepTxid := bytes.Repeat([]byte{0xAB}, chainhash.HashSize)

	err := store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: target,
		ActorID:        "job-active",
		Status:         UnilateralExitJobStatusSweeping,
		Trigger:        UnilateralExitJobTriggerManual,
		CreatedAt:      time.Unix(40, 0),
	})
	require.NoError(t, err)

	err = store.UpsertJob(ctx, UnilateralExitJobRecord{
		TargetOutpoint: target,
		ActorID:        "job-completed",
		Status:         UnilateralExitJobStatusCompleted,
		Trigger:        UnilateralExitJobTriggerManual,
		SweepTxid:      sweepTxid,
		CreatedAt:      time.Unix(40, 0),
	})
	require.NoError(t, err)

	job, err := store.GetJob(ctx, target)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, UnilateralExitJobStatusCompleted, job.Status)
	require.Equal(t, "job-completed", job.ActorID)
	require.Equal(t, sweepTxid, job.SweepTxid)
}

// TestUnilateralExitStoreGetJobMissingRowIsQuiet verifies that GetJob on a
// missing row returns ErrUnilateralExitJobNotFound without the underlying
// TransactionExecutor logging a "Transaction body failed" WARN. The miss is
// the normal negative-lookup path (callers depend on the sentinel to
// distinguish "no such job" from a real DB error), and the transaction
// layer must recognise it as benign so noisy WARN lines do not appear in
// production logs on every Unroll RPC.
func TestUnilateralExitStoreGetJobMissingRowIsQuiet(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := btclog.NewSLogger(btclog.NewDefaultHandler(&logBuf))
	logger.SetLevel(btclog.LevelTrace)

	testDB := NewTestDB(t)

	exitDB := NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) UnilateralExitStore {
			return testDB.WithTx(tx)
		},
		logger,
	)
	store := NewUnilateralExitPersistenceStore(
		exitDB, clock.NewDefaultClock(),
	)

	target := wire.OutPoint{
		Hash: chainhash.Hash{
			0x55,
			0x05,
		},
		Index: 5,
	}

	job, err := store.GetJob(t.Context(), target)
	require.ErrorIs(t, err, ErrUnilateralExitJobNotFound)
	require.Nil(t, job)

	require.NotContains(
		t, logBuf.String(),
		"Transaction body failed", "missing-row lookups must not "+
			"log a Transaction body failed WARN; raw log: %s",
		logBuf.String(),
	)
}
