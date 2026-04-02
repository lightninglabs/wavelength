package db

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

// ReceiveScriptVTXOMetadata is the standardized Ark VTXO descriptor metadata
// committed by a registered receive script.
type ReceiveScriptVTXOMetadata struct {
	// OwnerPubKey is the recipient pubkey committed to the receive script.
	OwnerPubKey *btcec.PublicKey

	// OperatorPubKey is the operator pubkey committed to the
	// receive script.
	OperatorPubKey *btcec.PublicKey

	// ExitDelay is the CSV delay committed to the receive script.
	ExitDelay uint32
}

// ResolveActiveReceiveScriptVTXOMetadataTx resolves the active standardized
// Ark VTXO metadata for pkScript using the caller's query context.
func ResolveActiveReceiveScriptVTXOMetadataTx(ctx context.Context,
	qtx *sqlc.Queries, pkScript []byte,
	expiresAtUnixS int64) (*ReceiveScriptVTXOMetadata, error) {

	if len(pkScript) == 0 {
		return nil, fmt.Errorf("pkScript must be provided")
	}

	rows, err := qtx.ListActiveIndexerReceivePrincipalsByScript(ctx,
		sqlc.ListActiveIndexerReceivePrincipalsByScriptParams{
			PkScript:       pkScript,
			ExpiresAtUnixS: expiresAtUnixS,
		},
	)
	if err != nil {
		return nil, err
	}

	var (
		resolved          *ReceiveScriptVTXOMetadata
		resolvedPrincipal string
	)
	for _, row := range rows {
		meta, err := metadataFromReceiveScriptRow(row)
		if err != nil {
			return nil, err
		}
		if meta == nil {
			continue
		}

		if resolved == nil {
			resolved = meta
			resolvedPrincipal = row.PrincipalMailboxID
			continue
		}

		if !resolved.OwnerPubKey.IsEqual(meta.OwnerPubKey) ||
			!resolved.OperatorPubKey.IsEqual(meta.OperatorPubKey) ||
			resolved.ExitDelay != meta.ExitDelay {

			return nil, fmt.Errorf(
				"conflicting receive script metadata "+
					"for pkScript between %s and %s",
				resolvedPrincipal, row.PrincipalMailboxID,
			)
		}
	}

	return resolved, nil
}

// metadataFromReceiveScriptRow parses standardized Ark VTXO descriptor
// metadata from a receive-script registration row.
//
// A nil result means the registration is for a generic script and does not
// carry Ark-specific descriptor metadata.
func metadataFromReceiveScriptRow(row sqlc.IndexerReceiveScript) (
	*ReceiveScriptVTXOMetadata, error) {

	hasMetadata := len(row.OwnerPubkey) > 0 ||
		len(row.OperatorPubkey) > 0 ||
		row.ExitDelay.Valid
	if !hasMetadata {
		return nil, nil
	}

	if len(row.OwnerPubkey) == 0 || len(row.OperatorPubkey) == 0 ||
		!row.ExitDelay.Valid {

		return nil, fmt.Errorf("incomplete receive script metadata")
	}

	ownerKey, err := btcec.ParsePubKey(row.OwnerPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse owner pubkey: %w", err)
	}

	operatorKey, err := btcec.ParsePubKey(row.OperatorPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse operator pubkey: %w", err)
	}

	if row.ExitDelay.Int64 <= 0 {
		return nil, fmt.Errorf("invalid exit delay %d",
			row.ExitDelay.Int64)
	}
	if row.ExitDelay.Int64 > math.MaxUint32 {
		return nil, fmt.Errorf(
			"exit delay out of range: %d", row.ExitDelay.Int64,
		)
	}

	return &ReceiveScriptVTXOMetadata{
		OwnerPubKey:    ownerKey,
		OperatorPubKey: operatorKey,
		ExitDelay:      uint32(row.ExitDelay.Int64),
	}, nil
}

// enrichRecordDescriptorMetadataTx upgrades a generic live record into a
// fully described Ark VTXO when the pkScript matches an active registered
// receive script with standardized descriptor metadata.
//
// The lookup runs on the caller's query context so callers can compose the
// enrichment step into a larger atomic transaction.
func enrichRecordDescriptorMetadataTx(ctx context.Context,
	qtx *sqlc.Queries, record *vtxo.Record, expiresAtUnixS int64,
	operatorKey keychain.KeyDescriptor) (*vtxo.Record, error) {

	enriched := *record
	hasDescriptorMetadata := enriched.OwnerKey != nil ||
		enriched.OperatorKeyDesc != nil ||
		enriched.ExitDelay != 0
	if hasDescriptorMetadata {
		if err := vtxo.ValidateDescriptorMetadata(
			&enriched,
		); err != nil {
			return nil, err
		}

		return &enriched, nil
	}

	meta, err := ResolveActiveReceiveScriptVTXOMetadataTx(
		ctx, qtx, enriched.PkScript, expiresAtUnixS,
	)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return &enriched, nil
	}

	if operatorKey.PubKey == nil {
		return nil, fmt.Errorf("operator key must be provided")
	}
	if !operatorKey.PubKey.IsEqual(meta.OperatorPubKey) {
		return nil, fmt.Errorf(
			"recipient metadata operator key mismatch",
		)
	}

	operatorKeyCopy := operatorKey
	enriched.OwnerKey = meta.OwnerPubKey
	enriched.OperatorKeyDesc = &operatorKeyCopy
	enriched.ExitDelay = meta.ExitDelay

	if err := vtxo.ValidateDescriptorMetadata(&enriched); err != nil {
		return nil, err
	}

	return &enriched, nil
}
