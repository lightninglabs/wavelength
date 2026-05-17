//nolint:ll
package db

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// Type aliases for sqlc generated types.
type (
	RoundRow                  = sqlc.Round
	RoundBoardingIntentRow    = sqlc.RoundBoardingIntent
	RoundClientTreeRow        = sqlc.RoundClientTree
	RoundEffectRow            = sqlc.ClientRoundEffect
	RoundAggNonceRow          = sqlc.ClientRoundAggNonceState
	RoundForfeitRequestRow    = sqlc.ClientRoundForfeitRequestState
	RoundForfeitSigRow        = sqlc.ClientRoundForfeitSigState
	RoundPendingLeaveQuoteRow = sqlc.ClientRoundPendingLeaveQuote
	RoundPendingQuoteRow      = sqlc.ClientRoundPendingQuote
	RoundPendingVTXOQuoteRow  = sqlc.ClientRoundPendingVtxoQuote
	RoundPartialSigRow        = sqlc.ClientRoundPartialSigState
	RoundSigningNonceRow      = sqlc.ClientRoundNonceState
	RoundVtxoRequestRow       = sqlc.RoundVtxoRequest
	VTXORow                   = sqlc.Vtxo
	InsertRoundParams         = sqlc.InsertRoundParams
	InsertVTXOParams          = sqlc.InsertVTXOParams
	ListRoundsPaginatedParams = sqlc.ListRoundsPaginatedParams

	// ClearPendingBoardParams aliases the sqlc-generated
	// clear-by-outpoint params so call sites can spell the type
	// concisely (the generated name exceeds the line-length cap
	// when nested inside the CommitState transaction body).
	ClearPendingBoardParams = sqlc.ClearPendingBoardRequestByOutpointParams
)

// ListRoundsQuery controls persisted round pagination and filtering.
type ListRoundsQuery struct {
	// Cursor is the last round id from the previous page. Use an empty
	// string to start from the first matching row.
	Cursor string

	// Limit is the maximum number of matching persisted rounds to return.
	Limit int32

	// Status restricts results to one persisted round status when set.
	Status string

	// CreatedAfter restricts results to rounds created at or after this
	// Unix timestamp when non-zero.
	CreatedAfter int64

	// CreatedBefore restricts results to rounds created at or before this
	// Unix timestamp when non-zero.
	CreatedBefore int64
}

// RoundStore is the interface that groups all round-related database queries.
// This is a subset of sqlc.Querier focused on round operations.
//
//nolint:interfacebloat
type RoundStore interface {
	InsertRound(ctx context.Context, arg InsertRoundParams) error

	GetRound(ctx context.Context, roundID string) (RoundRow, error)

	GetRoundByCommitmentTxid(ctx context.Context,
		txid []byte) (RoundRow, error)

	ListActiveRounds(ctx context.Context) ([]RoundRow, error)

	ListRoundsByStatus(ctx context.Context,
		status string) ([]RoundRow, error)

	UpdateRoundStatus(
		ctx context.Context, arg sqlc.UpdateRoundStatusParams,
	) error

	FinalizeRound(ctx context.Context, arg sqlc.FinalizeRoundParams) error

	InsertRoundBoardingIntent(ctx context.Context,
		arg sqlc.InsertRoundBoardingIntentParams) error

	GetRoundBoardingIntents(ctx context.Context,
		roundID string) ([]RoundBoardingIntentRow, error)

	InsertRoundVtxoRequest(ctx context.Context,
		arg sqlc.InsertRoundVtxoRequestParams) error

	GetRoundVtxoRequests(ctx context.Context,
		roundID string) ([]RoundVtxoRequestRow, error)

	InsertRoundClientTree(ctx context.Context,
		arg sqlc.InsertRoundClientTreeParams) error

	GetRoundClientTrees(ctx context.Context,
		roundID string) ([]RoundClientTreeRow, error)

	InsertClientTreeTxid(ctx context.Context,
		arg sqlc.InsertClientTreeTxidParams) error

	InsertClientRoundNonceState(ctx context.Context,
		arg sqlc.InsertClientRoundNonceStateParams) error

	GetClientRoundNonceState(ctx context.Context,
		roundID string) ([]RoundSigningNonceRow, error)

	InsertClientRoundAggNonceState(ctx context.Context,
		arg sqlc.InsertClientRoundAggNonceStateParams) error

	GetClientRoundAggNonceState(ctx context.Context,
		roundID string) ([]RoundAggNonceRow, error)

	InsertClientRoundPartialSigState(ctx context.Context,
		arg sqlc.InsertClientRoundPartialSigStateParams) error

	GetClientRoundPartialSigState(ctx context.Context,
		roundID string) ([]RoundPartialSigRow, error)

	InsertClientRoundForfeitSigState(ctx context.Context,
		arg sqlc.InsertClientRoundForfeitSigStateParams) error

	GetClientRoundForfeitSigState(ctx context.Context,
		roundID string) ([]RoundForfeitSigRow, error)

	InsertClientRoundForfeitRequestState(ctx context.Context,
		arg sqlc.InsertClientRoundForfeitRequestStateParams) error

	GetClientRoundForfeitRequestState(ctx context.Context,
		roundID string) ([]RoundForfeitRequestRow, error)

	UpsertClientRoundPendingQuote(ctx context.Context,
		arg sqlc.UpsertClientRoundPendingQuoteParams) error

	DeleteClientRoundPendingVTXOQuotes(ctx context.Context,
		roundID string) error

	InsertClientRoundPendingVTXOQuote(ctx context.Context,
		arg sqlc.InsertClientRoundPendingVTXOQuoteParams) error

	DeleteClientRoundPendingLeaveQuotes(ctx context.Context,
		roundID string) error

	InsertClientRoundPendingLeaveQuote(ctx context.Context,
		arg sqlc.InsertClientRoundPendingLeaveQuoteParams) error

	ListClientRoundPendingQuotes(ctx context.Context) (
		[]RoundPendingQuoteRow, error)

	GetClientRoundPendingVTXOQuotes(ctx context.Context,
		roundID string) ([]RoundPendingVTXOQuoteRow, error)

	GetClientRoundPendingLeaveQuotes(ctx context.Context,
		roundID string) ([]RoundPendingLeaveQuoteRow, error)

	DeleteClientRoundPendingQuote(ctx context.Context, roundID string) error

	InsertClientRoundEffect(ctx context.Context,
		arg sqlc.InsertClientRoundEffectParams) error

	ListDueClientRoundEffectIDs(ctx context.Context,
		arg sqlc.ListDueClientRoundEffectIDsParams) ([]string, error)

	ClaimClientRoundEffect(ctx context.Context,
		arg sqlc.ClaimClientRoundEffectParams) (RoundEffectRow, error)

	MarkClientRoundEffectDone(ctx context.Context,
		arg sqlc.MarkClientRoundEffectDoneParams) error

	ReleaseClientRoundEffectForRetry(ctx context.Context,
		arg sqlc.ReleaseClientRoundEffectForRetryParams) error

	ReleaseExpiredClientRoundEffectClaims(ctx context.Context,
		arg sqlc.ReleaseExpiredClientRoundEffectClaimsParams) error

	GetClientTreeByTxid(ctx context.Context,
		txid []byte) (RoundClientTreeRow, error)

	InsertVTXO(ctx context.Context, arg InsertVTXOParams) error

	GetVTXO(ctx context.Context, arg sqlc.GetVTXOParams) (VTXORow, error)

	ListUnspentVTXOs(ctx context.Context) ([]VTXORow, error)

	MarkVTXOSpent(ctx context.Context, arg sqlc.MarkVTXOSpentParams) error

	// VTXO lifecycle status queries.
	ListLiveVTXOs(ctx context.Context) ([]VTXORow, error)

	ListVTXOsByStatus(ctx context.Context, status int32) ([]VTXORow, error)

	UpdateVTXOStatus(
		ctx context.Context, arg sqlc.UpdateVTXOStatusParams,
	) error

	MarkVTXOForfeiting(
		ctx context.Context, arg sqlc.MarkVTXOForfeitingParams,
	) error

	GetVTXOForfeitTx(ctx context.Context,
		arg sqlc.GetVTXOForfeitTxParams) (
		sqlc.GetVTXOForfeitTxRow,
		error,
	)

	MarkVTXOForfeited(
		ctx context.Context, arg sqlc.MarkVTXOForfeitedParams,
	) error

	DeleteVTXO(ctx context.Context, arg sqlc.DeleteVTXOParams) error

	// Per-VTXO ancestry-paths side table (multi-tree ancestry for OOR).
	InsertVTXOAncestryPath(ctx context.Context,
		arg sqlc.InsertVTXOAncestryPathParams) error

	DeleteVTXOAncestryPaths(ctx context.Context,
		arg sqlc.DeleteVTXOAncestryPathsParams) error

	ListVTXOAncestryPaths(ctx context.Context,
		arg sqlc.ListVTXOAncestryPathsParams) (
		[]sqlc.VtxoAncestryPath,
		error,
	)

	// Batched ancestry queries used by the list paths to avoid an
	// N+1 ListVTXOAncestryPaths call per VTXO row.
	ListLiveVTXOAncestryPaths(ctx context.Context) (
		[]sqlc.VtxoAncestryPath,
		error,
	)

	ListVTXOAncestryPathsByStatus(ctx context.Context,
		status int32) ([]sqlc.VtxoAncestryPath, error)

	ListUnspentVTXOAncestryPaths(ctx context.Context) (
		[]sqlc.VtxoAncestryPath, error)

	// Include BoardingStore methods for fetching boarding intent details.
	GetBoardingIntent(ctx context.Context,
		arg BoardingIntentKey) (BoardingIntentRow, error)

	GetBoardingAddress(ctx context.Context,
		pkScript []byte) (BoardingAddrRow, error)

	UpdateBoardingIntentStatus(ctx context.Context,
		arg sqlc.UpdateBoardingIntentStatusParams) error

	// ClearPendingBoardRequestByOutpoint deletes the pending Board RPC
	// row bound to one outpoint. Called from CommitState in the same
	// transaction that marks the matching intent Adopted, so a stale
	// pending row can never rebind to an unrelated future boarding
	// deposit.
	ClearPendingBoardRequestByOutpoint(ctx context.Context,
		arg sqlc.ClearPendingBoardRequestByOutpointParams) error

	ListRoundsPaginated(ctx context.Context,
		arg ListRoundsPaginatedParams) ([]RoundRow, error)

	ListVTXOsByRound(ctx context.Context, roundID string) ([]VTXORow, error)
}

// BatchedRoundStore combines RoundStore with transaction support via the
// BatchedTx generic interface. This enables atomic operations across multiple
// queries.
type BatchedRoundStore interface {
	RoundStore
	BatchedTx[RoundStore]
}

// RoundPersistenceStore implements the round.RoundStore and round.VTXOStore
// interfaces using the BatchedTx pattern for transaction-safe operations.
type RoundPersistenceStore struct {
	db          BatchedRoundStore
	chainParams *chaincfg.Params
	clock       clock.Clock
}

// NewRoundPersistenceStore creates a new round persistence store using the
// transaction executor pattern.
func NewRoundPersistenceStore(
	db BatchedRoundStore, chainParams *chaincfg.Params, clk clock.Clock,
) *RoundPersistenceStore {

	return &RoundPersistenceStore{
		db:          db,
		chainParams: chainParams,
		clock:       clk,
	}
}

// SaveSigningNonces persists the round snapshot and local MuSig2 nonce
// material before public nonces are emitted to the server.
func (s *RoundPersistenceStore) SaveSigningNonces(ctx context.Context,
	r *round.Round, clientTrees map[round.SignerKey]*tree.Tree,
	nonces []round.SigningNonceState) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		commitmentTxBytes, commitmentTxid, err := serializeCommitmentTx(
			r.CommitmentTx,
		)
		if err != nil {
			return err
		}

		vtxtTreeBytes, err := serializeVTXOTreePaths(r.VTXOTreePaths)
		if err != nil {
			return err
		}

		nowUnix := s.clock.Now().Unix()
		err = q.InsertRound(ctx, InsertRoundParams{
			RoundID:               r.RoundID.String(),
			ConfirmationHeight:    sql.NullInt32{},
			ConfirmationBlockHash: nil,
			CommitmentTx:          commitmentTxBytes,
			CommitmentTxid:        commitmentTxid,
			VtxtTree:              vtxtTreeBytes,
			Status:                "nonces_generated",
			CreationTime:          nowUnix,
			LastUpdateTime:        nowUnix,
			StartHeight:           int32(r.StartHeight),
		})
		if err != nil {
			return fmt.Errorf("insert round nonce snapshot: %w",
				err)
		}

		for i := range r.Intents.Boarding {
			intent := r.Intents.Boarding[i]
			iParams, err := s.domainIntentToRoundParams(
				r.RoundID.String(), &intent, nil,
			)
			if err != nil {
				return fmt.Errorf("convert boarding intent: %w",
					err)
			}

			err = q.InsertRoundBoardingIntent(ctx, iParams)
			if err != nil {
				return fmt.Errorf("insert boarding intent "+
					"nonce snapshot: %w", err)
			}
		}

		for i, vtxoReq := range r.Intents.VTXOs {
			reqParams, err := vtxoRequestToRoundParams(
				r.RoundID.String(), i, &vtxoReq,
			)
			if err != nil {
				return fmt.Errorf("convert vtxo request: %w",
					err)
			}

			err = q.InsertRoundVtxoRequest(ctx, reqParams)
			if err != nil {
				return fmt.Errorf("insert vtxo request: %w",
					err)
			}
		}

		for key, clientTree := range clientTrees {
			treeData, err := SerializeTree(clientTree)
			if err != nil {
				return fmt.Errorf("serialize client tree: %w",
					err)
			}

			treeParams := sqlc.InsertRoundClientTreeParams{
				RoundID:   r.RoundID.String(),
				ClientKey: key[:],
				TreeData:  treeData,
			}
			err = q.InsertRoundClientTree(ctx, treeParams)
			if err != nil {
				return fmt.Errorf("insert client tree: %w", err)
			}

			txidEntries, err := clientTree.ExtractTxids()
			if err != nil {
				return fmt.Errorf("extract client tree "+
					"txids: %w", err)
			}

			for _, entry := range txidEntries {
				p := sqlc.InsertClientTreeTxidParams{
					Txid:        entry.Txid[:],
					RoundID:     r.RoundID.String(),
					ClientKey:   key[:],
					TreeLevel:   int32(entry.TreeLevel),
					OutputIndex: int32(entry.OutputIndex),
				}
				err = q.InsertClientTreeTxid(ctx, p)
				if err != nil {
					return fmt.Errorf("insert client tree "+
						"txid: %w", err)
				}
			}
		}

		for _, nonce := range nonces {
			err := q.InsertClientRoundNonceState(
				ctx, sqlc.InsertClientRoundNonceStateParams{
					RoundID:        r.RoundID.String(),
					SigningKey:     nonce.SignerKey[:],
					Txid:           nonce.TxID[:],
					PubNonce:       nonce.PubNonce[:],
					SecNonce:       nonce.SecNonce[:],
					CreationTime:   nowUnix,
					LastUpdateTime: nowUnix,
				},
			)
			if err != nil {
				return fmt.Errorf("insert signing nonce: %w",
					err)
			}
		}

		return insertRoundEffect(
			ctx, q, r.RoundID.String(), nowUnix,
			round.RoundEffectSendNonces,
		)
	})
}

// LoadSigningNonces loads local MuSig2 nonce material for a round.
func (s *RoundPersistenceStore) LoadSigningNonces(ctx context.Context,
	roundID round.RoundID) ([]round.SigningNonceState, error) {

	readTxOpts := ReadTxOption()
	roundIDStr := roundID.String()

	var result []round.SigningNonceState
	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		rows, err := q.GetClientRoundNonceState(ctx, roundIDStr)
		if err != nil {
			return fmt.Errorf("get signing nonces: %w", err)
		}

		result = make([]round.SigningNonceState, 0, len(rows))
		for _, row := range rows {
			nonce, err := signingNonceRowToDomain(row)
			if err != nil {
				return err
			}

			result = append(result, nonce)
		}

		return nil
	})

	return result, err
}

// SaveAggregatedNonces persists operator aggregate nonce material for a round.
func (s *RoundPersistenceStore) SaveAggregatedNonces(ctx context.Context,
	roundID round.RoundID, nonces map[tree.TxID]tree.Musig2PubNonce) error {

	if len(nonces) == 0 {
		return nil
	}

	roundIDStr := roundID.String()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		nowUnix := s.clock.Now().Unix()
		for txID, aggNonce := range nonces {
			err := q.InsertClientRoundAggNonceState(
				ctx, sqlc.InsertClientRoundAggNonceStateParams{
					RoundID:        roundIDStr,
					Txid:           txID[:],
					AggNonce:       aggNonce[:],
					CreationTime:   nowUnix,
					LastUpdateTime: nowUnix,
				},
			)
			if err != nil {
				return fmt.Errorf("insert aggregate nonce: %w",
					err)
			}
		}

		err := q.UpdateRoundStatus(
			ctx, sqlc.UpdateRoundStatusParams{
				RoundID:        roundIDStr,
				Status:         "nonces_aggregated",
				LastUpdateTime: nowUnix,
			},
		)
		if err != nil {
			return fmt.Errorf("mark aggregate nonces persisted: %w",
				err)
		}

		return nil
	})
}

// LoadAggregatedNonces loads persisted operator aggregate nonce material.
func (s *RoundPersistenceStore) LoadAggregatedNonces(ctx context.Context,
	roundID round.RoundID) (map[tree.TxID]tree.Musig2PubNonce, error) {

	roundIDStr := roundID.String()

	result := make(map[tree.TxID]tree.Musig2PubNonce)
	err := s.db.ExecTx(ctx, ReadTxOption(), func(q RoundStore) error {
		rows, err := q.GetClientRoundAggNonceState(ctx, roundIDStr)
		if err != nil {
			return fmt.Errorf("get aggregate nonces: %w", err)
		}

		for _, row := range rows {
			nonce, err := aggNonceRowToDomain(row)
			if err != nil {
				return err
			}

			result[nonce.TxID] = nonce.AggNonce
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// SavePartialSignatures persists generated MuSig2 partial signatures and a
// durable send effect.
func (s *RoundPersistenceStore) SavePartialSignatures(ctx context.Context,
	roundID round.RoundID,
	signatures map[round.SignerKey]map[tree.TxID]*musig2.PartialSignature) error {

	if len(signatures) == 0 {
		return nil
	}

	roundIDStr := roundID.String()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		nowUnix := s.clock.Now().Unix()
		for signerKey, txSigs := range signatures {
			for txID, sig := range txSigs {
				sigBytes, err := serializePartialSig(sig)
				if err != nil {
					return fmt.Errorf("serialize "+
						"partial sig: %w", err)
				}

				err = q.InsertClientRoundPartialSigState(
					ctx,
					sqlc.InsertClientRoundPartialSigStateParams{
						RoundID:        roundIDStr,
						SigningKey:     signerKey[:],
						Txid:           txID[:],
						PartialSig:     sigBytes,
						CreationTime:   nowUnix,
						LastUpdateTime: nowUnix,
					},
				)
				if err != nil {
					return fmt.Errorf("insert "+
						"partial sig: %w", err)
				}
			}
		}

		err := q.UpdateRoundStatus(
			ctx, sqlc.UpdateRoundStatusParams{
				RoundID:        roundIDStr,
				Status:         "partial_sigs_sent",
				LastUpdateTime: nowUnix,
			},
		)
		if err != nil {
			return fmt.Errorf("mark partial sigs persisted: %w",
				err)
		}

		return insertRoundEffect(
			ctx, q, roundIDStr, nowUnix,
			round.RoundEffectSendPartialSigs,
		)
	})
}

// LoadPartialSignatures loads generated MuSig2 partial signatures.
func (s *RoundPersistenceStore) LoadPartialSignatures(ctx context.Context,
	roundID round.RoundID) (
	map[round.SignerKey]map[tree.TxID]*musig2.PartialSignature, error) {

	roundIDStr := roundID.String()

	result := make(
		map[round.SignerKey]map[tree.TxID]*musig2.PartialSignature,
	)
	err := s.db.ExecTx(ctx, ReadTxOption(), func(q RoundStore) error {
		rows, err := q.GetClientRoundPartialSigState(ctx, roundIDStr)
		if err != nil {
			return fmt.Errorf("get partial signatures: %w", err)
		}

		for _, row := range rows {
			partialSig, err := partialSigRowToDomain(row)
			if err != nil {
				return err
			}

			txSigs := result[partialSig.SignerKey]
			if txSigs == nil {
				txSigs = make(
					map[tree.TxID]*musig2.PartialSignature,
				)
				result[partialSig.SignerKey] = txSigs
			}

			txSigs[partialSig.TxID] = partialSig.PartialSig
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// SaveForfeitRequests persists expected VTXO forfeit requests.
func (s *RoundPersistenceStore) SaveForfeitRequests(ctx context.Context,
	r *round.Round, clientTrees map[round.SignerKey]*tree.Tree,
	requests []round.ForfeitRequestState) error {

	if len(requests) == 0 {
		return nil
	}
	if r == nil {
		return fmt.Errorf("nil round")
	}

	roundIDStr := r.RoundID.String()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		nowUnix := s.clock.Now().Unix()
		err := s.insertRoundForfeitCollectionFacts(
			ctx, q, r, clientTrees, nowUnix,
		)
		if err != nil {
			return err
		}

		for _, request := range requests {
			params, err := forfeitRequestToInsertParams(
				roundIDStr, request, nowUnix,
			)
			if err != nil {
				return fmt.Errorf("convert forfeit request: %w",
					err)
			}

			err = q.InsertClientRoundForfeitRequestState(
				ctx, params,
			)
			if err != nil {
				return fmt.Errorf("insert forfeit request: %w",
					err)
			}
		}

		return insertRoundEffect(
			ctx, q, roundIDStr, nowUnix,
			round.RoundEffectRequestVTXOForfeitSigs,
		)
	})
}

func (s *RoundPersistenceStore) insertRoundForfeitCollectionFacts(
	ctx context.Context, q RoundStore, r *round.Round,
	clientTrees map[round.SignerKey]*tree.Tree, nowUnix int64) error {

	commitmentTxBytes, commitmentTxid, err := serializeCommitmentTx(
		r.CommitmentTx,
	)
	if err != nil {
		return err
	}

	vtxtTreeBytes, err := serializeVTXOTreePaths(r.VTXOTreePaths)
	if err != nil {
		return err
	}

	roundIDStr := r.RoundID.String()
	err = q.InsertRound(ctx, InsertRoundParams{
		RoundID:               roundIDStr,
		ConfirmationHeight:    sql.NullInt32{},
		ConfirmationBlockHash: nil,
		CommitmentTx:          commitmentTxBytes,
		CommitmentTxid:        commitmentTxid,
		VtxtTree:              vtxtTreeBytes,
		Status:                "forfeit_sigs_collecting",
		CreationTime:          nowUnix,
		LastUpdateTime:        nowUnix,
		StartHeight:           int32(r.StartHeight),
	})
	if err != nil {
		return fmt.Errorf("insert round forfeit snapshot: %w", err)
	}

	for i := range r.Intents.Boarding {
		intent := r.Intents.Boarding[i]
		params, err := s.domainIntentToRoundParams(
			roundIDStr, &intent, nil,
		)
		if err != nil {
			return fmt.Errorf("convert boarding intent: %w", err)
		}

		err = q.InsertRoundBoardingIntent(ctx, params)
		if err != nil {
			return fmt.Errorf("insert boarding intent: %w", err)
		}
	}

	for i, vtxoReq := range r.Intents.VTXOs {
		reqParams, err := vtxoRequestToRoundParams(
			roundIDStr, i, &vtxoReq,
		)
		if err != nil {
			return fmt.Errorf("convert vtxo request: %w", err)
		}

		err = q.InsertRoundVtxoRequest(ctx, reqParams)
		if err != nil {
			return fmt.Errorf("insert vtxo request: %w", err)
		}
	}

	for key, clientTree := range clientTrees {
		treeData, err := SerializeTree(clientTree)
		if err != nil {
			return fmt.Errorf("serialize client tree: %w", err)
		}

		treeParams := sqlc.InsertRoundClientTreeParams{
			RoundID:   roundIDStr,
			ClientKey: key[:],
			TreeData:  treeData,
		}
		err = q.InsertRoundClientTree(ctx, treeParams)
		if err != nil {
			return fmt.Errorf("insert client tree: %w", err)
		}

		txidEntries, err := clientTree.ExtractTxids()
		if err != nil {
			return fmt.Errorf("extract client tree txids: %w", err)
		}

		for _, entry := range txidEntries {
			p := sqlc.InsertClientTreeTxidParams{
				Txid:        entry.Txid[:],
				RoundID:     roundIDStr,
				ClientKey:   key[:],
				TreeLevel:   int32(entry.TreeLevel),
				OutputIndex: int32(entry.OutputIndex),
			}
			err = q.InsertClientTreeTxid(ctx, p)
			if err != nil {
				return fmt.Errorf("insert client tree txid: %w",
					err)
			}
		}
	}

	return nil
}

// LoadForfeitRequests loads expected VTXO forfeit requests.
func (s *RoundPersistenceStore) LoadForfeitRequests(ctx context.Context,
	roundID round.RoundID) ([]round.ForfeitRequestState, error) {

	roundIDStr := roundID.String()

	var result []round.ForfeitRequestState
	err := s.db.ExecTx(ctx, ReadTxOption(), func(q RoundStore) error {
		rows, err := q.GetClientRoundForfeitRequestState(
			ctx, roundIDStr,
		)
		if err != nil {
			return fmt.Errorf("get forfeit requests: %w", err)
		}

		result = make([]round.ForfeitRequestState, 0, len(rows))
		for _, row := range rows {
			request, err := forfeitRequestRowToDomain(row)
			if err != nil {
				return err
			}

			result = append(result, request)
		}

		return nil
	})

	return result, err
}

// SaveCollectedForfeitSignature persists one VTXO actor forfeit response.
func (s *RoundPersistenceStore) SaveCollectedForfeitSignature(
	ctx context.Context, roundID round.RoundID,
	response *round.ForfeitSignatureResponse) error {

	if response == nil {
		return fmt.Errorf("nil forfeit signature response")
	}

	forfeitSig := &types.ForfeitTxSig{
		UnsignedTx:    response.ForfeitTx,
		ClientVTXOSig: response.Signature,
		SpendPath:     response.SpendPath,
	}

	roundIDStr := roundID.String()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		nowUnix := s.clock.Now().Unix()
		params, err := forfeitSigToInsertParams(
			roundIDStr, response.VTXOOutpoint, forfeitSig, nowUnix,
		)
		if err != nil {
			return err
		}

		return q.InsertClientRoundForfeitSigState(ctx, params)
	})
}

// LoadVTXOForfeitSignatures loads collected VTXO forfeit signatures.
func (s *RoundPersistenceStore) LoadVTXOForfeitSignatures(ctx context.Context,
	roundID round.RoundID) (map[wire.OutPoint]*types.ForfeitTxSig, error) {

	roundIDStr := roundID.String()

	result := make(map[wire.OutPoint]*types.ForfeitTxSig)
	err := s.db.ExecTx(ctx, ReadTxOption(), func(q RoundStore) error {
		rows, err := q.GetClientRoundForfeitSigState(ctx, roundIDStr)
		if err != nil {
			return fmt.Errorf("get forfeit signatures: %w", err)
		}

		for _, row := range rows {
			outpoint, forfeitSig, err := forfeitSigRowToDomain(row)
			if err != nil {
				return err
			}

			result[outpoint] = forfeitSig
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// SavePendingQuote persists an acknowledged out-of-order quote.
func (s *RoundPersistenceStore) SavePendingQuote(ctx context.Context,
	quote *round.JoinRoundQuoteReceived) error {

	if quote == nil {
		return fmt.Errorf("nil pending quote")
	}
	if quote.Quote == nil {
		return fmt.Errorf("nil pending quote payload")
	}

	roundIDStr := quote.RoundID.String()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		nowUnix := s.clock.Now().Unix()
		err := q.UpsertClientRoundPendingQuote(
			ctx, sqlc.UpsertClientRoundPendingQuoteParams{
				RoundID:        roundIDStr,
				QuoteID:        quote.Quote.QuoteID[:],
				SealPass:       int64(quote.Quote.SealPass),
				OperatorFeeSat: quote.Quote.OperatorFeeSat,
				QuoteExpiresAt: quote.Quote.QuoteExpiresAt,
				RejectReason: int32(
					quote.Quote.RejectReason,
				),
				CreationTime:   nowUnix,
				LastUpdateTime: nowUnix,
			},
		)
		if err != nil {
			return fmt.Errorf("upsert pending quote: %w", err)
		}

		if err := q.DeleteClientRoundPendingVTXOQuotes(
			ctx, roundIDStr,
		); err != nil {
			return fmt.Errorf("clear pending vtxo quotes: %w", err)
		}

		for idx, vtxoQuote := range quote.Quote.VTXOQuotes {
			err := q.InsertClientRoundPendingVTXOQuote(
				ctx,
				sqlc.InsertClientRoundPendingVTXOQuoteParams{
					RoundID:      roundIDStr,
					QuoteIndex:   int32(idx),
					PkScript:     vtxoQuote.PkScript,
					AmountSat:    vtxoQuote.AmountSat,
					RecipientKey: vtxoQuote.RecipientKey,
				},
			)
			if err != nil {
				return fmt.Errorf("insert pending vtxo "+
					"quote: %w", err)
			}
		}

		if err := q.DeleteClientRoundPendingLeaveQuotes(
			ctx, roundIDStr,
		); err != nil {
			return fmt.Errorf("clear pending leave quotes: %w", err)
		}

		for idx, leaveQuote := range quote.Quote.LeaveQuotes {
			err := q.InsertClientRoundPendingLeaveQuote(
				ctx,
				sqlc.InsertClientRoundPendingLeaveQuoteParams{
					RoundID:    roundIDStr,
					QuoteIndex: int32(idx),
					PkScript:   leaveQuote.PkScript,
					AmountSat:  leaveQuote.AmountSat,
				},
			)
			if err != nil {
				return fmt.Errorf("insert pending leave "+
					"quote: %w", err)
			}
		}

		return nil
	})
}

// LoadPendingQuotes loads acknowledged out-of-order quotes.
func (s *RoundPersistenceStore) LoadPendingQuotes(ctx context.Context) (
	map[round.RoundID]*round.JoinRoundQuoteReceived, error) {

	result := make(map[round.RoundID]*round.JoinRoundQuoteReceived)
	err := s.db.ExecTx(ctx, ReadTxOption(), func(q RoundStore) error {
		rows, err := q.ListClientRoundPendingQuotes(ctx)
		if err != nil {
			return fmt.Errorf("list pending quotes: %w", err)
		}

		for _, row := range rows {
			quote, err := pendingQuoteRowToDomain(ctx, q, row)
			if err != nil {
				return err
			}

			result[quote.RoundID] = quote
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

// DeletePendingQuote deletes a pending quote once it has been drained.
func (s *RoundPersistenceStore) DeletePendingQuote(ctx context.Context,
	roundID round.RoundID) error {

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		return q.DeleteClientRoundPendingQuote(ctx, roundID.String())
	})
}

// CommitState atomically persists both the round data and FSM state. This
// should be called at the "point of no return" when the client has sent
// partial signatures and the server may broadcast.
//
//nolint:funlen
func (s *RoundPersistenceStore) CommitState(ctx context.Context, r *round.Round,
	state round.ClientState) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		// Serialize commitment tx if present.
		commitmentTxBytes, commitmentTxid, err := serializeCommitmentTx(
			r.CommitmentTx,
		)
		if err != nil {
			return err
		}

		// Serialize VTXO trees if present.
		vtxtTreeBytes, err := serializeVTXOTreePaths(r.VTXOTreePaths)
		if err != nil {
			return err
		}

		nowUnix := s.clock.Now().Unix()

		// Insert/update round. Status is always 'input_sig_sent' since
		// we only persist at the "point of no return" after sending
		// input signatures. Confirmation info is None at this stage.
		roundParams := InsertRoundParams{
			RoundID:               r.RoundID.String(),
			ConfirmationHeight:    sql.NullInt32{},
			ConfirmationBlockHash: nil,
			CommitmentTx:          commitmentTxBytes,
			CommitmentTxid:        commitmentTxid,
			VtxtTree:              vtxtTreeBytes,
			Status:                "input_sig_sent",
			CreationTime:          nowUnix,
			LastUpdateTime:        nowUnix,
			StartHeight:           int32(r.StartHeight),
		}
		if err := q.InsertRound(ctx, roundParams); err != nil {
			return fmt.Errorf("insert round: %w", err)
		}

		// Extract InputSigSentState to access input signatures. This is
		// required since we only persist at the "point of no return".
		inputSigState, ok := state.(*round.InputSigSentState)
		if !ok {
			return fmt.Errorf("CommitState called with "+
				"non-InputSigSentState: %T", state)
		}

		// Insert boarding intents for this round.
		if len(r.Intents.Boarding) > 0 {
			numIntents := len(r.Intents.Boarding)
			numSigs := len(inputSigState.InputSigs)
			if numIntents != numSigs {
				return fmt.Errorf("mismatch between intents "+
					"(%d) and input sigs (%d)", numIntents,
					numSigs)
			}

			for i, intent := range r.Intents.Boarding {
				sig := inputSigState.InputSigs[i]
				iParams, err := s.domainIntentToRoundParams(
					r.RoundID.String(), &intent, sig,
				)
				if err != nil {
					return fmt.Errorf("convert intent: %w",
						err)
				}

				err = q.InsertRoundBoardingIntent(ctx, iParams)
				if err != nil {
					return fmt.Errorf("insert round "+
						"intent: %w", err)
				}

				err = q.UpdateBoardingIntentStatus(
					ctx,
					sqlc.UpdateBoardingIntentStatusParams{
						OutpointHash: intent.Outpoint.
							Hash[:],
						OutpointIndex: int32(
							intent.Outpoint.Index,
						),
						Status:         "adopted",
						LastUpdateTime: nowUnix,
					},
				)
				if err != nil {
					return fmt.Errorf("mark boarding "+
						"intent adopted: %w", err)
				}

				// Clear the pending Board RPC row bound to
				// this outpoint in the same transaction.
				// Once the intent is Adopted, the user's
				// Board call is durably checkpointed in the
				// round itself; the pending row is no longer
				// load-bearing and a stale row would
				// otherwise rebind a future Board replay to
				// an unrelated boarding deposit.
				clearParams := ClearPendingBoardParams{
					OutpointHash: intent.Outpoint.Hash[:],
					OutpointIndex: int32(
						intent.Outpoint.Index,
					),
				}
				err = q.ClearPendingBoardRequestByOutpoint(
					ctx, clearParams,
				)
				if err != nil {
					return fmt.Errorf("clear pending "+
						"board request for adopted "+
						"intent: %w", err)
				}
			}
		}

		// Insert VTXO requests for this round.
		for i, vtxoReq := range r.Intents.VTXOs {
			reqParams, err := vtxoRequestToRoundParams(
				r.RoundID.String(), i, &vtxoReq,
			)
			if err != nil {
				return fmt.Errorf("convert vtxo request: %w",
					err)
			}

			err = q.InsertRoundVtxoRequest(ctx, reqParams)
			if err != nil {
				return fmt.Errorf("insert vtxo request: %w",
					err)
			}
		}

		// Insert client trees if present in the state.
		if inputSigState.ClientTrees != nil {
			for key, clientTree := range inputSigState.ClientTrees {
				treeData, err := SerializeTree(clientTree)
				if err != nil {
					return fmt.Errorf("serialize client "+
						"tree: %w", err)
				}

				treeParams := sqlc.InsertRoundClientTreeParams{
					RoundID:   r.RoundID.String(),
					ClientKey: key[:],
					TreeData:  treeData,
				}
				err = q.InsertRoundClientTree(ctx, treeParams)
				if err != nil {
					return fmt.Errorf("insert client "+
						"tree: %w", err)
				}

				// Extract and insert txids for this client
				// tree to enable efficient lookup by txid.
				txidEntries, err := clientTree.ExtractTxids()
				if err != nil {
					return fmt.Errorf("extract client "+
						"tree txids: %w", err)
				}

				for _, entry := range txidEntries {
					p := sqlc.InsertClientTreeTxidParams{
						Txid:      entry.Txid[:],
						RoundID:   r.RoundID.String(),
						ClientKey: key[:],
						TreeLevel: int32(
							entry.TreeLevel,
						),
						OutputIndex: int32(
							entry.OutputIndex,
						),
					}
					err = q.InsertClientTreeTxid(ctx, p)
					if err != nil {
						return fmt.Errorf(
							"insert client tree "+
								"txid: %w",
							err)
					}
				}
			}
		}

		for outpoint, forfeitSig := range inputSigState.ForfeitTxs {
			params, err := forfeitSigToInsertParams(
				r.RoundID.String(), outpoint, forfeitSig,
				nowUnix,
			)
			if err != nil {
				return fmt.Errorf("convert forfeit "+
					"signature: %w", err)
			}

			err = q.InsertClientRoundForfeitSigState(ctx, params)
			if err != nil {
				return fmt.Errorf("insert forfeit "+
					"signature: %w", err)
			}
		}

		if len(inputSigState.InputSigs) > 0 {
			err := insertRoundEffect(
				ctx, q, r.RoundID.String(), nowUnix,
				round.RoundEffectSendBoardingSigs,
			)
			if err != nil {
				return err
			}
		}

		if len(inputSigState.ForfeitTxs) > 0 {
			err := insertRoundEffect(
				ctx, q, r.RoundID.String(), nowUnix,
				round.RoundEffectSendVTXOForfeitSigs,
			)
			if err != nil {
				return err
			}
		}

		if r.CommitmentTx.IsSome() {
			err := insertRoundEffect(
				ctx, q, r.RoundID.String(), nowUnix,
				round.RoundEffectRegisterConfirmation,
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func insertRoundEffect(ctx context.Context, q RoundStore, roundID string,
	nowUnix int64, effectType round.RoundEffectType) error {

	idempotencyKey := fmt.Sprintf("client-round/%s/%s", roundID, effectType)
	err := q.InsertClientRoundEffect(
		ctx, sqlc.InsertClientRoundEffectParams{
			ID:             idempotencyKey,
			RoundID:        roundID,
			EffectType:     string(effectType),
			IdempotencyKey: idempotencyKey,
			MaxAttempts:    10,
			NextAttemptAt:  nowUnix,
			CreatedAt:      nowUnix,
			UpdatedAt:      nowUnix,
		},
	)
	if err != nil {
		return fmt.Errorf("insert round effect %s: %w", effectType, err)
	}

	return nil
}

// ClaimDueRoundEffects claims restart-safe round effect rows.
func (s *RoundPersistenceStore) ClaimDueRoundEffects(ctx context.Context,
	owner string, limit int, lease time.Duration) ([]round.RoundEffect,
	error) {

	if limit <= 0 {
		return nil, nil
	}

	now := s.clock.Now()
	claimed := make([]round.RoundEffect, 0, limit)
	err := s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		ids, err := q.ListDueClientRoundEffectIDs(
			ctx, sqlc.ListDueClientRoundEffectIDsParams{
				NextAttemptAt: now.Unix(),
				Limit:         int32(limit),
			},
		)
		if err != nil {
			return err
		}

		for _, id := range ids {
			token := uuid.NewString()
			row, err := q.ClaimClientRoundEffect(
				ctx, sqlc.ClaimClientRoundEffectParams{
					ID: id,
					ClaimOwner: sql.NullString{
						String: owner,
						Valid:  true,
					},
					ClaimToken: sql.NullString{
						String: token,
						Valid:  true,
					},
					ClaimUntil: sql.NullInt64{
						Int64: now.Add(lease).Unix(),
						Valid: true,
					},
					UpdatedAt:     now.Unix(),
					NextAttemptAt: now.Unix(),
				},
			)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}

			effect, err := roundEffectFromRow(row)
			if err != nil {
				return err
			}
			claimed = append(claimed, effect)
		}

		return nil
	})

	return claimed, err
}

// MarkRoundEffectDone marks a claimed round effect complete.
func (s *RoundPersistenceStore) MarkRoundEffectDone(ctx context.Context, id,
	claimToken string) error {

	now := s.clock.Now().Unix()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		return q.MarkClientRoundEffectDone(
			ctx, sqlc.MarkClientRoundEffectDoneParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				DoneAt: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
			},
		)
	})
}

// ReleaseRoundEffectForRetry releases a failed claimed round effect.
func (s *RoundPersistenceStore) ReleaseRoundEffectForRetry(ctx context.Context,
	id, claimToken string, retryAfter time.Duration, failure error) error {

	now := s.clock.Now()
	errText := ""
	if failure != nil {
		errText = failure.Error()
	}

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		return q.ReleaseClientRoundEffectForRetry(
			ctx, sqlc.ReleaseClientRoundEffectForRetryParams{
				ID: id,
				ClaimToken: sql.NullString{
					String: claimToken,
					Valid:  true,
				},
				NextAttemptAt: now.Add(retryAfter).Unix(),
				LastError: sql.NullString{
					String: errText,
					Valid:  errText != "",
				},
				UpdatedAt: now.Unix(),
			},
		)
	})
}

// ReleaseExpiredRoundEffectClaims releases stale round effect claims.
func (s *RoundPersistenceStore) ReleaseExpiredRoundEffectClaims(
	ctx context.Context) error {

	now := s.clock.Now().Unix()

	return s.db.ExecTx(ctx, WriteTxOption(), func(q RoundStore) error {
		return q.ReleaseExpiredClientRoundEffectClaims(
			ctx, sqlc.ReleaseExpiredClientRoundEffectClaimsParams{
				ClaimUntil: sql.NullInt64{
					Int64: now,
					Valid: true,
				},
				UpdatedAt: now,
			},
		)
	})
}

func roundEffectFromRow(row RoundEffectRow) (round.RoundEffect, error) {
	roundID, err := round.ParseRoundID(row.RoundID)
	if err != nil {
		return round.RoundEffect{}, fmt.Errorf("parse round ID: %w",
			err)
	}

	effectType := round.RoundEffectType(row.EffectType)
	switch effectType {
	case round.RoundEffectSendNonces,
		round.RoundEffectSendBoardingSigs,
		round.RoundEffectSendPartialSigs,
		round.RoundEffectRequestVTXOForfeitSigs,
		round.RoundEffectSendVTXOForfeitSigs,
		round.RoundEffectRegisterConfirmation:
	default:
		return round.RoundEffect{}, fmt.Errorf("unknown round effect "+
			"type: %s", row.EffectType)
	}

	return round.RoundEffect{
		ID:         row.ID,
		RoundID:    roundID,
		EffectType: effectType,
		ClaimToken: row.ClaimToken.String,
	}, nil
}

// FetchState retrieves a round and its FSM state by round ID. Returns
// (round, state, err) atomically to ensure consistency.
func (s *RoundPersistenceStore) FetchState(ctx context.Context,
	roundID round.RoundID) (*round.Round, round.ClientState, error) {

	readTxOpts := ReadTxOption()

	var (
		resultRound *round.Round
		resultState round.ClientState
	)

	// Convert domain RoundID to string for DB query.
	roundIDStr := roundID.String()

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		// Fetch round row.
		dbRound, err := q.GetRound(ctx, roundIDStr)
		if err != nil {
			return fmt.Errorf("get round: %w", err)
		}

		// Fetch boarding intents for this round.
		dbIntents, err := q.GetRoundBoardingIntents(ctx, roundIDStr)
		if err != nil {
			return fmt.Errorf("get round boarding intents: %w", err)
		}

		// Fetch client trees for this round.
		dbTrees, err := q.GetRoundClientTrees(ctx, roundIDStr)
		if err != nil {
			return fmt.Errorf("get round client trees: %w", err)
		}

		// Convert to domain round.
		r, err := s.dbRoundToDomainRound(ctx, q, dbRound, dbIntents)
		if err != nil {
			return fmt.Errorf("convert round: %w", err)
		}

		resultRound = r

		// Reconstruct FSM state from relational data.
		state, err := s.reconstructFSMState(
			ctx, q, dbRound, dbIntents, dbTrees,
		)
		if err != nil {
			return fmt.Errorf("reconstruct FSM state: %w", err)
		}

		resultState = state

		return nil
	})

	return resultRound, resultState, err
}

// LookupRoundByCommitmentTx finds the round associated with a commitment
// transaction TXID. Used to route commitment tx confirmations to the correct
// round FSM.
func (s *RoundPersistenceStore) LookupRoundByCommitmentTx(ctx context.Context,
	txid chainhash.Hash) (*round.Round, error) {

	readTxOpts := ReadTxOption()

	var result *round.Round

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		dbRound, err := q.GetRoundByCommitmentTxid(ctx, txid[:])
		if err != nil {
			return fmt.Errorf("get round by txid: %w", err)
		}

		// Fetch boarding intents for this round.
		dbIntents, err := q.GetRoundBoardingIntents(
			ctx, dbRound.RoundID,
		)
		if err != nil {
			return fmt.Errorf("get round boarding intents: %w", err)
		}

		r, err := s.dbRoundToDomainRound(ctx, q, dbRound, dbIntents)
		if err != nil {
			return err
		}

		result = r

		return nil
	})

	return result, err
}

// ListActiveRounds returns all rounds that are in progress (commitment tx
// broadcast but not yet confirmed or expired).
func (s *RoundPersistenceStore) ListActiveRounds(ctx context.Context) (
	[]*round.Round, error) {

	readTxOpts := ReadTxOption()

	var result []*round.Round

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		dbRounds, err := q.ListActiveRounds(ctx)
		if err != nil {
			return fmt.Errorf("list active rounds: %w", err)
		}

		rounds := make([]*round.Round, 0, len(dbRounds))
		for _, dbRound := range dbRounds {
			// Fetch boarding intents for this round.
			dbIntents, err := q.GetRoundBoardingIntents(
				ctx, dbRound.RoundID,
			)
			if err != nil {
				return fmt.Errorf("get round boarding "+
					"intents: %w", err)
			}

			r, err := s.dbRoundToDomainRound(
				ctx, q, dbRound, dbIntents,
			)
			if err != nil {
				return fmt.Errorf("convert round: %w", err)
			}

			rounds = append(rounds, r)
		}

		result = rounds

		return nil
	})

	return result, err
}

// ListConfirmedRounds returns all rounds that have been confirmed on-chain.
func (s *RoundPersistenceStore) ListConfirmedRounds(ctx context.Context) (
	[]*round.Round, error) {

	readTxOpts := ReadTxOption()

	var result []*round.Round

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		dbRounds, err := q.ListRoundsByStatus(ctx, "confirmed")
		if err != nil {
			return fmt.Errorf("list confirmed rounds: %w", err)
		}

		rounds := make([]*round.Round, 0, len(dbRounds))
		for _, dbRound := range dbRounds {
			// Fetch boarding intents for this round.
			dbIntents, err := q.GetRoundBoardingIntents(
				ctx, dbRound.RoundID,
			)
			if err != nil {
				return fmt.Errorf("get round boarding "+
					"intents: %w", err)
			}

			r, err := s.dbRoundToDomainRound(
				ctx, q, dbRound, dbIntents,
			)
			if err != nil {
				return fmt.Errorf("convert round: %w", err)
			}

			rounds = append(rounds, r)
		}

		result = rounds

		return nil
	})

	return result, err
}

// FinalizeRound marks a round as complete and archives it. The confInfo
// contains the block height and hash at which the commitment tx was confirmed.
func (s *RoundPersistenceStore) FinalizeRound(ctx context.Context,
	roundID round.RoundID, txid chainhash.Hash,
	confInfo round.ConfInfo) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		params := sqlc.FinalizeRoundParams{
			RoundID:        roundID.String(),
			CommitmentTxid: txid[:],
			ConfirmationHeight: sql.NullInt32{
				Int32: confInfo.Height,
				Valid: true,
			},
			ConfirmationBlockHash: confInfo.BlockHash[:],
			LastUpdateTime:        s.clock.Now().Unix(),
		}

		return q.FinalizeRound(ctx, params)
	})
}

// SaveVTXOs persists one or more VTXOs after a round confirms. Each VTXO
// includes its extracted tree path for unilateral exit.
func (s *RoundPersistenceStore) SaveVTXOs(ctx context.Context,
	vtxos []*round.ClientVTXO) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		for _, cv := range vtxos {
			params, err := s.domainVTXOToInsertParams(cv)
			if err != nil {
				return fmt.Errorf("convert VTXO: %w", err)
			}

			if err := q.InsertVTXO(ctx, params); err != nil {
				return fmt.Errorf("insert VTXO: %w", err)
			}

			err = upsertRoundClientVTXOAncestry(ctx, q, cv)
			if err != nil {
				return fmt.Errorf("persist VTXO ancestry: %w",
					err)
			}
		}

		return nil
	})
}

// ListVTXOs returns all VTXOs currently owned by the client.
func (s *RoundPersistenceStore) ListVTXOs(ctx context.Context) (
	[]*round.ClientVTXO, error) {

	readTxOpts := ReadTxOption()

	var result []*round.ClientVTXO

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		dbVTXOs, err := q.ListUnspentVTXOs(ctx)
		if err != nil {
			return fmt.Errorf("list VTXOs: %w", err)
		}

		ancestryRows, err := q.ListUnspentVTXOAncestryPaths(ctx)
		if err != nil {
			return fmt.Errorf("list unspent ancestry paths: %w",
				err)
		}

		ancestryByOutpoint, err := groupAncestryRows(ancestryRows)
		if err != nil {
			return fmt.Errorf("group ancestry rows: %w", err)
		}

		vtxos := make([]*round.ClientVTXO, 0, len(dbVTXOs))
		for _, dbVTXO := range dbVTXOs {
			cv, err := s.dbVTXOToDomainVTXO(
				ctx, q, dbVTXO, ancestryByOutpoint,
			)
			if err != nil {
				return fmt.Errorf("convert VTXO: %w", err)
			}

			vtxos = append(vtxos, cv)
		}

		result = vtxos

		return nil
	})

	return result, err
}

// GetVTXO retrieves a specific VTXO by its outpoint. Returns an error if not
// found.
func (s *RoundPersistenceStore) GetVTXO(ctx context.Context,
	outpoint wire.OutPoint) (*round.ClientVTXO, error) {

	readTxOpts := ReadTxOption()

	var result *round.ClientVTXO

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		params := sqlc.GetVTXOParams{
			OutpointHash:  outpoint.Hash[:],
			OutpointIndex: int32(outpoint.Index),
		}

		dbVTXO, err := q.GetVTXO(ctx, params)
		if err != nil {
			return fmt.Errorf("get VTXO: %w", err)
		}

		cv, err := s.dbVTXOToDomainVTXO(ctx, q, dbVTXO, nil)
		if err != nil {
			return err
		}

		result = cv

		return nil
	})

	return result, err
}

// MarkVTXOSpent marks a VTXO as spent (either via OOR transaction or forfeit).
// This prevents double-spending and records when the spend occurred.
func (s *RoundPersistenceStore) MarkVTXOSpent(ctx context.Context,
	outpoint wire.OutPoint) error {

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		params := sqlc.MarkVTXOSpentParams{
			OutpointHash:   outpoint.Hash[:],
			OutpointIndex:  int32(outpoint.Index),
			LastUpdateTime: s.clock.Now().Unix(),
		}

		return q.MarkVTXOSpent(ctx, params)
	})
}

func signingNonceRowToDomain(row RoundSigningNonceRow) (round.SigningNonceState,
	error) {

	if len(row.SigningKey) != btcec.PubKeyBytesLenCompressed {
		return round.SigningNonceState{}, fmt.Errorf("invalid signing "+
			"key length: %d", len(row.SigningKey))
	}

	if len(row.Txid) != chainhash.HashSize {
		return round.SigningNonceState{}, fmt.Errorf("invalid txid "+
			"length: %d", len(row.Txid))
	}

	if len(row.PubNonce) != len(tree.Musig2PubNonce{}) {
		return round.SigningNonceState{}, fmt.Errorf("invalid public "+
			"nonce length: %d", len(row.PubNonce))
	}

	if len(row.SecNonce) != len(tree.Musig2SecNonce{}) {
		return round.SigningNonceState{}, fmt.Errorf("invalid secret "+
			"nonce length: %d", len(row.SecNonce))
	}

	var signerKey round.SignerKey
	copy(signerKey[:], row.SigningKey)

	var txID tree.TxID
	copy(txID[:], row.Txid)

	var pubNonce tree.Musig2PubNonce
	copy(pubNonce[:], row.PubNonce)

	var secNonce tree.Musig2SecNonce
	copy(secNonce[:], row.SecNonce)

	return round.SigningNonceState{
		SignerKey: signerKey,
		TxID:      txID,
		PubNonce:  pubNonce,
		SecNonce:  secNonce,
	}, nil
}

func aggNonceRowToDomain(row RoundAggNonceRow) (round.AggregatedNonceState,
	error) {

	if len(row.Txid) != chainhash.HashSize {
		return round.AggregatedNonceState{}, fmt.Errorf("invalid txid "+
			"length: %d", len(row.Txid))
	}

	if len(row.AggNonce) != len(tree.Musig2PubNonce{}) {
		return round.AggregatedNonceState{}, fmt.Errorf("invalid "+
			"aggregate nonce length: %d", len(row.AggNonce))
	}

	var txID tree.TxID
	copy(txID[:], row.Txid)

	var aggNonce tree.Musig2PubNonce
	copy(aggNonce[:], row.AggNonce)

	return round.AggregatedNonceState{
		TxID:     txID,
		AggNonce: aggNonce,
	}, nil
}

func serializePartialSig(sig *musig2.PartialSignature) ([]byte, error) {
	if sig == nil {
		return nil, fmt.Errorf("nil partial signature")
	}

	var buf bytes.Buffer
	if err := sig.Encode(&buf); err != nil {
		return nil, err
	}

	if buf.Len() != 32 {
		return nil, fmt.Errorf("invalid partial signature length: %d",
			buf.Len())
	}

	return buf.Bytes(), nil
}

func partialSigRowToDomain(row RoundPartialSigRow) (round.PartialSignatureState,
	error) {

	if len(row.SigningKey) != btcec.PubKeyBytesLenCompressed {
		return round.PartialSignatureState{}, fmt.Errorf("invalid "+
			"signing key length: %d", len(row.SigningKey))
	}

	if len(row.Txid) != chainhash.HashSize {
		return round.PartialSignatureState{}, fmt.Errorf("invalid "+
			"txid length: %d", len(row.Txid))
	}

	if len(row.PartialSig) != 32 {
		return round.PartialSignatureState{}, fmt.Errorf("invalid "+
			"partial signature length: %d", len(row.PartialSig))
	}

	var signerKey round.SignerKey
	copy(signerKey[:], row.SigningKey)

	var txID tree.TxID
	copy(txID[:], row.Txid)

	partialSig := &musig2.PartialSignature{}
	if err := partialSig.Decode(
		bytes.NewReader(row.PartialSig),
	); err != nil {
		return round.PartialSignatureState{}, fmt.Errorf("decode "+
			"partial signature: %w", err)
	}

	return round.PartialSignatureState{
		SignerKey:  signerKey,
		TxID:       txID,
		PartialSig: partialSig,
	}, nil
}

func forfeitSigToInsertParams(roundID string, outpoint wire.OutPoint,
	forfeitSig *types.ForfeitTxSig,
	nowUnix int64) (sqlc.InsertClientRoundForfeitSigStateParams, error) {

	if forfeitSig == nil {
		return sqlc.InsertClientRoundForfeitSigStateParams{},
			fmt.Errorf("nil forfeit signature")
	}
	if forfeitSig.UnsignedTx == nil {
		return sqlc.InsertClientRoundForfeitSigStateParams{},
			fmt.Errorf("nil forfeit tx")
	}
	if forfeitSig.ClientVTXOSig == nil {
		return sqlc.InsertClientRoundForfeitSigStateParams{},
			fmt.Errorf("nil client VTXO signature")
	}
	if forfeitSig.SpendPath == nil {
		return sqlc.InsertClientRoundForfeitSigStateParams{},
			fmt.Errorf("nil spend path")
	}

	var txBuf bytes.Buffer
	if err := forfeitSig.UnsignedTx.Serialize(&txBuf); err != nil {
		return sqlc.InsertClientRoundForfeitSigStateParams{},
			fmt.Errorf("serialize forfeit tx: %w", err)
	}

	spendPath, err := forfeitSig.SpendPath.Encode()
	if err != nil {
		return sqlc.InsertClientRoundForfeitSigStateParams{},
			fmt.Errorf("encode spend path: %w", err)
	}

	return sqlc.InsertClientRoundForfeitSigStateParams{
		RoundID:           roundID,
		VtxoOutpointHash:  outpoint.Hash[:],
		VtxoOutpointIndex: int32(outpoint.Index),
		ForfeitTx:         txBuf.Bytes(),
		ClientSig:         forfeitSig.ClientVTXOSig.Serialize(),
		SpendPath:         spendPath,
		CreationTime:      nowUnix,
		LastUpdateTime:    nowUnix,
	}, nil
}

func forfeitRequestToInsertParams(roundID string,
	request round.ForfeitRequestState,
	nowUnix int64) (sqlc.InsertClientRoundForfeitRequestStateParams,
	error) {

	var forfeitSpend []byte
	if request.ForfeitSpend != nil {
		var err error
		forfeitSpend, err = request.ForfeitSpend.Encode()
		if err != nil {
			return sqlc.InsertClientRoundForfeitRequestStateParams{},
				fmt.Errorf("encode forfeit spend: %w", err)
		}
	}

	return sqlc.InsertClientRoundForfeitRequestStateParams{
		RoundID:                roundID,
		VtxoOutpointHash:       request.VTXOOutpoint.Hash[:],
		VtxoOutpointIndex:      int32(request.VTXOOutpoint.Index),
		ConnectorOutpointHash:  request.ConnectorOutpoint.Hash[:],
		ConnectorOutpointIndex: int32(request.ConnectorOutpoint.Index),
		ConnectorPkScript:      bytes.Clone(request.ConnectorPkScript),
		ConnectorAmount:        request.ConnectorAmount,
		VtxoAmount:             request.VTXOAmount,
		ServerForfeitPkScript: bytes.Clone(
			request.ServerForfeitPkScript,
		),
		ForfeitSpend:   forfeitSpend,
		CreationTime:   nowUnix,
		LastUpdateTime: nowUnix,
	}, nil
}

func forfeitRequestRowToDomain(row RoundForfeitRequestRow) (
	round.ForfeitRequestState, error) {

	if len(row.VtxoOutpointHash) != chainhash.HashSize {
		return round.ForfeitRequestState{}, fmt.Errorf("invalid VTXO "+
			"outpoint hash length: %d", len(row.VtxoOutpointHash))
	}
	if len(row.ConnectorOutpointHash) != chainhash.HashSize {
		return round.ForfeitRequestState{}, fmt.Errorf("invalid "+
			"connector outpoint hash length: %d",
			len(row.ConnectorOutpointHash))
	}

	var vtxoHash chainhash.Hash
	copy(vtxoHash[:], row.VtxoOutpointHash)

	var connectorHash chainhash.Hash
	copy(connectorHash[:], row.ConnectorOutpointHash)

	var forfeitSpend *arkscript.SpendPath
	if len(row.ForfeitSpend) > 0 {
		var err error
		forfeitSpend, err = arkscript.DecodeSpendPath(row.ForfeitSpend)
		if err != nil {
			return round.ForfeitRequestState{}, fmt.Errorf(
				"decode forfeit spend: %w", err)
		}
	}

	return round.ForfeitRequestState{
		VTXOOutpoint: wire.OutPoint{
			Hash:  vtxoHash,
			Index: uint32(row.VtxoOutpointIndex),
		},
		ConnectorOutpoint: wire.OutPoint{
			Hash:  connectorHash,
			Index: uint32(row.ConnectorOutpointIndex),
		},
		ConnectorPkScript:     bytes.Clone(row.ConnectorPkScript),
		ConnectorAmount:       row.ConnectorAmount,
		VTXOAmount:            row.VtxoAmount,
		ServerForfeitPkScript: bytes.Clone(row.ServerForfeitPkScript),
		ForfeitSpend:          forfeitSpend,
	}, nil
}

func forfeitSigRowToDomain(row RoundForfeitSigRow) (wire.OutPoint,
	*types.ForfeitTxSig, error) {

	if len(row.VtxoOutpointHash) != chainhash.HashSize {
		return wire.OutPoint{}, nil, fmt.Errorf("invalid VTXO "+
			"outpoint hash length: %d", len(row.VtxoOutpointHash))
	}

	if len(row.ClientSig) != schnorr.SignatureSize {
		return wire.OutPoint{}, nil, fmt.Errorf("invalid client "+
			"signature length: %d", len(row.ClientSig))
	}

	var outpointHash chainhash.Hash
	copy(outpointHash[:], row.VtxoOutpointHash)
	outpoint := wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(row.VtxoOutpointIndex),
	}

	forfeitTx := &wire.MsgTx{}
	if err := forfeitTx.Deserialize(
		bytes.NewReader(row.ForfeitTx),
	); err != nil {
		return wire.OutPoint{}, nil, fmt.Errorf("deserialize "+
			"forfeit tx: %w", err)
	}

	sig, err := schnorr.ParseSignature(row.ClientSig)
	if err != nil {
		return wire.OutPoint{}, nil, fmt.Errorf("parse client "+
			"signature: %w", err)
	}

	spendPath, err := arkscript.DecodeSpendPath(row.SpendPath)
	if err != nil {
		return wire.OutPoint{}, nil, fmt.Errorf("decode spend path: %w",
			err)
	}

	return outpoint, &types.ForfeitTxSig{
		UnsignedTx:    forfeitTx,
		ClientVTXOSig: sig,
		SpendPath:     spendPath,
	}, nil
}

func pendingQuoteRowToDomain(ctx context.Context, q RoundStore,
	row RoundPendingQuoteRow) (*round.JoinRoundQuoteReceived, error) {

	roundID, err := round.ParseRoundID(row.RoundID)
	if err != nil {
		return nil, fmt.Errorf("parse pending quote round ID: %w", err)
	}

	if len(row.QuoteID) != 32 {
		return nil, fmt.Errorf("pending quote_id length %d, want 32",
			len(row.QuoteID))
	}

	if row.SealPass < 0 || row.SealPass > int64(^uint32(0)) {
		return nil, fmt.Errorf("pending quote seal_pass out of "+
			"range: %d", row.SealPass)
	}

	var quoteID [32]byte
	copy(quoteID[:], row.QuoteID)

	vtxoRows, err := q.GetClientRoundPendingVTXOQuotes(ctx, row.RoundID)
	if err != nil {
		return nil, fmt.Errorf("get pending vtxo quotes: %w", err)
	}

	vtxoQuotes := make([]round.VTXOQuoteEntry, 0, len(vtxoRows))
	for _, vtxoRow := range vtxoRows {
		vtxoQuotes = append(vtxoQuotes, round.VTXOQuoteEntry{
			PkScript:     bytes.Clone(vtxoRow.PkScript),
			AmountSat:    vtxoRow.AmountSat,
			RecipientKey: bytes.Clone(vtxoRow.RecipientKey),
		})
	}

	leaveRows, err := q.GetClientRoundPendingLeaveQuotes(ctx, row.RoundID)
	if err != nil {
		return nil, fmt.Errorf("get pending leave quotes: %w", err)
	}

	leaveQuotes := make([]round.LeaveQuoteEntry, 0, len(leaveRows))
	for _, leaveRow := range leaveRows {
		leaveQuotes = append(leaveQuotes, round.LeaveQuoteEntry{
			PkScript:  bytes.Clone(leaveRow.PkScript),
			AmountSat: leaveRow.AmountSat,
		})
	}

	return &round.JoinRoundQuoteReceived{
		RoundID: roundID,
		Quote: &round.ClientQuote{
			QuoteID:        quoteID,
			SealPass:       uint32(row.SealPass),
			OperatorFeeSat: row.OperatorFeeSat,
			VTXOQuotes:     vtxoQuotes,
			LeaveQuotes:    leaveQuotes,
			QuoteExpiresAt: row.QuoteExpiresAt,
			RejectReason: roundpb.QuoteReason(
				row.RejectReason,
			),
		},
	}, nil
}

// dbRoundToDomainRound converts a database round row to a domain Round struct.
func (s *RoundPersistenceStore) dbRoundToDomainRound(ctx context.Context,
	q RoundStore, dbRound RoundRow, dbIntents []RoundBoardingIntentRow) (
	*round.Round, error) {

	// Parse the round ID from the database string.
	roundID, err := round.ParseRoundID(dbRound.RoundID)
	if err != nil {
		return nil, fmt.Errorf("parse round ID: %w", err)
	}

	r := &round.Round{
		RoundID:     roundID,
		StartHeight: uint32(dbRound.StartHeight),
	}

	// Populate confirmation info if present.
	if dbRound.ConfirmationHeight.Valid &&
		len(dbRound.ConfirmationBlockHash) == chainhash.HashSize {

		var blockHash chainhash.Hash
		copy(blockHash[:], dbRound.ConfirmationBlockHash)

		r.ConfInfo = fn.Some(round.ConfInfo{
			Height:    dbRound.ConfirmationHeight.Int32,
			BlockHash: blockHash,
		})
	}

	// Deserialize commitment tx if present.
	if len(dbRound.CommitmentTx) > 0 {
		reader := bytes.NewReader(dbRound.CommitmentTx)
		packet, err := psbt.NewFromRawBytes(reader, false)
		if err != nil {
			return nil, fmt.Errorf("deserialize commitment tx: %w",
				err)
		}

		r.CommitmentTx = fn.Some(packet)
	}

	// Deserialize VTXO trees if present.
	if len(dbRound.VtxtTree) > 0 {
		vtxtTree, err := DeserializeTree(dbRound.VtxtTree)
		if err != nil {
			return nil, fmt.Errorf("deserialize vtxt tree: %w", err)
		}

		// For now, we store a single tree. Wrap it in a map at index 0.
		// TODO: Support proper multi-tree serialization format.
		r.VTXOTreePaths = fn.Some(map[int]*tree.Tree{0: vtxtTree})
	}

	// Convert boarding intents.
	if len(dbIntents) > 0 {
		intents := make([]round.BoardingIntent, 0, len(dbIntents))
		for _, dbIntent := range dbIntents {
			intent, err := s.dbRoundIntentToDomainIntent(
				ctx, q, dbIntent,
			)
			if err != nil {
				return nil, fmt.Errorf("convert round "+
					"intent: %w", err)
			}

			intents = append(intents, *intent)
		}

		r.Intents.Boarding = intents
	}

	return r, nil
}

// dbRoundIntentToDomainIntent converts a round boarding intent row to a domain
// BoardingIntent. This joins with the base boarding_intents table to get the
// full intent data.
func (s *RoundPersistenceStore) dbRoundIntentToDomainIntent(ctx context.Context,
	q RoundStore, dbRoundIntent RoundBoardingIntentRow) (
	*round.BoardingIntent, error) {

	// Fetch the base boarding intent.
	params := BoardingIntentKey{
		OutpointHash:  dbRoundIntent.OutpointHash,
		OutpointIndex: dbRoundIntent.OutpointIndex,
	}

	dbIntent, err := q.GetBoardingIntent(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("get boarding intent: %w", err)
	}

	// Fetch the boarding address.
	dbAddr, err := q.GetBoardingAddress(ctx, dbIntent.PkScript)
	if err != nil {
		return nil, fmt.Errorf("get boarding address: %w", err)
	}

	// Convert the base boarding intent.
	baseAddr, err := dbAddrToDomainAddr(s.chainParams, dbAddr)
	if err != nil {
		return nil, fmt.Errorf("convert address: %w", err)
	}

	var outpointHash chainhash.Hash
	copy(outpointHash[:], dbIntent.OutpointHash)
	outpoint := wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(dbIntent.OutpointIndex),
	}

	var confHash chainhash.Hash
	copy(confHash[:], dbIntent.ConfHash)

	var confTx *wire.MsgTx
	if len(dbIntent.ConfTx) > 0 {
		confTx = &wire.MsgTx{}
		reader := bytes.NewReader(dbIntent.ConfTx)
		if err := confTx.Deserialize(reader); err != nil {
			return nil, fmt.Errorf("deserialize conf tx: %w", err)
		}
	}

	chainInfo := round.BoardingChainInfo{
		ConfHeight: dbIntent.ConfHeight,
		ConfHash:   confHash,
		ConfTx:     confTx,
		OutPoint:   outpoint,
		Amount:     btcutil.Amount(dbIntent.Amount),
	}

	status, err := stringToStatus(dbIntent.Status)
	if err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}

	baseIntent := round.WalletBoardingIntent{
		Address:   *baseAddr,
		Outpoint:  outpoint,
		ChainInfo: chainInfo,
		Status:    status,
	}

	var clientKey, operatorKey *btcec.PublicKey
	if len(dbRoundIntent.ClientKey) > 0 {
		clientKey, err = btcec.ParsePubKey(dbRoundIntent.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("parse client key: %w", err)
		}
	}
	if len(dbRoundIntent.OperatorKey) > 0 {
		operatorKey, err = btcec.ParsePubKey(dbRoundIntent.OperatorKey)
		if err != nil {
			return nil, fmt.Errorf("parse operator key: %w", err)
		}
	}

	// Deserialize TxProof if present.
	var txProofOpt fn.Option[proof.TxProof]
	if len(dbRoundIntent.TxProof) > 0 {
		txProof, err := types.DeserializeTxProof(dbRoundIntent.TxProof)
		if err != nil {
			return nil, fmt.Errorf("deserialize tx proof: %w", err)
		}
		if txProof != nil {
			txProofOpt = fn.Some(*txProof)
		}
	}

	boardingReq := types.BoardingRequest{
		Outpoint:       &outpoint,
		PolicyTemplate: bytes.Clone(dbRoundIntent.PolicyTemplate),
		ClientKey:      clientKey,
		OperatorKey:    operatorKey,
		ExitDelay:      uint32(dbRoundIntent.ExitDelay),
		TxProof:        txProofOpt,
	}

	intent := &round.BoardingIntent{
		BoardingIntent: baseIntent,
		Request:        boardingReq,
	}

	return intent, nil
}

// reconstructFSMState reconstructs the FSM state from relational data.
//
// Nonce/signature exchange states are reconstructed so the actor can redrive
// persisted send effects after a restart. After input signatures are sent, the
// client additionally registers for commitment confirmation because the server
// may broadcast the commitment transaction at any time.
//
// Terminal statuses (confirmed, failed, archived) return minimal state objects
// to indicate the round's final status. If we encounter an unknown status, it
// indicates data corruption or a version mismatch that must be surfaced as an
// error.
func (s *RoundPersistenceStore) reconstructFSMState(ctx context.Context,
	q RoundStore, dbRound RoundRow, dbIntents []RoundBoardingIntentRow,
	dbTrees []RoundClientTreeRow) (round.ClientState, error) {

	switch dbRound.Status {
	case "nonces_generated", "nonces_aggregated":
		return s.reconstructCommitmentTxValidatedState(
			ctx, q, dbRound, dbIntents, dbTrees,
		)

	case "partial_sigs_sent":
		return s.reconstructPartialSigsSentState(
			ctx, q, dbRound, dbIntents, dbTrees,
		)

	case "forfeit_sigs_collecting":
		return s.reconstructForfeitSignaturesCollectingState(
			ctx, q, dbRound, dbIntents, dbTrees,
		)

	case "input_sig_sent":
		return s.reconstructInputSigSentState(
			ctx, q, dbRound, dbIntents, dbTrees,
		)

	case "confirmed":
		// Confirmed rounds are terminal. Return a ConfirmedState with
		// the confirmation info from the database.
		var blockHash chainhash.Hash
		if len(dbRound.ConfirmationBlockHash) == chainhash.HashSize {
			copy(blockHash[:], dbRound.ConfirmationBlockHash)
		}

		var txid chainhash.Hash
		if len(dbRound.CommitmentTxid) == chainhash.HashSize {
			copy(txid[:], dbRound.CommitmentTxid)
		}

		return &round.ConfirmedState{
			TxID:        txid,
			BlockHeight: dbRound.ConfirmationHeight.Int32,
			BlockHash:   blockHash,
		}, nil

	case "failed", "archived":
		// Failed and archived rounds are terminal. Return nil state
		// since no FSM reconstruction is needed.
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown round status: %s",
			dbRound.Status)
	}
}

func (s *RoundPersistenceStore) reconstructCommitmentTxValidatedState(
	ctx context.Context, q RoundStore, dbRound RoundRow,
	dbIntents []RoundBoardingIntentRow, dbTrees []RoundClientTreeRow,
) (*round.CommitmentTxValidatedState, error) {

	roundID, err := round.ParseRoundID(dbRound.RoundID)
	if err != nil {
		return nil, fmt.Errorf("parse round ID: %w", err)
	}

	commitmentTx, err := deserializeCommitmentTx(dbRound.CommitmentTx)
	if err != nil {
		return nil, err
	}
	if commitmentTx == nil {
		return nil, fmt.Errorf("nonce-generated round %s has no "+
			"commitment tx", dbRound.RoundID)
	}

	vtxoTreePaths, err := deserializeStoredVTXOTreePaths(dbRound.VtxtTree)
	if err != nil {
		return nil, err
	}

	intents, err := s.roundIntentsFromRows(ctx, q, dbRound, dbIntents)
	if err != nil {
		return nil, err
	}

	clientTrees, err := deserializeClientTreeRows(dbTrees)
	if err != nil {
		return nil, err
	}

	return &round.CommitmentTxValidatedState{
		RoundID:       roundID,
		CommitmentTx:  commitmentTx,
		VTXOTreePaths: vtxoTreePaths,
		Intents:       intents,
		ClientTrees:   clientTrees,
		BoardingInputIndices: boardingInputIndices(
			commitmentTx, intents,
		),
		ForfeitMappings: nil,
	}, nil
}

func (s *RoundPersistenceStore) reconstructPartialSigsSentState(
	ctx context.Context, q RoundStore, dbRound RoundRow,
	dbIntents []RoundBoardingIntentRow, dbTrees []RoundClientTreeRow,
) (*round.PartialSigsSentState, error) {

	validated, err := s.reconstructCommitmentTxValidatedState(
		ctx, q, dbRound, dbIntents, dbTrees,
	)
	if err != nil {
		return nil, err
	}

	return &round.PartialSigsSentState{
		RoundID:              validated.RoundID,
		CommitmentTx:         validated.CommitmentTx,
		VTXOTreePaths:        validated.VTXOTreePaths,
		Intents:              validated.Intents.Clone(),
		ClientTrees:          validated.ClientTrees,
		BoardingInputIndices: validated.BoardingInputIndices,
		ForfeitMappings:      validated.ForfeitMappings,
	}, nil
}

func (s *RoundPersistenceStore) reconstructForfeitSignaturesCollectingState(
	ctx context.Context, q RoundStore, dbRound RoundRow,
	dbIntents []RoundBoardingIntentRow, dbTrees []RoundClientTreeRow,
) (*round.ForfeitSignaturesCollectingState, error) {

	partialState, err := s.reconstructPartialSigsSentState(
		ctx, q, dbRound, dbIntents, dbTrees,
	)
	if err != nil {
		return nil, err
	}

	requestRows, err := q.GetClientRoundForfeitRequestState(
		ctx, dbRound.RoundID,
	)
	if err != nil {
		return nil, fmt.Errorf("get forfeit requests: %w", err)
	}

	expectedForfeits := make(
		map[wire.OutPoint]*round.ConnectorLeafInfo, len(requestRows),
	)
	for _, row := range requestRows {
		request, err := forfeitRequestRowToDomain(row)
		if err != nil {
			return nil, err
		}

		expectedForfeits[request.VTXOOutpoint] = &round.ConnectorLeafInfo{
			ConnectorOutpoint: request.ConnectorOutpoint,
			ConnectorPkScript: bytes.Clone(
				request.ConnectorPkScript,
			),
			ConnectorAmount: request.ConnectorAmount,
			VTXOAmount:      btcutil.Amount(request.VTXOAmount),
		}
	}

	sigRows, err := q.GetClientRoundForfeitSigState(ctx, dbRound.RoundID)
	if err != nil {
		return nil, fmt.Errorf("get collected forfeit signatures: %w",
			err)
	}

	collected := make(
		map[wire.OutPoint]*round.ForfeitSignatureResponse, len(sigRows),
	)
	for _, row := range sigRows {
		outpoint, sig, err := forfeitSigRowToDomain(row)
		if err != nil {
			return nil, err
		}

		collected[outpoint] = &round.ForfeitSignatureResponse{
			VTXOOutpoint: outpoint,
			RoundID:      dbRound.RoundID,
			ForfeitTx:    sig.UnsignedTx,
			Signature:    sig.ClientVTXOSig,
			SpendPath:    sig.SpendPath,
		}
	}

	return &round.ForfeitSignaturesCollectingState{
		RoundID:              partialState.RoundID,
		CommitmentTx:         partialState.CommitmentTx,
		VTXOTreePaths:        partialState.VTXOTreePaths,
		Intents:              partialState.Intents.Clone(),
		ClientTrees:          partialState.ClientTrees,
		BoardingInputIndices: partialState.BoardingInputIndices,
		ExpectedForfeits:     expectedForfeits,
		CollectedForfeits:    collected,
	}, nil
}

// reconstructInputSigSentState reconstructs the InputSigSentState from
// relational data.
func (s *RoundPersistenceStore) reconstructInputSigSentState(
	ctx context.Context, q RoundStore, dbRound RoundRow,
	dbIntents []RoundBoardingIntentRow, dbTrees []RoundClientTreeRow,
) (*round.InputSigSentState, error) {

	// Parse the round ID from the database string.
	roundID, err := round.ParseRoundID(dbRound.RoundID)
	if err != nil {
		return nil, fmt.Errorf("parse round ID: %w", err)
	}

	state := &round.InputSigSentState{
		RoundID:     roundID,
		ClientTrees: make(map[round.SignerKey]*tree.Tree),
	}

	commitmentTx, err := deserializeCommitmentTx(dbRound.CommitmentTx)
	if err != nil {
		return nil, err
	}
	state.CommitmentTx = commitmentTx

	vtxoTreePaths, err := deserializeStoredVTXOTreePaths(dbRound.VtxtTree)
	if err != nil {
		return nil, err
	}
	state.VTXOTreePaths = vtxoTreePaths

	// Fetch VTXO requests for this round.
	dbVtxoReqs, err := q.GetRoundVtxoRequests(ctx, dbRound.RoundID)
	if err != nil {
		return nil, fmt.Errorf("get round vtxo requests: %w", err)
	}

	vtxos := make([]types.VTXORequest, 0, len(dbVtxoReqs))
	for _, dbReq := range dbVtxoReqs {
		req, err := dbVtxoRequestRowToVTXORequest(dbReq)
		if err != nil {
			return nil, fmt.Errorf("convert round vtxo request: %w",
				err)
		}
		vtxos = append(vtxos, *req)
	}

	// Convert boarding intents and input signatures.
	intents := make([]round.BoardingIntent, 0, len(dbIntents))
	inputSigs := make([]*types.BoardingInputSignature, 0, len(dbIntents))
	for _, dbIntent := range dbIntents {
		intent, err := s.dbRoundIntentToDomainIntent(ctx, q, dbIntent)
		if err != nil {
			return nil, fmt.Errorf("convert round intent: %w", err)
		}

		intents = append(intents, *intent)

		// Reconstruct the BoardingInputSignature from stored data.
		if len(dbIntent.InputSignature) > 0 {
			inputSig := dbIntent.InputSignature
			sig, err := schnorr.ParseSignature(inputSig)
			if err != nil {
				return nil, fmt.Errorf("parse input "+
					"signature: %w", err)
			}

			var outpoint wire.OutPoint
			copy(outpoint.Hash[:], dbIntent.OutpointHash)
			outpoint.Index = uint32(dbIntent.OutpointIndex)

			inputSigs = append(
				inputSigs,
				&types.BoardingInputSignature{
					InputIndex: int(
						dbIntent.
							InputIndex.
							Int32,
					),
					Outpoint:        outpoint,
					ClientSignature: sig,
				},
			)
		} else {
			// Missing signature - should not happen for
			// input_sig_sent state, but handle gracefully.
			inputSigs = append(inputSigs, nil)
		}
	}

	state.Intents = round.Intents{
		Boarding: intents,
		VTXOs:    vtxos,
	}
	state.InputSigs = inputSigs

	// Deserialize client trees.
	for _, dbTree := range dbTrees {
		clientTree, err := DeserializeTree(dbTree.TreeData)
		if err != nil {
			return nil, fmt.Errorf("deserialize client tree: %w",
				err)
		}

		var signerKey round.SignerKey
		copy(signerKey[:], dbTree.ClientKey)
		state.ClientTrees[signerKey] = clientTree
	}

	return state, nil
}

func deserializeCommitmentTx(raw []byte) (*psbt.Packet, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(raw), false)
	if err != nil {
		return nil, fmt.Errorf("deserialize commitment tx: %w", err)
	}

	return packet, nil
}

func deserializeStoredVTXOTreePaths(raw []byte) (map[int]*tree.Tree, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	vtxtTree, err := DeserializeTree(raw)
	if err != nil {
		return nil, fmt.Errorf("deserialize vtxt tree: %w", err)
	}

	return map[int]*tree.Tree{0: vtxtTree}, nil
}

func (s *RoundPersistenceStore) roundIntentsFromRows(ctx context.Context,
	q RoundStore, dbRound RoundRow, dbIntents []RoundBoardingIntentRow) (
	round.Intents, error) {

	dbVtxoReqs, err := q.GetRoundVtxoRequests(ctx, dbRound.RoundID)
	if err != nil {
		return round.Intents{}, fmt.Errorf("get round vtxo "+
			"requests: %w", err)
	}

	vtxos := make([]types.VTXORequest, 0, len(dbVtxoReqs))
	for _, dbReq := range dbVtxoReqs {
		req, err := dbVtxoRequestRowToVTXORequest(dbReq)
		if err != nil {
			return round.Intents{}, fmt.Errorf("convert round "+
				"vtxo request: %w", err)
		}
		vtxos = append(vtxos, *req)
	}

	boardings := make([]round.BoardingIntent, 0, len(dbIntents))
	for _, dbIntent := range dbIntents {
		intent, err := s.dbRoundIntentToDomainIntent(ctx, q, dbIntent)
		if err != nil {
			return round.Intents{}, fmt.Errorf("convert round "+
				"intent: %w", err)
		}
		boardings = append(boardings, *intent)
	}

	return round.Intents{
		Boarding: boardings,
		VTXOs:    vtxos,
	}, nil
}

func deserializeClientTreeRows(dbTrees []RoundClientTreeRow) (
	map[round.SignerKey]*tree.Tree, error) {

	clientTrees := make(map[round.SignerKey]*tree.Tree, len(dbTrees))
	for _, dbTree := range dbTrees {
		clientTree, err := DeserializeTree(dbTree.TreeData)
		if err != nil {
			return nil, fmt.Errorf("deserialize client tree: %w",
				err)
		}

		var signerKey round.SignerKey
		copy(signerKey[:], dbTree.ClientKey)
		clientTrees[signerKey] = clientTree
	}

	return clientTrees, nil
}

func boardingInputIndices(commitmentTx *psbt.Packet,
	intents round.Intents) map[wire.OutPoint]int {

	indices := make(map[wire.OutPoint]int, len(intents.Boarding))
	if commitmentTx == nil || commitmentTx.UnsignedTx == nil {
		return indices
	}

	wanted := make(map[wire.OutPoint]struct{}, len(intents.Boarding))
	for _, intent := range intents.Boarding {
		wanted[intent.Outpoint] = struct{}{}
	}
	for i, txIn := range commitmentTx.UnsignedTx.TxIn {
		if _, ok := wanted[txIn.PreviousOutPoint]; ok {
			indices[txIn.PreviousOutPoint] = i
		}
	}

	return indices
}

// domainIntentToRoundParams converts a round.BoardingIntent to sqlc insert
// parameters for the round_boarding_intents table. The inputSig parameter
// contains the client's input signature for this boarding intent, which is
// critical for state recovery after restart.
func (s *RoundPersistenceStore) domainIntentToRoundParams(roundID string,
	intent *round.BoardingIntent, inputSig *types.BoardingInputSignature) (
	sqlc.InsertRoundBoardingIntentParams, error) {

	// Serialize TxProof if present.
	var txProofBytes []byte
	if intent.Request.TxProof.IsSome() {
		p := intent.Request.TxProof.UnsafeFromSome()
		data, err := types.SerializeTxProof(&p)
		if err != nil {
			return sqlc.InsertRoundBoardingIntentParams{},
				fmt.Errorf("serialize tx proof: %w", err)
		}
		txProofBytes = data
	}

	policyTemplate, err := intent.Request.EffectivePolicyTemplate()
	if err != nil {
		return sqlc.InsertRoundBoardingIntentParams{}, fmt.Errorf(
			"encode boarding policy template: %w", err)
	}

	params, err := intent.Request.DecodeStandardPolicyTemplate()
	if err != nil {
		return sqlc.InsertRoundBoardingIntentParams{}, fmt.Errorf(
			"decode boarding policy template: %w", err)
	}

	clientKey := params.OwnerKey.SerializeCompressed()
	operatorKey := params.OperatorKey.SerializeCompressed()

	var inputIdxVal sql.NullInt32
	if inputSig != nil {
		if inputSig.Outpoint != intent.Outpoint {
			return sqlc.InsertRoundBoardingIntentParams{},
				fmt.Errorf("input signature outpoint %s does "+
					"not match intent outpoint %s",
					inputSig.Outpoint, intent.Outpoint)
		}

		inputIdxVal = sql.NullInt32{
			Int32: int32(inputSig.InputIndex),
			Valid: true,
		}
	}

	// Serialize the input signature if present.
	var inputSigBytes []byte
	if inputSig != nil && inputSig.ClientSignature != nil {
		inputSigBytes = inputSig.ClientSignature.Serialize()
	}

	return sqlc.InsertRoundBoardingIntentParams{
		RoundID:        roundID,
		OutpointHash:   intent.Outpoint.Hash[:],
		OutpointIndex:  int32(intent.Outpoint.Index),
		ClientKey:      clientKey,
		OperatorKey:    operatorKey,
		ExitDelay:      int32(params.ExitDelay),
		PolicyTemplate: policyTemplate,
		TxProof:        txProofBytes,
		InputIndex:     inputIdxVal,
		InputSignature: inputSigBytes,
	}, nil
}

// vtxoRequestToRoundParams converts a types.VTXORequest to sqlc insert
// parameters for the round_vtxo_requests table.
func vtxoRequestToRoundParams(roundID string, requestIndex int,
	req *types.VTXORequest) (sqlc.InsertRoundVtxoRequestParams, error) {

	params, err := req.DecodeStandardPolicyTemplate()
	if err != nil {
		return sqlc.InsertRoundVtxoRequestParams{}, fmt.Errorf(
			"decode VTXO policy template: %w", err)
	}

	pkScript, err := req.EffectivePkScript()
	if err != nil {
		return sqlc.InsertRoundVtxoRequestParams{}, fmt.Errorf(
			"derive VTXO pkScript: %w", err)
	}

	var signingPubkey []byte
	if req.SigningKey.PubKey != nil {
		signingPubkey = req.SigningKey.PubKey.SerializeCompressed()
	}

	ownerKeyFamily := int32(-1)
	ownerKeyIndex := int32(-1)
	if req.OwnerKey.PubKey != nil {
		ownerKeyFamily = int32(req.OwnerKey.KeyLocator.Family)
		ownerKeyIndex = int32(req.OwnerKey.KeyLocator.Index)
	}

	policyTemplate, err := req.EffectivePolicyTemplate()
	if err != nil {
		return sqlc.InsertRoundVtxoRequestParams{}, fmt.Errorf(
			"encode VTXO policy template: %w", err)
	}

	return sqlc.InsertRoundVtxoRequestParams{
		RoundID:          roundID,
		RequestIndex:     int32(requestIndex),
		Amount:           int64(req.Amount),
		PkScript:         pkScript,
		Expiry:           int32(params.ExitDelay),
		PolicyTemplate:   policyTemplate,
		ClientPubkey:     params.OwnerKey.SerializeCompressed(),
		OperatorPubkey:   params.OperatorKey.SerializeCompressed(),
		OwnerKeyFamily:   ownerKeyFamily,
		OwnerKeyIndex:    ownerKeyIndex,
		SigningKeyFamily: int32(req.SigningKey.KeyLocator.Family),
		SigningKeyIndex:  int32(req.SigningKey.KeyLocator.Index),
		SigningPubkey:    signingPubkey,
	}, nil
}

// dbVtxoRequestRowToVTXORequest converts a database row to a VTXORequest.
func dbVtxoRequestRowToVTXORequest(t RoundVtxoRequestRow) (*types.VTXORequest,
	error) {

	var clientPubkey *btcec.PublicKey
	if len(t.ClientPubkey) > 0 {
		var err error
		clientPubkey, err = btcec.ParsePubKey(t.ClientPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse client pubkey: %w", err)
		}
	}

	var operatorPubkey *btcec.PublicKey
	if len(t.OperatorPubkey) > 0 {
		var err error
		operatorPubkey, err = btcec.ParsePubKey(t.OperatorPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse operator pubkey: %w", err)
		}
	}

	var signingPubkey *btcec.PublicKey
	if len(t.SigningPubkey) > 0 {
		var err error
		signingPubkey, err = btcec.ParsePubKey(t.SigningPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse signing pubkey: %w", err)
		}
	}

	var ownerKey keychain.KeyDescriptor
	if t.OwnerKeyFamily >= 0 && t.OwnerKeyIndex >= 0 {
		ownerKey = keychain.KeyDescriptor{
			PubKey: clientPubkey,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(t.OwnerKeyFamily),
				Index:  uint32(t.OwnerKeyIndex),
			},
		}
	}

	return &types.VTXORequest{
		Amount:         btcutil.Amount(t.Amount),
		PolicyTemplate: bytes.Clone(t.PolicyTemplate),
		PkScript:       bytes.Clone(t.PkScript),
		ClientKey:      clientPubkey,
		OwnerKey:       ownerKey,
		Expiry:         uint32(t.Expiry),
		OperatorKey:    operatorPubkey,
		SigningKey: keychain.KeyDescriptor{
			PubKey: signingPubkey,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(t.SigningKeyFamily),
				Index:  uint32(t.SigningKeyIndex),
			},
		},
	}, nil
}

// domainVTXOToInsertParams converts a round.ClientVTXO to sqlc insert
// parameters. The single round-direct ancestry is persisted separately
// in the vtxo_ancestry_paths side table; callers must call
// upsertRoundClientVTXOAncestry alongside InsertVTXO inside the same
// transaction.
func (s *RoundPersistenceStore) domainVTXOToInsertParams(
	vtxo *round.ClientVTXO) (InsertVTXOParams, error) {

	roundIDStr := ""
	vtxo.RoundID.WhenSome(func(rid round.RoundID) {
		roundIDStr = rid.String()
	})

	var operatorPubkey []byte
	if vtxo.OperatorKey != nil {
		operatorPubkey = vtxo.OperatorKey.SerializeCompressed()
	}

	var clientPubkey []byte
	if vtxo.OwnerKey.PubKey != nil {
		clientPubkey = vtxo.OwnerKey.PubKey.SerializeCompressed()
	}

	nowUnix := s.clock.Now().Unix()

	policyTemplate := bytes.Clone(vtxo.PolicyTemplate)
	if len(policyTemplate) == 0 && vtxo.OwnerKey.PubKey != nil &&
		vtxo.OperatorKey != nil && vtxo.Expiry != 0 {

		encodedPolicy, err := arkscript.EncodeStandardVTXOTemplate(
			vtxo.OwnerKey.PubKey, vtxo.OperatorKey, vtxo.Expiry,
		)
		if err != nil {
			return InsertVTXOParams{}, fmt.Errorf("encode client "+
				"VTXO policy: %w", err)
		}

		policyTemplate = encodedPolicy
	}

	return InsertVTXOParams{
		OutpointHash:    vtxo.Outpoint.Hash[:],
		OutpointIndex:   int32(vtxo.Outpoint.Index),
		RoundID:         roundIDStr,
		Amount:          int64(vtxo.Amount),
		PkScript:        vtxo.PkScript,
		Expiry:          int32(vtxo.Expiry),
		PolicyTemplate:  policyTemplate,
		ClientKeyFamily: int32(vtxo.OwnerKey.Family),
		ClientKeyIndex:  int32(vtxo.OwnerKey.Index),
		ClientPubkey:    clientPubkey,
		OperatorPubkey:  operatorPubkey,
		BatchExpiry:     vtxo.BatchExpiry,
		ChainDepth:      0,
		CreatedHeight:   vtxo.CreatedHeight,
		CommitmentTxid:  vtxo.CommitmentTxID[:],
		Spent:           false,
		CreationTime:    nowUnix,
		LastUpdateTime:  nowUnix,
	}, nil
}

// upsertRoundClientVTXOAncestry persists the full Ancestry slice carried
// on a round.ClientVTXO into the vtxo_ancestry_paths side table. Must
// run in the same transaction as the parent vtxos InsertVTXO call to
// maintain referential integrity.
//
// VTXOs with an empty Ancestry (e.g. transient round-create rows that
// have not yet had their finalized lineage filled in) clear any prior
// side rows but write none — leaving the ancestry "unresolved" until
// the manager fills in the descriptor.
func upsertRoundClientVTXOAncestry(ctx context.Context, q RoundStore,
	clientVTXO *round.ClientVTXO) error {

	return upsertAncestryPaths(
		ctx, q, clientVTXO.Outpoint.Hash[:],
		int32(clientVTXO.Outpoint.Index), clientVTXO.Ancestry,
	)
}

// dbVTXOToDomainVTXO converts a database VTXO row to a domain ClientVTXO.
// The supplied query handle is used to load the per-VTXO ancestry rows
// from the side table and rehydrate the full Ancestry slice on the
// returned ClientVTXO. Round-direct VTXOs surface as length-1 slices;
// cross-round multi-input OOR VTXOs persisted via this side table
// surface with their full multi-fragment ancestry intact.
//
// preloaded is an optional per-outpoint ancestry index built by the
// caller (typically via groupAncestryRows over a batched ancestry
// query) so list paths can avoid the per-row N+1
// ListVTXOAncestryPaths fetch. When preloaded is nil,
// dbVTXOToDomainVTXO falls back to the singleton query.
func (s *RoundPersistenceStore) dbVTXOToDomainVTXO(ctx context.Context,
	q RoundStore, dbVTXO VTXORow,
	preloaded map[wire.OutPoint][]types.Ancestry) (*round.ClientVTXO,
	error) {

	var outpointHash chainhash.Hash
	copy(outpointHash[:], dbVTXO.OutpointHash)

	outpoint := wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(dbVTXO.OutpointIndex),
	}

	policyTemplate := bytes.Clone(dbVTXO.PolicyTemplate)

	// Parse client and operator public keys from the stored columns first.
	// These preserve the wallet's exact compressed pubkeys. When a semantic
	// policy template is present we use it to fill in missing keys and
	// derive the canonical expiry, but we don't want to overwrite stored
	// owner/operator pubkeys with x-only lifts from policy decoding.
	var clientPubkey, operatorPubkey *btcec.PublicKey
	expiry := uint32(dbVTXO.Expiry)
	if len(dbVTXO.ClientPubkey) > 0 {
		key, err := btcec.ParsePubKey(dbVTXO.ClientPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse client pubkey: %w", err)
		}

		clientPubkey = key
	}

	if len(dbVTXO.OperatorPubkey) > 0 {
		key, err := btcec.ParsePubKey(dbVTXO.OperatorPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse operator pubkey: %w", err)
		}

		operatorPubkey = key
	}

	if len(policyTemplate) > 0 {
		template, err := arkscript.DecodePolicyTemplate(policyTemplate)
		if err != nil {
			return nil, fmt.Errorf("decode client VTXO policy: %w",
				err)
		}

		params, err := arkscript.DecodeStandardVTXOParams(template)
		if err == nil {
			expiry = params.ExitDelay

			if clientPubkey == nil {
				clientPubkey = params.OwnerKey
			}

			if operatorPubkey == nil {
				operatorPubkey = params.OperatorKey
			}
		}
	}

	// Load the full Ancestry slice from the side table. The
	// ClientVTXO domain shape now mirrors the persistence shape, so
	// every fragment survives the round-trip — multi-fragment
	// cross-round OOR ancestry is not silently truncated. List paths
	// supply a pre-grouped index so a batched list call runs in 2
	// queries instead of N+1; the singleton path falls back to the
	// per-row query.
	var (
		ancestry []types.Ancestry
		err      error
	)
	if preloaded != nil {
		var key chainhash.Hash
		copy(key[:], dbVTXO.OutpointHash)
		ancestry = preloaded[wire.OutPoint{
			Hash:  key,
			Index: uint32(dbVTXO.OutpointIndex),
		}]
	} else {
		ancestry, err = loadAncestryPaths(
			ctx, q, dbVTXO.OutpointHash, dbVTXO.OutpointIndex,
		)
		if err != nil {
			return nil, fmt.Errorf("load ancestry paths: %w", err)
		}
	}

	var roundIDOpt fn.Option[round.RoundID]
	if dbVTXO.RoundID != "" {
		roundID, err := round.ParseRoundID(dbVTXO.RoundID)
		if err != nil {
			return nil, fmt.Errorf("parse round ID: %w", err)
		}

		roundIDOpt = fn.Some(roundID)
	}

	keyFamily := keychain.KeyFamily(dbVTXO.ClientKeyFamily)

	// Parse commitment txid. Empty/short blobs leave the zero hash so
	// historical rows that never got a round-metadata write remain
	// distinguishable from a real all-zero txid.
	var commitmentTxID chainhash.Hash
	if len(dbVTXO.CommitmentTxid) == chainhash.HashSize {
		copy(commitmentTxID[:], dbVTXO.CommitmentTxid)
	}

	return &round.ClientVTXO{
		Outpoint:       outpoint,
		Amount:         btcutil.Amount(dbVTXO.Amount),
		PolicyTemplate: policyTemplate,
		PkScript:       dbVTXO.PkScript,
		Expiry:         expiry,
		OwnerKey: keychain.KeyDescriptor{
			PubKey: clientPubkey,
			KeyLocator: keychain.KeyLocator{
				Family: keyFamily,
				Index:  uint32(dbVTXO.ClientKeyIndex),
			},
		},
		OperatorKey:    operatorPubkey,
		Ancestry:       ancestry,
		RoundID:        roundIDOpt,
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    dbVTXO.BatchExpiry,
		CreatedHeight:  dbVTXO.CreatedHeight,
	}, nil
}

// serializeCommitmentTx serializes a commitment transaction PSBT if present.
// Returns the serialized bytes and txid, or nil slices if the Option is None.
func serializeCommitmentTx(txOpt fn.Option[*psbt.Packet]) ([]byte, []byte,
	error) {

	if !txOpt.IsSome() {
		return nil, nil, nil
	}

	packet := txOpt.UnwrapOr(nil)

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, nil, fmt.Errorf("serialize commitment tx: %w", err)
	}

	txid := packet.UnsignedTx.TxHash()

	return buf.Bytes(), txid[:], nil
}

// serializeVTXOTreePaths serializes a VTXO tree paths map if present. Returns
// the serialized bytes or nil if the Option is None.
func serializeVTXOTreePaths(treesOpt fn.Option[map[int]*tree.Tree]) ([]byte,
	error) {

	if !treesOpt.IsSome() {
		return nil, nil
	}

	trees := treesOpt.UnwrapOr(nil)
	if len(trees) == 0 {
		return nil, nil
	}

	// Serialize as a simple format: for now just take the first tree.
	// TODO: Support multiple trees with proper serialization format.
	for _, t := range trees {
		data, err := SerializeTree(t)
		if err != nil {
			return nil, fmt.Errorf("serialize vtxt tree: %w", err)
		}

		return data, nil
	}

	return nil, nil
}

// RoundSummary is a lightweight round descriptor returned by paginated
// queries.
type RoundSummary struct {
	// RoundID is the unique identifier for this round.
	RoundID round.RoundID

	// Status is the persisted status string (e.g. "input_sig_sent",
	// "confirmed").
	Status string

	// CommitmentTxID is the commitment transaction id when persisted.
	CommitmentTxID fn.Option[chainhash.Hash]

	// ConfirmationHeight is the block height that confirmed the commitment
	// transaction when known.
	ConfirmationHeight fn.Option[int32]

	// CreationTime is the Unix timestamp when this round row was created.
	CreationTime int64

	// LastUpdateTime is the Unix timestamp when this round row was last
	// updated.
	LastUpdateTime int64

	// InputOutpoints are locally known round inputs. Today this is limited
	// to persisted boarding inputs because refresh and leave inputs are
	// not yet modeled as first-class persisted round input rows.
	InputOutpoints []wire.OutPoint

	// VTXOs lists the outpoints and amounts of VTXOs created in this
	// round.
	VTXOs []VTXOSummary
}

// VTXOSummary is a lightweight VTXO descriptor containing only the
// outpoint and amount.
type VTXOSummary struct {
	// Outpoint is the VTXO's outpoint.
	Outpoint wire.OutPoint

	// Amount is the VTXO value in satoshis.
	Amount btcutil.Amount
}

// GetRoundSummary returns one persisted round summary by round id.
func (s *RoundPersistenceStore) GetRoundSummary(ctx context.Context,
	roundID string) (*RoundSummary, error) {

	if s == nil || s.db == nil {
		return nil, fmt.Errorf("round store must be provided")
	}

	readTxOpts := ReadTxOption()

	var summary *RoundSummary
	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		dbRound, err := q.GetRound(ctx, roundID)
		if err != nil {
			return err
		}

		result, err := roundSummaryFromRow(ctx, q, dbRound)
		if err != nil {
			return err
		}

		summary = result

		return nil
	})
	if err != nil {
		return nil, err
	}

	return summary, nil
}

// roundSummaryFromRow materializes a RoundSummary from a persisted row.
func roundSummaryFromRow(ctx context.Context, q RoundStore,
	dbRound RoundRow) (*RoundSummary, error) {

	roundID, err := round.ParseRoundID(dbRound.RoundID)
	if err != nil {
		return nil, fmt.Errorf("parse round ID: %w", err)
	}

	vtxos, err := roundVTXOSummaries(ctx, q, dbRound.RoundID)
	if err != nil {
		return nil, err
	}

	inputOutpoints, err := roundBoardingInputOutpoints(
		ctx, q, dbRound.RoundID,
	)
	if err != nil {
		return nil, err
	}

	summary := &RoundSummary{
		RoundID:        roundID,
		Status:         dbRound.Status,
		CreationTime:   dbRound.CreationTime,
		LastUpdateTime: dbRound.LastUpdateTime,
		InputOutpoints: inputOutpoints,
		VTXOs:          vtxos,
	}

	if len(dbRound.CommitmentTxid) == chainhash.HashSize {
		var txid chainhash.Hash
		copy(txid[:], dbRound.CommitmentTxid)
		summary.CommitmentTxID = fn.Some(txid)
	}

	if dbRound.ConfirmationHeight.Valid {
		summary.ConfirmationHeight = fn.Some(
			dbRound.ConfirmationHeight.Int32,
		)
	}

	return summary, nil
}

// roundVTXOSummaries returns lightweight VTXO descriptors for a round.
func roundVTXOSummaries(ctx context.Context, q RoundStore,
	roundID string) ([]VTXOSummary, error) {

	dbVTXOs, err := q.ListVTXOsByRound(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("list vtxos for round %s: %w", roundID,
			err)
	}

	vtxos := make([]VTXOSummary, 0, len(dbVTXOs))
	for _, v := range dbVTXOs {
		outpoint, err := outpointFromDB(
			v.OutpointHash, v.OutpointIndex,
		)
		if err != nil {
			return nil, err
		}

		vtxos = append(vtxos, VTXOSummary{
			Outpoint: outpoint,
			Amount:   btcutil.Amount(v.Amount),
		})
	}

	return vtxos, nil
}

// roundBoardingInputOutpoints returns persisted boarding inputs for a round.
func roundBoardingInputOutpoints(ctx context.Context, q RoundStore,
	roundID string) ([]wire.OutPoint, error) {

	intents, err := q.GetRoundBoardingIntents(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("get round boarding intents for %s: %w",
			roundID, err)
	}

	outpoints := make([]wire.OutPoint, 0, len(intents))
	for _, intent := range intents {
		outpoint, err := outpointFromDB(
			intent.OutpointHash, intent.OutpointIndex,
		)
		if err != nil {
			return nil, err
		}

		outpoints = append(outpoints, outpoint)
	}

	return outpoints, nil
}

// outpointFromDB converts persisted hash/index columns into a wire outpoint.
func outpointFromDB(hashBytes []byte, index int32) (wire.OutPoint, error) {
	if len(hashBytes) != chainhash.HashSize {
		return wire.OutPoint{}, fmt.Errorf("outpoint hash must be %d "+
			"bytes, got %d", chainhash.HashSize, len(hashBytes))
	}

	if index < 0 {
		return wire.OutPoint{}, fmt.Errorf("outpoint index must be " +
			"non-negative")
	}

	var hash chainhash.Hash
	copy(hash[:], hashBytes)

	return wire.OutPoint{
		Hash:  hash,
		Index: uint32(index),
	}, nil
}

// ListRoundsPaginated returns a page of persisted round summaries ordered by
// round_id using cursor-based pagination. Filters are applied before ordering
// and limiting, so every returned page is filled from matching rows only.
func (s *RoundPersistenceStore) ListRoundsPaginated(ctx context.Context,
	query ListRoundsQuery) ([]RoundSummary, error) {

	readTxOpts := ReadTxOption()

	var result []RoundSummary

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		params := ListRoundsPaginatedParams{
			Cursor:        query.Cursor,
			StatusFilter:  query.Status,
			CreatedAfter:  query.CreatedAfter,
			CreatedBefore: query.CreatedBefore,
			LimitCount:    query.Limit,
		}

		dbRounds, err := q.ListRoundsPaginated(ctx, params)
		if err != nil {
			return fmt.Errorf("list rounds paginated: %w", err)
		}

		summaries := make([]RoundSummary, 0, len(dbRounds))
		for _, dbRound := range dbRounds {
			summary, err := roundSummaryFromRow(ctx, q, dbRound)
			if err != nil {
				return err
			}

			summaries = append(summaries, *summary)
		}

		result = summaries

		return nil
	})

	return result, err
}

// Compile-time checks that RoundPersistenceStore implements the interfaces.
var _ round.RoundStore = (*RoundPersistenceStore)(nil)
var _ round.VTXOStore = (*RoundPersistenceStore)(nil)
