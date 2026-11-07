package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
)

// RoundBoarding returns the round boarding input of a confirmed deposit.
func (d *AssetDeposit) RoundBoarding() (*round.AssetBoarding, error) {
	if d.Status != AssetDepositConfirmed || d.Output == nil {
		return nil, fmt.Errorf("asset deposit %s is not confirmed",
			d.Request.RequestID)
	}
	output := d.Output

	return &round.AssetBoarding{
		Outpoint:           output.Outpoint,
		Value:              btcutil.Amount(output.ValueSat),
		PkScript:           output.PkScript,
		OwnerKey:           output.OwnerKey,
		OperatorKey:        d.Request.OperatorKey,
		ExitDelay:          d.Request.ExitDelay,
		ConfHeight:         int32(d.ConfirmedHeight),
		AssetRef:           d.Request.AssetRef,
		AssetAmount:        d.Request.AssetAmount,
		AssetDigest:        output.Digest,
		AssetProof:         d.ProofFile,
		CommitmentLeafHash: output.TaprootAssetRoot,
		AssetWitness:       output.OPTrueWitness,
	}, nil
}

// AssetDepositRound returns the ID of the round that spends a deposit, or an
// empty ID when no round that has not failed took it.
func (s *AssetDepositStore) AssetDepositRound(ctx context.Context,
	requestID string) (string, error) {

	var roundID string
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetAssetDepositRound(ctx, requestID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		roundID = row.RoundID

		return nil
	})

	return roundID, err
}

// insertRoundAssetDeposit links the deposit an asset boarding intent spends
// to a checkpointed round, with the client's signature of its input.
func insertRoundAssetDeposit(ctx context.Context, q RoundStore, roundID string,
	intent *round.BoardingIntent,
	inputSig *types.BoardingInputSignature) error {

	if inputSig == nil || inputSig.ClientSignature == nil ||
		inputSig.Outpoint != intent.Outpoint {
		return fmt.Errorf("asset deposit %s has no matching input "+
			"signature", intent.Outpoint)
	}

	deposit, err := q.GetAssetDepositByOutpoint(
		ctx, sqlc.GetAssetDepositByOutpointParams{
			OutpointHash: intent.Outpoint.Hash[:],
			OutpointIndex: sql.NullInt32{
				Int32: int32(intent.Outpoint.Index),
				Valid: true,
			},
		},
	)
	if err != nil {
		return fmt.Errorf("asset deposit %s: %w", intent.Outpoint, err)
	}

	return q.InsertRoundAssetDeposit(
		ctx, sqlc.InsertRoundAssetDepositParams{
			RoundID:        roundID,
			RequestID:      deposit.RequestID,
			InputIndex:     int32(inputSig.InputIndex),
			InputSignature: inputSig.ClientSignature.Serialize(),
		},
	)
}

// roundAssetDepositIntents restores the asset deposits a checkpointed round
// spends, with the client's signature of each input.
func (s *RoundPersistenceStore) roundAssetDepositIntents(ctx context.Context,
	q RoundStore, roundID string) ([]round.BoardingIntent,
	[]*types.BoardingInputSignature, error) {

	rows, err := q.GetRoundAssetDeposits(ctx, roundID)
	if err != nil {
		return nil, nil, fmt.Errorf("get round asset deposits: %w", err)
	}

	intents := make([]round.BoardingIntent, 0, len(rows))
	sigs := make([]*types.BoardingInputSignature, 0, len(rows))
	for _, row := range rows {
		depositRow, err := q.GetAssetDeposit(ctx, row.RequestID)
		if err != nil {
			return nil, nil, fmt.Errorf("get asset deposit %s: %w",
				row.RequestID, err)
		}
		deposit, err := assetDepositFromRow(depositRow)
		if err != nil {
			return nil, nil, err
		}
		boarding, err := deposit.RoundBoarding()
		if err != nil {
			return nil, nil, err
		}
		intent, err := round.NewAssetBoardingIntent(
			boarding, s.chainParams,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("asset deposit %s: %w",
				row.RequestID, err)
		}
		intent.Status = round.BoardingStatusAdopted

		sig, err := schnorr.ParseSignature(row.InputSignature)
		if err != nil {
			return nil, nil, fmt.Errorf("parse asset deposit "+
				"signature: %w", err)
		}

		intents = append(intents, intent)
		sigs = append(sigs, &types.BoardingInputSignature{
			InputIndex:      int(row.InputIndex),
			Outpoint:        intent.Outpoint,
			ClientSignature: sig,
		})
	}

	return intents, sigs, nil
}
