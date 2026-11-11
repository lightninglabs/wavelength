package waved

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/db"
	oorlib "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/stretchr/testify/require"
)

// compactRecoveryTestPacket reproduces the indexer's metadata-free inventory
// representation while retaining its exact finalized Taproot witness.
func compactRecoveryTestPacket(t *testing.T, full *psbt.Packet) *psbt.Packet {
	t.Helper()

	proof, err := psbt.NewFromUnsignedTx(full.UnsignedTx.Copy())
	require.NoError(t, err)
	for i, input := range full.Inputs {
		witness, err := oorlib.BuildTaprootWitness(input)
		require.NoError(t, err)
		var encoded bytes.Buffer
		require.NoError(t, psbt.WriteTxWitness(&encoded, witness))
		proof.Inputs[i].FinalScriptWitness = encoded.Bytes()
	}

	return proof
}

// TestIndexedRecoveryReusesFullPackage covers seed recovery restoring an OOR
// change output before vHTLC recovery fetches its sibling's compact proof.
func TestIndexedRecoveryReusesFullPackage(t *testing.T) {
	t.Parallel()

	for _, direction := range []db.OORPackageDirection{
		db.OORPackageDirectionIncoming, db.OORPackageDirectionOutgoing,
	} {
		t.Run(string(direction), func(t *testing.T) {
			t.Parallel()
			job, candidate := indexedRecoveryFixture(t)
			m := newIndexedRecoveryMaterializer(t, candidate)
			pkg := candidate.AncestryPackages[0]
			fullArk, err := psbtutil.Parse(pkg.ArkPsbt)
			require.NoError(t, err)
			fullCheckpoint, err := psbtutil.Parse(
				pkg.CheckpointPsbts[0],
			)
			require.NoError(t, err)
			originalArk := bytes.Clone(pkg.ArkPsbt)
			originalCheckpoint := bytes.Clone(
				pkg.CheckpointPsbts[0],
			)
			err = m.packages.UpsertPackage(
				t.Context(), direction,
				fullArk.UnsignedTx.TxHash(), fullArk,
				[]*psbt.Packet{fullCheckpoint},
			)
			require.NoError(t, err)

			pkg.ArkPsbt, err = psbtutil.Serialize(
				compactRecoveryTestPacket(t, fullArk),
			)
			require.NoError(t, err)
			pkg.CheckpointPsbts[0], err = psbtutil.Serialize(
				compactRecoveryTestPacket(t, fullCheckpoint),
			)
			require.NoError(t, err)
			require.NotEqual(t, originalArk, pkg.ArkPsbt)
			require.NotEqual(
				t, originalCheckpoint, pkg.CheckpointPsbts[0],
			)

			require.NoError(
				t,
				m.EnsureRecoveryTarget(
					t.Context(), job,
				),
			)
			stored, err := m.packages.GetPackage(
				t.Context(), fullArk.UnsignedTx.TxHash(),
			)
			require.NoError(t, err)
			require.Equal(t, direction, stored.Direction)
			arkRaw, err := psbtutil.Serialize(stored.ArkPSBT)
			require.NoError(t, err)
			require.Equal(t, originalArk, arkRaw)
			checkpointRaw, err := psbtutil.Serialize(
				stored.FinalCheckpointPSBTs[0],
			)
			require.NoError(t, err)
			require.Equal(t, originalCheckpoint, checkpointRaw)

			assembler := &unroll.LocalProofAssembler{
				VTXOStore: m.vtxos, ArtifactStore: m.packages,
			}
			_, err = assembler.EnsureProof(
				t.Context(), job.VTXOOutpoint,
			)
			require.NoError(t, err)
		})
	}
}

// TestRecoveryPackageRejectsSignedMutation rejects txid-preserving witness
// changes as well as transaction and graph changes without modifying storage.
func TestRecoveryPackageRejectsSignedMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*db.OORPackageBundle)
	}{
		{
			name: "ark witness",
			mutate: func(pkg *db.OORPackageBundle) {
				pkg.ArkPSBT.Inputs[0].FinalScriptWitness[2] ^= 1
			},
		},
		{
			name: "checkpoint witness",
			mutate: func(pkg *db.OORPackageBundle) {
				pkg.FinalCheckpointPSBTs[0].Inputs[0].
					FinalScriptWitness[2] ^= 1
			},
		},
		{
			name: "ark transaction",
			mutate: func(pkg *db.OORPackageBundle) {
				pkg.ArkPSBT.UnsignedTx.TxOut[0].Value--
			},
		},
		{
			name: "checkpoint transaction",
			mutate: func(pkg *db.OORPackageBundle) {
				checkpoint := pkg.FinalCheckpointPSBTs[0]
				checkpoint.UnsignedTx.LockTime++
			},
		},
		{
			name: "checkpoint count",
			mutate: func(pkg *db.OORPackageBundle) {
				pkg.FinalCheckpointPSBTs = nil
			},
		},
		{
			name: "missing witness",
			mutate: func(pkg *db.OORPackageBundle) {
				pkg.ArkPSBT.Inputs[0].FinalScriptWitness = nil
			},
		},
		{
			name: "malformed witness",
			mutate: func(pkg *db.OORPackageBundle) {
				input := &pkg.ArkPSBT.Inputs[0]
				input.FinalScriptWitness = []byte{
					1,
				}
			},
		},
		{
			name: "script sig",
			mutate: func(pkg *db.OORPackageBundle) {
				pkg.ArkPSBT.Inputs[0].FinalScriptSig = []byte{
					1,
				}
			},
		},
		{
			name: "missing ark",
			mutate: func(pkg *db.OORPackageBundle) {
				pkg.ArkPSBT = nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, candidate := indexedRecoveryFixture(t)
			m := newIndexedRecoveryMaterializer(t, candidate)
			pkg := candidate.AncestryPackages[0]
			fullArk, err := psbtutil.Parse(pkg.ArkPsbt)
			require.NoError(t, err)
			checkpoint, err := psbtutil.Parse(
				pkg.CheckpointPsbts[0],
			)
			require.NoError(t, err)
			id := fullArk.UnsignedTx.TxHash()
			err = m.packages.UpsertPackage(
				t.Context(), db.OORPackageDirectionIncoming, id,
				fullArk, []*psbt.Packet{checkpoint},
			)
			require.NoError(t, err)
			compactArk := compactRecoveryTestPacket(t, fullArk)
			compactCheckpoint := compactRecoveryTestPacket(
				t, checkpoint,
			)
			incoming := &db.OORPackageBundle{
				SessionID: id,
				ArkPSBT:   compactArk,
				FinalCheckpointPSBTs: []*psbt.Packet{
					compactCheckpoint,
				},
			}
			test.mutate(incoming)
			require.Error(
				t,
				persistIndexedRecoveryPackage(
					t.Context(), m.packages, incoming,
				),
			)
			stored, err := m.packages.GetPackage(t.Context(), id)
			require.NoError(t, err)
			raw, err := psbtutil.Serialize(stored.ArkPSBT)
			require.NoError(t, err)
			require.Equal(t, pkg.ArkPsbt, raw)
			raw, err = psbtutil.Serialize(
				stored.FinalCheckpointPSBTs[0],
			)
			require.NoError(t, err)
			require.Equal(t, pkg.CheckpointPsbts[0], raw)
		})
	}
}

// recoveryPackageStoreStub makes the lookup/insert/reload race deterministic
// without changing the real artifact store's transaction or immutability rules.
type recoveryPackageStoreStub struct {
	existing  *db.OORPackageBundle
	raced     *db.OORPackageBundle
	readErr   error
	writeErr  error
	reloadErr error
	reads     int
	writes    int
}

// GetPackage models a wallet replay inserting between the initial read and
// the failed import write, or a genuine storage failure at either read.
func (s *recoveryPackageStoreStub) GetPackage(context.Context, chainhash.Hash) (
	*db.OORPackageBundle, error) {

	s.reads++
	if s.reads == 1 {
		if s.readErr != nil {
			return nil, s.readErr
		}
		if s.existing != nil {
			return s.existing, nil
		}

		return nil, sql.ErrNoRows
	}
	if s.reloadErr != nil {
		return nil, s.reloadErr
	}
	if s.raced != nil {
		return s.raced, nil
	}

	return nil, sql.ErrNoRows
}

// UpsertPackage reports the configured write result while counting attempts.
func (s *recoveryPackageStoreStub) UpsertPackage(context.Context,
	db.OORPackageDirection, chainhash.Hash, *psbt.Packet,
	[]*psbt.Packet) error {

	s.writes++

	return s.writeErr
}

// TestRecoveryPackageImportStoreErrors permits a raced insertion only after
// reloading and matching its complete signed graph. All other storage and
// mismatch errors stay visible to the caller, including cancellation.
func TestRecoveryPackageImportStoreErrors(t *testing.T) {
	t.Parallel()
	readFailure := errors.New("package read failed")
	writeFailure := errors.New("package insert failed")
	reloadFailure := errors.New("package reload failed")
	tests := []struct {
		name         string
		existing     bool
		race         bool
		wrongWitness bool
		readErr      error
		writeErr     error
		reloadErr    error
		wantErrors   []error
		wantMismatch bool
		reads        int
		writes       int
	}{
		{
			name: "existing equivalent", existing: true, reads: 1,
		},
		{
			name: "new insert", reads: 1, writes: 1,
		},
		{
			name: "initial read error", readErr: readFailure,
			wantErrors: []error{
				readFailure,
			}, reads: 1,
		},
		{
			name:    "initial read cancelled",
			readErr: context.Canceled,
			wantErrors: []error{
				context.Canceled,
			}, reads: 1,
		},
		{
			name: "raced equivalent insert", race: true,
			writeErr: writeFailure, reads: 2, writes: 1,
		},
		{
			name: "raced different witness", race: true,
			wrongWitness: true, writeErr: writeFailure,
			wantErrors: []error{
				writeFailure,
			}, wantMismatch: true,
			reads: 2, writes: 1,
		},
		{
			name:     "write error without raced row",
			writeErr: writeFailure,
			wantErrors: []error{
				writeFailure,
				sql.ErrNoRows,
			},
			reads: 2, writes: 1,
		},
		{
			name: "reread error", writeErr: writeFailure,
			reloadErr: reloadFailure,
			wantErrors: []error{
				writeFailure,
				reloadFailure,
			},
			reads: 2, writes: 1,
		},
		{
			name: "reread cancellation", writeErr: writeFailure,
			reloadErr: context.Canceled,
			wantErrors: []error{
				writeFailure,
				context.Canceled,
			},
			reads: 2, writes: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, candidate := indexedRecoveryFixture(t)
			pkg := candidate.AncestryPackages[0]
			ark, err := psbtutil.Parse(pkg.ArkPsbt)
			require.NoError(t, err)
			checkpoint, err := psbtutil.Parse(
				pkg.CheckpointPsbts[0],
			)
			require.NoError(t, err)
			full := &db.OORPackageBundle{
				SessionID: ark.UnsignedTx.TxHash(),
				ArkPSBT:   ark,
				FinalCheckpointPSBTs: []*psbt.Packet{
					checkpoint,
				},
			}
			compactCheckpoint := compactRecoveryTestPacket(
				t, checkpoint,
			)
			incoming := &db.OORPackageBundle{
				SessionID: full.SessionID,
				ArkPSBT:   compactRecoveryTestPacket(t, ark),
				FinalCheckpointPSBTs: []*psbt.Packet{
					compactCheckpoint,
				},
			}
			store := &recoveryPackageStoreStub{
				readErr: test.readErr, writeErr: test.writeErr,
				reloadErr: test.reloadErr,
			}
			if test.existing {
				store.existing = full
			}
			if test.race {
				store.raced = full
			}
			if test.wrongWitness {
				input := &full.ArkPSBT.Inputs[0]
				input.TaprootKeySpendSig[0] ^= 1
			}
			err = persistIndexedRecoveryPackage(
				t.Context(), store, incoming,
			)
			if len(test.wantErrors) == 0 {
				require.NoError(t, err)
			}
			for _, want := range test.wantErrors {
				require.ErrorIs(t, err, want)
			}
			if test.wantMismatch {
				require.ErrorContains(
					t, err, "witness mismatch",
				)
			}
			require.Equal(t, test.reads, store.reads)
			require.Equal(t, test.writes, store.writes)
		})
	}
}
