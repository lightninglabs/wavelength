package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/vtxo"
)

// encodeTaprootAssetMetadata rejects partial identities and binds asset state
// to the composed output before it enters the durable wallet inventory.
func encodeTaprootAssetMetadata(desc *vtxo.Descriptor) (sql.NullString, []byte,
	error) {

	if desc == nil {
		return sql.NullString{}, nil, fmt.Errorf("descriptor must be " +
			"provided")
	}
	if desc.TaprootAssetRoot == nil {
		if desc.TaprootAssetRef != "" || desc.TaprootAssetAmount != 0 ||
			len(desc.TaprootAssetSealedPackage) != 0 {
			return sql.NullString{}, nil, fmt.Errorf("Taproot " +
				"Asset metadata requires a commitment root")
		}

		return sql.NullString{}, nil, nil
	}
	if desc.TaprootAssetRef == "" || desc.TaprootAssetAmount == 0 {
		return sql.NullString{}, nil, fmt.Errorf("Taproot Asset ref " +
			"and positive amount must both be provided")
	}
	if len(desc.TaprootAssetRef) > vtxo.MaxTaprootAssetRefBytes {
		return sql.NullString{}, nil, fmt.Errorf("Taproot Asset ref "+
			"exceeds %d bytes", vtxo.MaxTaprootAssetRefBytes)
	}
	script, err := desc.EffectivePkScript()
	if err != nil {
		return sql.NullString{}, nil, err
	}
	if !bytes.Equal(script, desc.PkScript) {
		return sql.NullString{}, nil, fmt.Errorf("Taproot Asset " +
			"composed output script mismatch")
	}
	amount := make([]byte, 8)
	binary.BigEndian.PutUint64(amount, desc.TaprootAssetAmount)

	return sql.NullString{
		String: desc.TaprootAssetRef,
		Valid:  true,
	}, amount, nil
}

// decodeAssetVTXOState validates persisted asset state on every load, including
// cache hits, so malformed state can never become an ordinary Bitcoin VTXO.
func decodeAssetVTXOState(row VTXORow) (*vtxo.Descriptor, error) {
	desc := &vtxo.Descriptor{
		PolicyTemplate:  row.PolicyTemplate,
		PkScript:        row.PkScript,
		TaprootAssetRef: row.TaprootAssetRef.String,
		TaprootAssetSealedPackage: bytes.Clone(
			row.TaprootAssetSealedPackage,
		),
	}
	if row.TaprootAssetRoot != nil {
		if len(row.TaprootAssetRoot) != chainhash.HashSize {
			return nil, fmt.Errorf("invalid Taproot Asset root "+
				"length: %d", len(row.TaprootAssetRoot))
		}
		root := chainhash.Hash{}
		copy(root[:], row.TaprootAssetRoot)
		desc.TaprootAssetRoot = &root
	}
	if row.TaprootAssetAmount != nil {
		if len(row.TaprootAssetAmount) != 8 {
			return nil, fmt.Errorf("invalid Taproot Asset amount "+
				"length: %d", len(row.TaprootAssetAmount))
		}
		desc.TaprootAssetAmount = binary.BigEndian.Uint64(
			row.TaprootAssetAmount,
		)
	}
	hasAsset := desc.TaprootAssetRoot != nil
	if row.TaprootAssetRef.Valid != hasAsset ||
		(row.TaprootAssetAmount != nil) != hasAsset {
		return nil, fmt.Errorf("incomplete Taproot Asset metadata")
	}
	if _, _, err := encodeTaprootAssetMetadata(desc); err != nil {
		return nil, err
	}

	return desc, nil
}

// validateAssetVTXOReplay keeps an outpoint's asset identity immutable while
// allowing a minimal round row to be enriched with its verified leaf state.
func validateAssetVTXOReplay(ctx context.Context, q RoundStore,
	desc *vtxo.Descriptor) error {

	row, err := q.GetVTXO(ctx, sqlc.GetVTXOParams{
		OutpointHash:  desc.Outpoint.Hash[:],
		OutpointIndex: int32(desc.Outpoint.Index),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.TaprootAssetRoot == nil && desc.TaprootAssetRoot == nil {
		return nil
	}
	old, err := decodeAssetVTXOState(row)
	if err != nil {
		return err
	}
	if len(desc.PkScript) != 0 &&
		!bytes.Equal(desc.PkScript, row.PkScript) {
		return fmt.Errorf("asset VTXO replay changes output script")
	}
	if len(desc.PolicyTemplate) != 0 &&
		!bytes.Equal(desc.PolicyTemplate, row.PolicyTemplate) {
		return fmt.Errorf("asset VTXO replay changes policy template")
	}
	if old.TaprootAssetRoot == nil || desc.TaprootAssetRoot == nil {
		return nil
	}
	if *old.TaprootAssetRoot != *desc.TaprootAssetRoot ||
		old.TaprootAssetRef != desc.TaprootAssetRef ||
		old.TaprootAssetAmount != desc.TaprootAssetAmount {
		return fmt.Errorf("asset VTXO replay changes asset identity")
	}
	if len(old.TaprootAssetSealedPackage) != 0 &&
		len(desc.TaprootAssetSealedPackage) != 0 &&
		!bytes.Equal(
			old.TaprootAssetSealedPackage,
			desc.TaprootAssetSealedPackage,
		) {
		return fmt.Errorf("asset VTXO replay changes sealed package")
	}

	return nil
}
