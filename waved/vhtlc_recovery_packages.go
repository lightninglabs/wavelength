package waved

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/db"
	oorlib "github.com/lightninglabs/wavelength/lib/tx/oor"
)

// recoveryPackagePersistence bounds import reconciliation to immutable
// package reads and writes, including a concurrent wallet event replay.
type recoveryPackagePersistence interface {
	GetPackage(context.Context,
		chainhash.Hash) (*db.OORPackageBundle, error)

	UpsertPackage(context.Context, db.OORPackageDirection, chainhash.Hash,
		*psbt.Packet, []*psbt.Packet) error
}

// persistIndexedRecoveryPackage imports an already authenticated ancestry
// package without rewriting full local signing metadata with a compact indexer
// proof. The generic artifact-store upsert remains byte-exact and immutable.
func persistIndexedRecoveryPackage(ctx context.Context,
	store recoveryPackagePersistence, pkg *db.OORPackageBundle) error {

	existing, err := store.GetPackage(ctx, pkg.SessionID)
	if err == nil {
		return matchRecoveryPackageTransactions(existing, pkg)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	err = store.UpsertPackage(
		ctx, db.OORPackageDirectionIncoming, pkg.SessionID, pkg.ArkPSBT,
		pkg.FinalCheckpointPSBTs,
	)
	if err == nil {
		return nil
	}

	// Wallet event replay can import the full package after the lookup.
	// Only an independently loaded, witness-identical durable row makes
	// that raced insert unnecessary; an arbitrary write error is not
	// success.
	existing, readErr := store.GetPackage(ctx, pkg.SessionID)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	matchErr := matchRecoveryPackageTransactions(existing, pkg)
	if matchErr != nil {
		return errors.Join(err, matchErr)
	}

	return nil
}

// matchRecoveryPackageTransactions compares the exact signed transaction graph,
// including witness bytes, rather than just txids or PSBT metadata. Its caller
// has already authenticated the incoming proof against the funded target and
// local chain; this check only decides whether a durable package can be reused.
func matchRecoveryPackageTransactions(
	existing, incoming *db.OORPackageBundle) error {

	if existing == nil || incoming == nil ||
		existing.SessionID != incoming.SessionID {
		return fmt.Errorf("recovery package session mismatch")
	}
	if existing.ArkPSBT == nil || existing.ArkPSBT.UnsignedTx == nil ||
		incoming.ArkPSBT == nil || incoming.ArkPSBT.UnsignedTx == nil {
		return fmt.Errorf("recovery Ark transaction missing")
	}
	if existing.ArkPSBT.UnsignedTx.TxHash() != existing.SessionID ||
		incoming.ArkPSBT.UnsignedTx.TxHash() != incoming.SessionID {
		return fmt.Errorf("recovery Ark session does not match " +
			"transaction")
	}
	if len(existing.FinalCheckpointPSBTs) == 0 ||
		len(existing.FinalCheckpointPSBTs) != len(
			incoming.FinalCheckpointPSBTs,
		) {
		return fmt.Errorf("recovery checkpoint count mismatch")
	}

	if err := matchRecoverySignedTransaction(
		existing.ArkPSBT, incoming.ArkPSBT,
	); err != nil {
		return fmt.Errorf("recovery Ark transaction: %w", err)
	}
	for i := range incoming.FinalCheckpointPSBTs {
		err := matchRecoverySignedTransaction(
			existing.FinalCheckpointPSBTs[i],
			incoming.FinalCheckpointPSBTs[i],
		)
		if err != nil {
			return fmt.Errorf("recovery checkpoint %d: %w", i, err)
		}
	}

	return nil
}

// matchRecoverySignedTransaction rejects witness changes that a txid-only
// comparison would miss while tolerating discarded PSBT signing metadata.
func matchRecoverySignedTransaction(existing, incoming *psbt.Packet) error {
	local, err := recoverySignedTransaction(existing)
	if err != nil {
		return fmt.Errorf("local signed transaction: %w", err)
	}
	indexed, err := recoverySignedTransaction(incoming)
	if err != nil {
		return fmt.Errorf("indexed signed transaction: %w", err)
	}
	if !bytes.Equal(local, indexed) {
		return fmt.Errorf("signed transaction or witness mismatch")
	}

	return nil
}

// recoverySignedTransaction reconstructs the Taproot OOR transaction without
// modifying its PSBT. Both full signing records and compact finalized proofs
// use the same witness builder as OOR validation and indexer compaction.
func recoverySignedTransaction(packet *psbt.Packet) ([]byte, error) {
	if packet == nil || packet.UnsignedTx == nil ||
		len(packet.Inputs) == 0 ||
		len(packet.Inputs) != len(packet.UnsignedTx.TxIn) {
		return nil, fmt.Errorf("recovery transaction inputs missing")
	}
	for i, input := range packet.UnsignedTx.TxIn {
		if input == nil || len(input.SignatureScript) != 0 ||
			len(packet.Inputs[i].FinalScriptSig) != 0 {
			return nil, fmt.Errorf("invalid Taproot recovery "+
				"input %d", i)
		}
	}
	for i, output := range packet.UnsignedTx.TxOut {
		if output == nil {
			return nil, fmt.Errorf("recovery output %d missing", i)
		}
	}

	tx := packet.UnsignedTx.Copy()
	for i, input := range packet.Inputs {
		witness, err := oorlib.BuildTaprootWitness(input)
		if err != nil {
			return nil, fmt.Errorf("recovery input %d witness: %w",
				i, err)
		}
		if len(witness) == 0 {
			return nil, fmt.Errorf("recovery input %d "+
				"witness missing", i)
		}
		tx.TxIn[i].Witness = witness
	}
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		return nil, err
	}

	return encoded.Bytes(), nil
}
