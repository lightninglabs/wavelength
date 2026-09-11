package unroll

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestLegacyFeeReadmissionGuards proves that permanent failures, mismatched
// checkpoint identity and unfinished old ownership cannot start another actor.
func TestLegacyFeeReadmissionGuards(t *testing.T) {
	for _, name := range []string{
		"permanent",
		"candidateMismatch",
		"policyMismatch",
		"draining",
		"persisting",
		"writeFailure",
	} {
		t.Run(name, func(t *testing.T) {
			testLegacyFeeReadmissionGuard(t, name)
		})
	}
}

// testLegacyFeeReadmissionGuard runs one legacy admission boundary case.
func testLegacyFeeReadmissionGuard(t *testing.T, name string) {
	t.Helper()
	proof := buildLinearProof(t)
	target := proof.TargetOutpoint()
	desc := testDescriptor(t, target, proof.CSVDelay())
	tx, err := buildSweepTx(
		t.Context(), &fakeSweepWallet{}, &fakeChainSourceRef{}, proof,
		desc, 0, 0, 104, NewStandardVTXOExitSpendPolicy(desc),
	)
	require.NoError(t, err)
	txid := tx.TxHash()
	reason := "sweep tx " + txid.String() + " failed: min relay fee not met"
	if name == "permanent" {
		reason = "sweep tx " + txid.String() +
			" failed: mandatory-script-verify-flag-failed"
	}
	row := RegistryRecord{TargetOutpoint: target, ActorID: actorIDForTarget(
		target,
	),
		Phase: PhaseFailed, FailReason: reason, SweepTxid: &txid}
	cp := &actorCheckpoint{
		Version: 1, Started: true, Height: 104,
		SweepTx: tx, Fail: reason,
		State: unrollplan.State{ConfirmedTxids: []chainhash.Hash{
			proof.RootTxids()[0],
			target.Hash,
		},
			TargetConfirmHeight: fn.Some[int32](102),
			Sweep: unrollplan.SweepState{
				Status: unrollplan.SweepStatusBroadcasted,
				Txid:   fn.Some(txid),
			}},
	}
	if name == "candidateMismatch" {
		cp.SweepTx.TxOut[0].Value--
	}
	if name == "policyMismatch" {
		cp.ExitPolicyKind = "other-policy"
		cp.ExitPolicyRef = "other-ref"
	}
	raw, err := encodeCheckpoint(cp)
	require.NoError(t, err)
	delivery := newMemCheckpointStore()
	require.NoError(
		t,
		delivery.SaveCheckpoint(
			t.Context(), actor.CheckpointParams{
				ActorID:   row.ActorID,
				StateData: raw,
			},
		),
	)
	store := newMemRegistryStore()
	require.NoError(t, store.UpsertRecord(t.Context(), row))
	r := &registryBehavior{
		cfg: RegistryConfig{
			Store:         store,
			DeliveryStore: delivery,
		},
	}
	if name == "writeFailure" {
		r.cfg.Store = &readmissionFailStore{store}
	}
	if name == "draining" {
		r.feeFailureDrains = map[wire.OutPoint]<-chan struct{}{
			target: make(chan struct{}),
		}
	}
	if name == "persisting" {
		r.pending = map[wire.OutPoint]RegistryRecord{
			target: row,
		}
	}
	_, err = r.
		handleEnsure(
			t.Context(),
			&EnsureUnrollRequest{Outpoint: target},
		).
		Unpack()
	if name == "permanent" {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
	unchanged, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.Equal(t, row, *unchanged)
	require.Empty(t, r.active)
}

// readmissionFailStore models a database failure before a legacy row is
// revived.
type readmissionFailStore struct{ *memRegistryStore }

// UpsertRecord refuses the recovery write without modifying the durable record.
func (s *readmissionFailStore) UpsertRecord(_ context.Context,
	_ RegistryRecord) error {

	return errors.New("readmission write unavailable")
}
