package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
)

// AssetDepositClaimStatus is the lifecycle state of a deposit claim.
type AssetDepositClaimStatus string

const (
	// AssetDepositClaiming is a signed claim that tapd has not yet
	// recorded as confirmed.
	AssetDepositClaiming AssetDepositClaimStatus = "claiming"

	// AssetDepositClaimed is a claim tapd recorded as confirmed, so the
	// asset is back in the asset wallet.
	AssetDepositClaimed AssetDepositClaimStatus = "claimed"
)

// ErrAssetDepositClaimNotFound reports a deposit without a claim.
var ErrAssetDepositClaimNotFound = errors.New("asset deposit claim not found")

// AssetDepositClaim is the claim of one asset deposit.
type AssetDepositClaim struct {
	RequestID string
	ClaimTxid chainhash.Hash
	Status    AssetDepositClaimStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateAssetDepositClaim records a signed claim, or returns the stored claim
// when it names the same transaction.
func (s *AssetDepositStore) CreateAssetDepositClaim(ctx context.Context,
	requestID string, claimTxid chainhash.Hash) (*AssetDepositClaim,
	error) {

	var claim *AssetDepositClaim
	err := s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		_, err := q.InsertAssetDepositClaim(
			ctx, sqlc.InsertAssetDepositClaimParams{
				RequestID: requestID,
				ClaimTxid: claimTxid[:],
				CreatedAt: s.now().Unix(),
			},
		)
		if err != nil {
			return err
		}

		row, err := q.GetAssetDepositClaim(ctx, requestID)
		if err != nil {
			return err
		}
		claim, err = assetDepositClaimFromRow(row)
		if err != nil {
			return err
		}
		if claim.ClaimTxid != claimTxid {
			return fmt.Errorf("%w: %s is claimed by %s",
				ErrAssetDepositMismatch, requestID,
				claim.ClaimTxid)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return claim, nil
}

// GetAssetDepositClaim returns the claim of a deposit.
func (s *AssetDepositStore) GetAssetDepositClaim(ctx context.Context,
	requestID string) (*AssetDepositClaim, error) {

	var row sqlc.AssetDepositClaim
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		row, err = q.GetAssetDepositClaim(ctx, requestID)

		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAssetDepositClaimNotFound
	}
	if err != nil {
		return nil, err
	}

	return assetDepositClaimFromRow(row)
}

// ListAssetDepositClaimsByStatus returns the claims in one status.
func (s *AssetDepositStore) ListAssetDepositClaimsByStatus(ctx context.Context,
	status AssetDepositClaimStatus) ([]*AssetDepositClaim, error) {

	var rows []sqlc.AssetDepositClaim
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		var err error
		rows, err = q.ListAssetDepositClaimsByStatus(
			ctx, string(status),
		)

		return err
	})
	if err != nil {
		return nil, err
	}

	claims := make([]*AssetDepositClaim, 0, len(rows))
	for _, row := range rows {
		claim, err := assetDepositClaimFromRow(row)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}

	return claims, nil
}

// MarkAssetDepositClaimed records that tapd confirmed a claim. Marking a
// claimed deposit again is a no-op.
func (s *AssetDepositStore) MarkAssetDepositClaimed(ctx context.Context,
	requestID string) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		moved, err := q.MarkAssetDepositClaimed(
			ctx, sqlc.MarkAssetDepositClaimedParams{
				RequestID: requestID,
				UpdatedAt: s.now().Unix(),
			},
		)
		if err != nil || moved == 1 {
			return err
		}

		row, err := q.GetAssetDepositClaim(ctx, requestID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAssetDepositClaimNotFound
		}
		if err != nil {
			return err
		}
		if row.Status != string(AssetDepositClaimed) {
			return fmt.Errorf("%w: claim of %s is %s",
				ErrAssetDepositMismatch, requestID, row.Status)
		}

		return nil
	})
}

// assetDepositClaimFromRow decodes a stored claim.
func assetDepositClaimFromRow(row sqlc.AssetDepositClaim) (*AssetDepositClaim,
	error) {

	txid, err := chainhash.NewHash(row.ClaimTxid)
	if err != nil {
		return nil, fmt.Errorf("stored claim txid: %w", err)
	}

	return &AssetDepositClaim{
		RequestID: row.RequestID,
		ClaimTxid: *txid,
		Status:    AssetDepositClaimStatus(row.Status),
		CreatedAt: time.Unix(row.CreatedAt, 0),
		UpdatedAt: time.Unix(row.UpdatedAt, 0),
	}, nil
}
