package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// Type aliases for sqlc generated types.
type (
	RoundRow                  = sqlc.Round
	RoundBoardingIntentRow    = sqlc.RoundBoardingIntent
	RoundClientTreeRow        = sqlc.RoundClientTree
	RoundVtxoRequestRow       = sqlc.RoundVtxoRequest
	VTXORow                   = sqlc.Vtxo
	InsertRoundParams         = sqlc.InsertRoundParams
	InsertVTXOParams          = sqlc.InsertVTXOParams
	ListRoundsPaginatedParams = sqlc.ListRoundsPaginatedParams

	// ClearAnchorParams aliases the sqlc-generated clear-by-outpoint
	// params so call sites can spell the type concisely (the generated
	// name exceeds the line-length cap when nested inside the
	// CommitState transaction body).
	ClearAnchorParams = sqlc.ClearPendingIntentAnchorByOutpointParams
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
	// InternalKeyQuerier lets the round and VTXO stores register and
	// hydrate wallet keys via the shared internal_keys registry within
	// their own transactions.
	InternalKeyQuerier

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

	GetClientTreeByTxid(ctx context.Context,
		txid []byte) (RoundClientTreeRow, error)

	InsertVTXO(ctx context.Context, arg InsertVTXOParams) error

	GetVTXO(ctx context.Context, arg sqlc.GetVTXOParams) (VTXORow, error)

	ListUnspentVTXOs(ctx context.Context) ([]VTXORow, error)

	MarkVTXOSpent(ctx context.Context, arg sqlc.MarkVTXOSpentParams) error

	// VTXO lifecycle status queries.
	ListLiveVTXOs(ctx context.Context) ([]VTXORow, error)

	ListVTXOsByStatus(ctx context.Context,
		status int32) ([]sqlc.ListVTXOsByStatusRow, error)

	// ListVTXOSelectionCandidatesByStatus returns the lightweight
	// (outpoint, amount, pkScript) projection coin selection runs on,
	// avoiding the full descriptor decode on the per-payment hot path.
	ListVTXOSelectionCandidatesByStatus(ctx context.Context,
		status int32) (
		[]sqlc.ListVTXOSelectionCandidatesByStatusRow,
		error,
	)

	UpdateVTXOStatus(
		ctx context.Context, arg sqlc.UpdateVTXOStatusParams,
	) error

	// DeleteSpendingReservation removes a spending-reservation row so the
	// VTXO store can atomically update a VTXO's status and drop its
	// reservation in the same transaction when it leaves SpendingState.
	DeleteSpendingReservation(ctx context.Context,
		arg sqlc.DeleteSpendingReservationParams) error

	MarkVTXOForfeiting(
		ctx context.Context, arg sqlc.MarkVTXOForfeitingParams,
	) error

	// ListForfeitingVTXOsByRound returns the Forfeiting VTXOs bound to a
	// round, used to rebuild a reloaded round's forfeit set on restart.
	ListForfeitingVTXOsByRound(ctx context.Context,
		forfeitRoundID sql.NullString) (
		[]sqlc.ListForfeitingVTXOsByRoundRow, error)

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

	// ClearPendingIntentAnchorByOutpoint deletes the pending-intent
	// anchor row bound to one outpoint. Called from CommitState in the
	// same transaction that records the round adopting the anchored
	// intent (boarding outpoints for Board, forfeit VTXO outpoints for
	// SendOnChain), so a persisted intent can never be replayed after
	// the round it landed in has durably checkpointed.
	ClearPendingIntentAnchorByOutpoint(ctx context.Context,
		arg sqlc.ClearPendingIntentAnchorByOutpointParams) error

	// DeleteOrphanedPendingBoardIntents / DeleteOrphanedPendingSendIntents
	// sweep per-kind detail rows whose anchors have all been cleared.
	// Detail rows foreign-key the header, so they must be deleted before
	// DeleteOrphanedPendingIntents removes the now-anchorless headers.
	DeleteOrphanedPendingBoardIntents(ctx context.Context) error

	DeleteOrphanedPendingSendIntents(ctx context.Context) error

	// DeleteOrphanedPendingIntents sweeps pending-intent header rows
	// whose anchors have all been cleared. Called from CommitState after
	// the anchor clears (and detail sweeps) above so fully-adopted
	// intents vanish in the same transaction.
	DeleteOrphanedPendingIntents(ctx context.Context) error

	// MarkPendingSendIntentFailedByOutpoint terminally fails the pending
	// send intent anchored to one outpoint, recording the reason and typed
	// code. Called from FailForfeitIntents when a round fails terminally so
	// the intent stops replaying and its activity entry can surface as
	// failed. Anchors are retained so the failed job stays correlatable by
	// its consumed outpoint.
	MarkPendingSendIntentFailedByOutpoint(ctx context.Context,
		arg sqlc.MarkPendingSendIntentFailedByOutpointParams) error

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

// CommitState atomically persists both the round data and FSM state. This
// should be called at the "point of no return" when the client has sent
// partial signatures and the server may broadcast.
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

		// The point-of-no-return checkpoint is defined by this state.
		// Extract it before writing the round so every immutable term
		// needed to resume the ceremony is persisted with that row.
		inputSigState, ok := state.(*round.InputSigSentState)
		if !ok {
			return fmt.Errorf("CommitState called with "+
				"non-InputSigSentState: %T", state)
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

			// Stamp the round flow version. Versions are
			// zero-indexed, so an unstamped round reads as V1
			// (the zero value) with no normalization needed.
			FlowVersion: int32(r.FlowVersion),

			// SweepDelay is delivered per round and must survive a
			// restart. A process-wide default may have changed and
			// is not authoritative for an already signed round.
			SweepDelay: int32(inputSigState.SweepDelay),
		}
		if err := q.InsertRound(ctx, roundParams); err != nil {
			return fmt.Errorf("insert round: %w", err)
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

				// Clear the pending-intent anchor bound to
				// this outpoint in the same transaction.
				// Once the intent is Adopted, the user's
				// Board call is durably checkpointed in the
				// round itself; the anchor is no longer
				// load-bearing and a stale row would
				// otherwise rebind a future Board replay to
				// an unrelated boarding deposit.
				err = q.ClearPendingIntentAnchorByOutpoint(
					ctx, ClearAnchorParams{
						OutpointHash: intent.Outpoint.
							Hash[:],
						OutpointIndex: int32(
							intent.Outpoint.Index,
						),
					},
				)
				if err != nil {
					return fmt.Errorf("clear pending "+
						"intent anchor for adopted "+
						"boarding intent: %w", err)
				}
			}
		}

		// Clear the pending-intent anchors bound to this round's
		// forfeited VTXO outpoints, then sweep any intent left without
		// anchors, so a fully-adopted intent disappears atomically
		// with the checkpoint that adopted it.
		if err := clearForfeitIntentAnchors(ctx, q, r); err != nil {
			return err
		}

		// Insert VTXO requests for this round.
		for i, vtxoReq := range r.Intents.VTXOs {
			reqParams, err := vtxoRequestToRoundParams(
				ctx, q, s.clock.Now().Unix(),
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

		return nil
	})
}

// clearForfeitIntentAnchors clears the pending-intent anchors bound to the
// forfeited VTXO outpoints this round consumes (e.g. a SendOnChain intent's
// reserved forfeit set), then sweeps any intent left without anchors. Once
// the round is durably checkpointed at the point of no return, the user's
// intent is carried by the round itself; the outbox row must not outlive it
// or a restart would replay an already-adopted send. Per-kind detail rows
// foreign-key the header, so they are swept before the headers.
func clearForfeitIntentAnchors(ctx context.Context, q RoundStore,
	r *round.Round) error {

	outpoints := make([]wire.OutPoint, 0, len(r.Intents.Forfeits))
	for _, forfeit := range r.Intents.Forfeits {
		if forfeit.VTXOOutpoint == nil {
			continue
		}

		outpoints = append(outpoints, *forfeit.VTXOOutpoint)
	}

	return clearForfeitIntentAnchorsByOutpoints(ctx, q, outpoints)
}

// clearForfeitIntentAnchorsByOutpoints clears the pending-intent anchors bound
// to the given forfeited VTXO outpoints, then sweeps any pending intent left
// without anchors. It backs the success path (clearForfeitIntentAnchors, from
// CommitState): once a round adopts the reserved forfeit set, the originating
// job's outbox row must go, or a restart would replay an already-adopted send.
// The terminal-failure path deliberately does not use this helper; it marks the
// intent 'failed' and keeps the anchors (see FailForfeitIntents) so the record
// stays a durable, correlatable entry rather than being deleted. Per-kind
// detail rows foreign-key the header, so they are swept before the headers.
func clearForfeitIntentAnchorsByOutpoints(ctx context.Context, q RoundStore,
	outpoints []wire.OutPoint) error {

	for _, op := range outpoints {
		err := q.ClearPendingIntentAnchorByOutpoint(
			ctx, ClearAnchorParams{
				OutpointHash:  op.Hash[:],
				OutpointIndex: int32(op.Index),
			},
		)
		if err != nil {
			return fmt.Errorf("clear pending intent anchor for "+
				"forfeited vtxo: %w", err)
		}
	}

	if err := q.DeleteOrphanedPendingBoardIntents(ctx); err != nil {
		return fmt.Errorf("sweep orphaned board intents: %w", err)
	}

	if err := q.DeleteOrphanedPendingSendIntents(ctx); err != nil {
		return fmt.Errorf("sweep orphaned send intents: %w", err)
	}

	if err := q.DeleteOrphanedPendingIntents(ctx); err != nil {
		return fmt.Errorf("sweep orphaned pending intents: %w", err)
	}

	return nil
}

// FailForfeitIntents terminally fails the pending send intents anchored to the
// given forfeited VTXO outpoints, recording the reason and typed failure code,
// all in one write transaction. It is the terminal-failure counterpart to the
// anchor clear CommitState performs on the success path: when a round fails
// terminally (e.g. the operator cannot fund the commitment tx) the originating
// job must not keep replaying into the same wall. Marking (rather than
// deleting) the intent both stops the replay — ListPendingSendIntents skips
// non-pending rows — and leaves a durable, anchor-correlatable record the
// activity projection surfaces as failed with the reason. It is a no-op for an
// empty outpoint set.
func (s *RoundPersistenceStore) FailForfeitIntents(ctx context.Context,
	outpoints []wire.OutPoint, reason string,
	code round.RoundFailureCode) error {

	if len(outpoints) == 0 {
		return nil
	}

	failureReason := sql.NullString{String: reason, Valid: reason != ""}

	writeTxOpts := WriteTxOption()

	return s.db.ExecTx(ctx, writeTxOpts, func(q RoundStore) error {
		return markSendIntentsFailedByOutpoint(
			ctx, q, outpoints, failureReason, int32(code),
		)
	})
}

// markSendIntentsFailedByOutpoint marks the pending send intent anchored to
// each outpoint as failed, recording the shared reason and code. Extracted from
// FailForfeitIntents' write-tx closure so the generated method call sits at a
// shallow enough indentation to read.
func markSendIntentsFailedByOutpoint(ctx context.Context, q RoundStore,
	outpoints []wire.OutPoint, reason sql.NullString, code int32) error {

	for _, op := range outpoints {
		params := sqlc.MarkPendingSendIntentFailedByOutpointParams{
			OutpointHash:  op.Hash[:],
			OutpointIndex: int32(op.Index),
			FailureReason: reason,
			FailureCode:   code,
		}

		err := q.MarkPendingSendIntentFailedByOutpoint(ctx, params)
		if err != nil {
			return fmt.Errorf("mark pending send intent failed "+
				"for forfeited vtxo: %w", err)
		}
	}

	return nil
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
			params, err := s.domainVTXOToInsertParams(ctx, q, cv)
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

// roundForfeitRequests rebuilds the forfeit set of a reloaded round from the
// VTXO table. Standard wallet forfeits are the only entries that can survive
// a restart: each Forfeiting VTXO row carries the binding forfeit_round_id,
// while custom (caller-supplied) forfeit inputs never enter the wallet store
// and their in-memory signing contexts die with the process. The rebuilt
// requests carry just the outpoint and amount, which is exactly what the
// status-reconcile release path needs to return the inputs to LiveState.
func roundForfeitRequests(ctx context.Context, q RoundStore,
	roundID string) ([]types.ForfeitRequest, error) {

	rows, err := q.ListForfeitingVTXOsByRound(ctx, sql.NullString{
		String: roundID,
		Valid:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("list forfeiting vtxos: %w", err)
	}

	if len(rows) == 0 {
		return nil, nil
	}

	forfeits := make([]types.ForfeitRequest, 0, len(rows))
	for _, row := range rows {
		var op wire.OutPoint
		copy(op.Hash[:], row.OutpointHash)
		op.Index = uint32(row.OutpointIndex)

		forfeits = append(forfeits, types.ForfeitRequest{
			VTXOOutpoint: &op,
			Amount:       btcutil.Amount(row.Amount),
		})
	}

	return forfeits, nil
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
		FlowVersion: roundpb.FlowVersion(dbRound.FlowVersion),
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

	// Rebuild the forfeit set from the VTXO table. Without this, a
	// reloaded forfeit-bearing round looks boarding-only: the actor's
	// restart re-arm guard never arms the status-reconcile timer, and a
	// DEAD verdict would release an empty set, silently reopening the
	// wavelength#844 strand across every restart.
	forfeits, err := roundForfeitRequests(ctx, q, dbRound.RoundID)
	if err != nil {
		return nil, err
	}
	r.Intents.Forfeits = forfeits

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
	baseAddr, err := dbAddrToDomainAddr(ctx, q, s.chainParams, dbAddr)
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
// NOTE: Only 'input_sig_sent' status is supported for full FSM reconstruction.
// This is intentional per the "point of no return" persistence design:
//   - Before sending input signatures, the client can safely abort and rejoin
//     a new round with no data loss.
//   - After sending input signatures (status='input_sig_sent'), the server may
//     broadcast the commitment transaction at any time. The client must track
//     confirmation to detect when VTXOs become spendable.
//
// Terminal statuses (confirmed, failed, archived) return minimal state objects
// to indicate the round's final status. If we encounter an unknown status, it
// indicates data corruption or a version mismatch that must be surfaced as an
// error.
func (s *RoundPersistenceStore) reconstructFSMState(ctx context.Context,
	q RoundStore, dbRound RoundRow, dbIntents []RoundBoardingIntentRow,
	dbTrees []RoundClientTreeRow) (round.ClientState, error) {

	switch dbRound.Status {
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
		SweepDelay:  uint32(dbRound.SweepDelay),

		// Carry the persisted flow version onto the reconstructed
		// state so a mid-round resume does not silently downgrade it.
		FlowVersion: roundpb.FlowVersion(dbRound.FlowVersion),
	}

	// Deserialize commitment tx.
	if len(dbRound.CommitmentTx) > 0 {
		reader := bytes.NewReader(dbRound.CommitmentTx)
		packet, err := psbt.NewFromRawBytes(reader, false)
		if err != nil {
			return nil, fmt.Errorf("deserialize commitment tx: %w",
				err)
		}

		state.CommitmentTx = packet
	}

	// Deserialize VTXO trees.
	if len(dbRound.VtxtTree) > 0 {
		vtxtTree, err := DeserializeTree(dbRound.VtxtTree)
		if err != nil {
			return nil, fmt.Errorf("deserialize vtxt tree: %w", err)
		}

		// For now, we store a single tree. Wrap it in a map at index 0.
		// TODO: Support proper multi-tree serialization format.
		state.VTXOTreePaths = map[int]*tree.Tree{0: vtxtTree}
	}

	// Fetch VTXO requests for this round.
	dbVtxoReqs, err := q.GetRoundVtxoRequests(ctx, dbRound.RoundID)
	if err != nil {
		return nil, fmt.Errorf("get round vtxo requests: %w", err)
	}

	vtxos := make([]types.VTXORequest, 0, len(dbVtxoReqs))
	for _, dbReq := range dbVtxoReqs {
		req, err := dbVtxoRequestRowToVTXORequest(ctx, q, dbReq)
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

	// Rebuild the forfeit set alongside the boarding intents. The
	// reconstructed InputSigSentState gates every status-reconcile
	// decision on its forfeit count, so leaving Forfeits empty here would
	// turn the post-restart reconcile into a no-op.
	forfeits, err := roundForfeitRequests(ctx, q, dbRound.RoundID)
	if err != nil {
		return nil, err
	}

	state.Intents = round.Intents{
		Boarding: intents,
		VTXOs:    vtxos,
		Forfeits: forfeits,
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
func vtxoRequestToRoundParams(ctx context.Context, q RoundStore, now int64,
	roundID string, requestIndex int,
	req *types.VTXORequest) (sqlc.InsertRoundVtxoRequestParams, error) {

	pkScript, err := req.EffectivePkScript()
	if err != nil {
		return sqlc.InsertRoundVtxoRequestParams{}, fmt.Errorf(
			"derive VTXO pkScript: %w", err)
	}

	var (
		expiry         = int32(req.Expiry)
		clientPubkey   = []byte{}
		operatorPubkey = []byte{}
	)
	if req.ClientKey != nil {
		clientPubkey = req.ClientKey.SerializeCompressed()
	}
	if req.OperatorKey != nil {
		operatorPubkey = req.OperatorKey.SerializeCompressed()
	}

	standardParams, err := req.DecodeStandardPolicyTemplate()
	if err == nil {
		expiry = int32(standardParams.ExitDelay)
		clientPubkey = standardParams.OwnerKey.SerializeCompressed()
		operatorPubkey = standardParams.OperatorKey.
			SerializeCompressed()
	}
	// A decode error here is expected for caller-supplied custom policy
	// outputs such as vHTLC refresh replacements. The policy template and
	// pkScript above remain the authoritative persisted script data; these
	// legacy client/operator/expiry columns are populated only for the
	// standard VTXO shape.

	// Register the signing descriptor in the shared internal_keys
	// registry and reference it by id. A request always carries a
	// signing key.
	var signingKeyID sql.NullInt64
	if req.SigningKey.PubKey != nil {
		id, err := RegisterInternalKeyTx(ctx, q, now, req.SigningKey)
		if err != nil {
			return sqlc.InsertRoundVtxoRequestParams{}, fmt.Errorf(
				"register "+
					"signing key: %w", err)
		}

		signingKeyID = sql.NullInt64{Int64: id, Valid: true}
	}

	// Register the local owner descriptor when present. A foreign-owned
	// request has no local owner descriptor, so owner_key_id stays NULL --
	// the replacement for the old -1/-1 sentinel locator. The owner pubkey
	// is the policy owner key (formerly the client_pubkey column) paired
	// with the request's owner locator, matching the descriptor the old
	// read path reconstructed.
	var ownerKeyID sql.NullInt64
	if req.OwnerKey.PubKey != nil {
		ownerDesc := req.OwnerKey
		if standardParams != nil {
			ownerDesc.PubKey = standardParams.OwnerKey
		}

		id, err := RegisterInternalKeyTx(ctx, q, now, ownerDesc)
		if err != nil {
			return sqlc.InsertRoundVtxoRequestParams{}, fmt.Errorf(
				"register "+
					"owner key: %w", err)
		}

		ownerKeyID = sql.NullInt64{Int64: id, Valid: true}
	}

	policyTemplate, err := req.EffectivePolicyTemplate()
	if err != nil {
		return sqlc.InsertRoundVtxoRequestParams{}, fmt.Errorf(
			"encode VTXO policy template: %w", err)
	}

	return sqlc.InsertRoundVtxoRequestParams{
		RoundID:        roundID,
		RequestIndex:   int32(requestIndex),
		Amount:         int64(req.Amount),
		PkScript:       pkScript,
		Expiry:         expiry,
		PolicyTemplate: policyTemplate,
		ClientPubkey:   clientPubkey,
		OperatorPubkey: operatorPubkey,
		OwnerKeyID:     ownerKeyID,
		SigningKeyID:   signingKeyID,
	}, nil
}

// dbVtxoRequestRowToVTXORequest converts a database row to a VTXORequest. The
// query handle hydrates the owner and signing descriptors from the
// internal_keys registry via their FKs.
func dbVtxoRequestRowToVTXORequest(ctx context.Context, q RoundStore,
	t RoundVtxoRequestRow) (*types.VTXORequest, error) {

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

	// Hydrate the signing descriptor from the registry. The pubkey carried
	// by the registry row is authoritative over the inline client_pubkey.
	var signingKey keychain.KeyDescriptor
	if t.SigningKeyID.Valid {
		desc, err := InternalKeyDescByIDTx(ctx, q, t.SigningKeyID.Int64)
		if err != nil {
			return nil, fmt.Errorf("hydrate signing key: %w", err)
		}

		signingKey = desc
	}

	// Hydrate the owner descriptor when present. A NULL owner_key_id marks
	// a foreign-owned request with no local owner descriptor.
	var ownerKey keychain.KeyDescriptor
	if t.OwnerKeyID.Valid {
		desc, err := InternalKeyDescByIDTx(ctx, q, t.OwnerKeyID.Int64)
		if err != nil {
			return nil, fmt.Errorf("hydrate owner key: %w", err)
		}

		ownerKey = desc
	}

	return &types.VTXORequest{
		Amount:         btcutil.Amount(t.Amount),
		PolicyTemplate: bytes.Clone(t.PolicyTemplate),
		PkScript:       bytes.Clone(t.PkScript),
		ClientKey:      clientPubkey,
		OwnerKey:       ownerKey,
		Expiry:         uint32(t.Expiry),
		OperatorKey:    operatorPubkey,
		SigningKey:     signingKey,
	}, nil
}

// domainVTXOToInsertParams converts a round.ClientVTXO to sqlc insert
// parameters. The single round-direct ancestry is persisted separately
// in the vtxo_ancestry_paths side table; callers must call
// upsertRoundClientVTXOAncestry alongside InsertVTXO inside the same
// transaction.
func (s *RoundPersistenceStore) domainVTXOToInsertParams(ctx context.Context,
	q RoundStore, vtxo *round.ClientVTXO) (InsertVTXOParams, error) {

	roundIDStr := ""
	vtxo.RoundID.WhenSome(func(rid round.RoundID) {
		roundIDStr = rid.String()
	})

	var operatorPubkey []byte
	if vtxo.OperatorKey != nil {
		operatorPubkey = vtxo.OperatorKey.SerializeCompressed()
	}

	nowUnix := s.clock.Now().Unix()

	// Register the local-ownership (owner) key in the shared internal_keys
	// registry and reference it by id. A round-create row may carry no
	// owner pubkey yet; in that case the FK stays NULL until the VTXO
	// manager heals the row with the full descriptor.
	var clientKeyID sql.NullInt64
	if vtxo.OwnerKey.PubKey != nil {
		id, err := RegisterInternalKeyTx(ctx, q, nowUnix, vtxo.OwnerKey)
		if err != nil {
			return InsertVTXOParams{}, fmt.Errorf("register "+
				"owner key: %w", err)
		}

		clientKeyID = sql.NullInt64{Int64: id, Valid: true}
	}

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
		OutpointHash:   vtxo.Outpoint.Hash[:],
		OutpointIndex:  int32(vtxo.Outpoint.Index),
		RoundID:        roundIDStr,
		Amount:         int64(vtxo.Amount),
		PkScript:       vtxo.PkScript,
		Expiry:         int32(vtxo.Expiry),
		PolicyTemplate: policyTemplate,
		ClientKeyID:    clientKeyID,
		OperatorPubkey: operatorPubkey,
		BatchExpiry:    vtxo.BatchExpiry,
		ChainDepth:     0,
		CreatedHeight:  vtxo.CreatedHeight,
		CommitmentTxid: vtxo.CommitmentTxID[:],
		Spent:          false,
		CreationTime:   nowUnix,
		LastUpdateTime: nowUnix,

		// Round-created VTXOs are built under the current construction
		// version (V1 today). Versions are zero-indexed, so V1 is the
		// zero value; stamp it explicitly here as well as on the
		// descriptor-heal path so the write-once construction_version
		// is set deliberately at creation rather than defaulted.
		ConstructionVersion: int32(arkrpc.ConstructionVersionV1),
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

	// Hydrate the owner key descriptor (pubkey + locator) from the
	// internal_keys registry via the client_key_id FK, and parse the
	// operator pubkey from its inline column. These preserve the wallet's
	// exact compressed pubkeys. When a semantic policy template is present
	// we use it to fill in missing keys and derive the canonical expiry,
	// but we don't want to overwrite stored owner/operator pubkeys with
	// x-only lifts from policy decoding.
	var ownerKey keychain.KeyDescriptor
	var operatorPubkey *btcec.PublicKey
	expiry := uint32(dbVTXO.Expiry)
	if dbVTXO.ClientKeyID.Valid {
		desc, err := InternalKeyDescByIDTx(
			ctx, q, dbVTXO.ClientKeyID.Int64,
		)
		if err != nil {
			return nil, fmt.Errorf("hydrate owner key: %w", err)
		}

		ownerKey = desc
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

			if ownerKey.PubKey == nil {
				ownerKey.PubKey = params.OwnerKey
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
		OwnerKey:       ownerKey,
		OperatorKey:    operatorPubkey,
		Ancestry:       ancestry,
		RoundID:        roundIDOpt,
		CommitmentTxID: commitmentTxID,
		BatchExpiry:    dbVTXO.BatchExpiry,
		CreatedHeight:  dbVTXO.CreatedHeight,
		BusinessRevision: uint64(
			dbVTXO.BusinessRevision,
		),
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
