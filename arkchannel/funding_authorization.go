package arkchannel

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// FundingAuthorizationStore claims the first backing at a validated intent
// revision. Ordinary FSM updates must never overwrite this local evidence.
type FundingAuthorizationStore interface {
	BindFundingTxID(context.Context, Terms, uint64, chainhash.Hash) error

	RecordFundingSignature(context.Context, Terms, uint64, chainhash.Hash,
		[]byte) error
}

// SignAuthorizedBacking releases only a committed signature. If concurrent
// signers chose different valid nonces, all callers receive the durable winner.
func (s *Service) SignAuthorizedBacking(ctx context.Context, terms Terms,
	txid chainhash.Hash, sign func() ([]byte, error)) ([]byte, error) {

	store, ok := s.coordinator.store.(FundingAuthorizationStore)
	if !ok {
		return nil, fmt.Errorf("funding authorization store is " +
			"required")
	}
	var candidate []byte
	for range maxCASAttempts {
		record, err := s.GetChannel(ctx, terms.ID)
		if err != nil {
			return nil, err
		}
		if record.Snapshot.Phase != PhaseNegotiating ||
			record.Snapshot.Terms != terms ||
			record.AuthorizedBackingTxID == nil ||
			*record.AuthorizedBackingTxID != txid {
			return nil, fmt.Errorf("backing is not authorized " +
				"for signing")
		}
		if record.FundingSignature != nil {
			return record.FundingSignature, nil
		}
		if candidate == nil {
			candidate, err = sign()
			if err != nil {
				return nil, err
			}
		}
		err = store.RecordFundingSignature(
			ctx, terms, record.Revision, txid, candidate,
		)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}

		return candidate, nil
	}

	return nil, ErrConflict
}

// AuthorizeFundingBacking checks the canonical unsigned backing. A nil
// verifyReservation means installation: it can consume but never create a
// signing authorization. A matching durable claim survives loss of the lnd
// wallet's in-memory reservation.
func (s *Service) AuthorizeFundingBacking(ctx context.Context, terms Terms,
	source VTXOBinding, point wire.OutPoint,
	verifyReservation func() error) error {

	if point.Index != 0 || point.Hash == (chainhash.Hash{}) {
		return fmt.Errorf("invalid funding authorization point")
	}
	for range maxCASAttempts {
		record, err := s.GetChannel(ctx, terms.ID)
		if err != nil {
			return err
		}
		snap := record.Snapshot
		if snap.Terms != terms || snap.Source == nil ||
			!bindingsEqual(*snap.Source, source) ||
			!bytes.Equal(
				snap.Source.ArkTransaction,
				source.ArkTransaction,
			) {
			return fmt.Errorf("funding authorization does not " +
				"match channel intent")
		}
		if snap.Backing != nil && snap.Backing.ChannelPoint != point {
			return fmt.Errorf("funding backing does not match " +
				"channel FSM")
		}
		if snap.Phase != PhaseNegotiating {
			return fmt.Errorf("cannot authorize funding in %s",
				snap.Phase)
		}
		if bound := record.AuthorizedBackingTxID; bound != nil {
			if *bound != point.Hash {
				return fmt.Errorf("funding reservation " +
					"already authorizes a different " +
					"backing")
			}
			if verifyReservation == nil &&
				record.FundingSignature == nil {
				return fmt.Errorf("funding authorization has " +
					"no released signature")
			}

			return nil
		}
		if verifyReservation == nil {
			return fmt.Errorf("funding backing has no signing " +
				"authorization")
		}
		if err := verifyReservation(); err != nil {
			return err
		}
		store, ok := s.coordinator.store.(FundingAuthorizationStore)
		if !ok {
			return fmt.Errorf("funding authorization store is " +
				"required")
		}
		err = store.BindFundingTxID(
			ctx, terms, record.Revision, point.Hash,
		)
		if errors.Is(err, ErrConflict) {
			continue
		}

		return err
	}

	return ErrConflict
}
