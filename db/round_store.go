package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

// Type aliases for sqlc generated types.
type (
	RoundRow               = sqlc.Round
	RoundBoardingIntentRow = sqlc.RoundBoardingIntent
	RoundClientTreeRow     = sqlc.RoundClientTree
	RoundVtxoTemplateRow   = sqlc.RoundVtxoTemplate
	VTXORow                = sqlc.Vtxo
	InsertRoundParams      = sqlc.InsertRoundParams
	InsertVTXOParams       = sqlc.InsertVTXOParams
)

// RoundStore is the interface that groups all round-related database queries.
// This is a subset of sqlc.Querier focused on round operations.
//
//nolint:interfacebloat
type RoundStore interface {
	InsertRound(ctx context.Context, arg InsertRoundParams) error

	GetRound(ctx context.Context, roundID string) (RoundRow, error)

	GetRoundByCommitmentTxid(
		ctx context.Context, txid []byte,
	) (RoundRow, error)

	ListActiveRounds(ctx context.Context) ([]RoundRow, error)

	ListRoundsByStatus(ctx context.Context,
		status string) ([]RoundRow, error)

	UpdateRoundStatus(
		ctx context.Context, arg sqlc.UpdateRoundStatusParams,
	) error

	FinalizeRound(ctx context.Context, arg sqlc.FinalizeRoundParams) error

	InsertRoundBoardingIntent(
		ctx context.Context, arg sqlc.InsertRoundBoardingIntentParams,
	) error

	GetRoundBoardingIntents(
		ctx context.Context, roundID string,
	) ([]RoundBoardingIntentRow, error)

	InsertRoundVtxoTemplate(
		ctx context.Context, arg sqlc.InsertRoundVtxoTemplateParams,
	) error

	GetRoundVtxoTemplates(
		ctx context.Context, arg sqlc.GetRoundVtxoTemplatesParams,
	) ([]RoundVtxoTemplateRow, error)

	InsertRoundClientTree(
		ctx context.Context, arg sqlc.InsertRoundClientTreeParams,
	) error

	GetRoundClientTrees(
		ctx context.Context, roundID string,
	) ([]RoundClientTreeRow, error)

	InsertClientTreeTxid(
		ctx context.Context, arg sqlc.InsertClientTreeTxidParams,
	) error

	GetClientTreeByTxid(
		ctx context.Context, txid []byte,
	) (RoundClientTreeRow, error)

	InsertVTXO(ctx context.Context, arg InsertVTXOParams) error

	GetVTXO(ctx context.Context, arg sqlc.GetVTXOParams) (VTXORow, error)

	ListUnspentVTXOs(ctx context.Context) ([]VTXORow, error)

	MarkVTXOSpent(ctx context.Context, arg sqlc.MarkVTXOSpentParams) error

	// Include BoardingStore methods for fetching boarding intent details.
	GetBoardingIntent(
		ctx context.Context, arg BoardingIntentKey,
	) (BoardingIntentRow, error)

	GetBoardingAddress(
		ctx context.Context, pkScript []byte,
	) (BoardingAddrRow, error)
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
func (s *RoundPersistenceStore) CommitState(ctx context.Context,
	r *round.Round, state round.ClientState) error {

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
			return fmt.Errorf(
				"CommitState called with "+
					"non-InputSigSentState: %T", state,
			)
		}

		// Insert boarding intents for this round.
		if len(r.BoardingIntents) > 0 {
			numIntents := len(r.BoardingIntents)
			numSigs := len(inputSigState.InputSigs)
			if numIntents != numSigs {
				return fmt.Errorf(
					"mismatch between intents (%d) and "+
						"input sigs (%d)",
					numIntents, numSigs,
				)
			}

			for i, intent := range r.BoardingIntents {
				sig := inputSigState.InputSigs[i]
				iParams, err := s.domainIntentToRoundParams(
					r.RoundID.String(), &intent, i, sig,
				)
				if err != nil {
					return fmt.Errorf(
						"convert intent: %w", err,
					)
				}

				err = q.InsertRoundBoardingIntent(ctx, iParams)
				if err != nil {
					return fmt.Errorf(
						"insert round intent: %w", err,
					)
				}

				// Insert VTXO templates for this intent.
				for j, vtxoReq := range intent.VtxoTemplate {
					roundStr := r.RoundID.String()
					tParams := vtxoRequestToParams(
						roundStr, &intent, j, &vtxoReq,
					)
					err = q.InsertRoundVtxoTemplate(
						ctx, tParams,
					)
					if err != nil {
						return fmt.Errorf(
							"insert vtxo "+
								"template: %w",
							err,
						)
					}
				}
			}
		}

		// Insert client trees if present in the state.
		if inputSigState.ClientTrees != nil {
			for key, clientTree := range inputSigState.ClientTrees {
				treeData, err := SerializeTree(clientTree)
				if err != nil {
					return fmt.Errorf(
						"serialize client tree: %w",
						err,
					)
				}

				treeParams := sqlc.InsertRoundClientTreeParams{
					RoundID:   r.RoundID.String(),
					ClientKey: key[:],
					TreeData:  treeData,
				}
				err = q.InsertRoundClientTree(ctx, treeParams)
				if err != nil {
					return fmt.Errorf(
						"insert client tree: %w", err,
					)
				}

				// Extract and insert txids for this client
				// tree to enable efficient lookup by txid.
				txidEntries, err := clientTree.ExtractTxids()
				if err != nil {
					return fmt.Errorf(
						"extract client tree txids: %w",
						err,
					)
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
								"txid: %w", err,
						)
					}
				}
			}
		}

		return nil
	})
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
			return fmt.Errorf(
				"get round boarding intents: %w", err,
			)
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
func (s *RoundPersistenceStore) LookupRoundByCommitmentTx(
	ctx context.Context, txid chainhash.Hash) (*round.Round, error) {

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
			return fmt.Errorf(
				"get round boarding intents: %w", err,
			)
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
func (s *RoundPersistenceStore) ListActiveRounds(
	ctx context.Context) ([]*round.Round, error) {

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
				return fmt.Errorf(
					"get round boarding intents: %w", err,
				)
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
func (s *RoundPersistenceStore) ListConfirmedRounds(
	ctx context.Context) ([]*round.Round, error) {

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
				return fmt.Errorf(
					"get round boarding intents: %w", err,
				)
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
		for _, vtxo := range vtxos {
			params, err := s.domainVTXOToInsertParams(vtxo)
			if err != nil {
				return fmt.Errorf("convert VTXO: %w", err)
			}

			if err := q.InsertVTXO(ctx, params); err != nil {
				return fmt.Errorf("insert VTXO: %w", err)
			}
		}

		return nil
	})
}

// ListVTXOs returns all VTXOs currently owned by the client.
func (s *RoundPersistenceStore) ListVTXOs(
	ctx context.Context) ([]*round.ClientVTXO, error) {

	readTxOpts := ReadTxOption()

	var result []*round.ClientVTXO

	err := s.db.ExecTx(ctx, readTxOpts, func(q RoundStore) error {
		dbVTXOs, err := q.ListUnspentVTXOs(ctx)
		if err != nil {
			return fmt.Errorf("list VTXOs: %w", err)
		}

		vtxos := make([]*round.ClientVTXO, 0, len(dbVTXOs))
		for _, dbVTXO := range dbVTXOs {
			vtxo, err := s.dbVTXOToDomainVTXO(dbVTXO)
			if err != nil {
				return fmt.Errorf("convert VTXO: %w", err)
			}

			vtxos = append(vtxos, vtxo)
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

		vtxo, err := s.dbVTXOToDomainVTXO(dbVTXO)
		if err != nil {
			return err
		}

		result = vtxo

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

// dbRoundToDomainRound converts a database round row to a domain Round struct.
func (s *RoundPersistenceStore) dbRoundToDomainRound(ctx context.Context,
	q RoundStore, dbRound RoundRow,
	dbIntents []RoundBoardingIntentRow) (*round.Round, error) {

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
			return nil, fmt.Errorf(
				"deserialize commitment tx: %w", err,
			)
		}

		r.CommitmentTx = fn.Some(packet)
	}

	// Deserialize VTXO trees if present.
	if len(dbRound.VtxtTree) > 0 {
		vtxtTree, err := DeserializeTree(dbRound.VtxtTree)
		if err != nil {
			return nil, fmt.Errorf(
				"deserialize vtxt tree: %w", err,
			)
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
				return nil, fmt.Errorf(
					"convert round intent: %w", err,
				)
			}

			intents = append(intents, *intent)
		}

		r.BoardingIntents = intents
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
			return nil, fmt.Errorf(
				"deserialize conf tx: %w", err,
			)
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

	// Reconstruct BoardingRequest from relational columns.
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
		txProof, err := DeserializeTxProof(dbRoundIntent.TxProof)
		if err != nil {
			return nil, fmt.Errorf("deserialize tx proof: %w", err)
		}
		if txProof != nil {
			txProofOpt = fn.Some(*txProof)
		}
	}

	boardingReq := types.BoardingRequest{
		Outpoint:    &outpoint,
		ClientKey:   clientKey,
		OperatorKey: operatorKey,
		ExitDelay:   uint32(dbRoundIntent.ExitDelay),
		TxProof:     txProofOpt,
	}

	// Fetch and reconstruct VtxoTemplate from relational data.
	templateParams := sqlc.GetRoundVtxoTemplatesParams{
		RoundID:       dbRoundIntent.RoundID,
		OutpointHash:  dbRoundIntent.OutpointHash,
		OutpointIndex: dbRoundIntent.OutpointIndex,
	}
	dbTemplates, err := q.GetRoundVtxoTemplates(ctx, templateParams)
	if err != nil {
		return nil, fmt.Errorf("get vtxo templates: %w", err)
	}

	vtxoTemplate := make([]types.VTXORequest, 0, len(dbTemplates))
	for _, t := range dbTemplates {
		vtxoReq, err := dbTemplateToVTXORequest(t)
		if err != nil {
			return nil, fmt.Errorf("convert vtxo template: %w", err)
		}
		vtxoTemplate = append(vtxoTemplate, *vtxoReq)
	}

	intent := &round.BoardingIntent{
		BoardingIntent:  baseIntent,
		BoardingRequest: boardingReq,
		VtxoTemplate:    vtxoTemplate,
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
		return nil, fmt.Errorf(
			"unknown round status: %s", dbRound.Status,
		)
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
	}

	// Deserialize commitment tx.
	if len(dbRound.CommitmentTx) > 0 {
		reader := bytes.NewReader(dbRound.CommitmentTx)
		packet, err := psbt.NewFromRawBytes(reader, false)
		if err != nil {
			return nil, fmt.Errorf(
				"deserialize commitment tx: %w", err,
			)
		}

		state.CommitmentTx = packet
	}

	// Deserialize VTXO trees.
	if len(dbRound.VtxtTree) > 0 {
		vtxtTree, err := DeserializeTree(dbRound.VtxtTree)
		if err != nil {
			return nil, fmt.Errorf(
				"deserialize vtxt tree: %w", err,
			)
		}

		// For now, we store a single tree. Wrap it in a map at index 0.
		// TODO: Support proper multi-tree serialization format.
		state.VTXOTreePaths = map[int]*tree.Tree{0: vtxtTree}
	}

	// Convert boarding intents and input signatures.
	intents := make([]round.BoardingIntent, 0, len(dbIntents))
	inputSigs := make([]*types.BoardingInputSignature, 0, len(dbIntents))
	for _, dbIntent := range dbIntents {
		intent, err := s.dbRoundIntentToDomainIntent(ctx, q, dbIntent)
		if err != nil {
			return nil, fmt.Errorf(
				"convert round intent: %w", err,
			)
		}

		intents = append(intents, *intent)

		// Reconstruct the BoardingInputSignature from stored data.
		if len(dbIntent.InputSignature) > 0 {
			inputSig := dbIntent.InputSignature
			sig, err := schnorr.ParseSignature(inputSig)
			if err != nil {
				return nil, fmt.Errorf(
					"parse input signature: %w", err,
				)
			}

			var outpoint wire.OutPoint
			copy(outpoint.Hash[:], dbIntent.OutpointHash)
			outpoint.Index = uint32(dbIntent.OutpointIndex)

			inputSigs = append(
				inputSigs,
				&types.BoardingInputSignature{
					InputIndex:      int(dbIntent.InputIndex.Int32), //nolint:ll
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

	state.Intents = intents
	state.InputSigs = inputSigs

	// Deserialize client trees.
	for _, dbTree := range dbTrees {
		clientTree, err := DeserializeTree(dbTree.TreeData)
		if err != nil {
			return nil, fmt.Errorf(
				"deserialize client tree: %w", err,
			)
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
func (s *RoundPersistenceStore) domainIntentToRoundParams(
	roundID string, intent *round.BoardingIntent, inputIndex int,
	inputSig *types.BoardingInputSignature,
) (sqlc.InsertRoundBoardingIntentParams, error) {

	// Serialize TxProof if present.
	var txProofBytes []byte
	if intent.BoardingRequest.TxProof.IsSome() {
		p := intent.BoardingRequest.TxProof.UnsafeFromSome()
		data, err := SerializeTxProof(&p)
		if err != nil {
			return sqlc.InsertRoundBoardingIntentParams{},
				fmt.Errorf("serialize tx proof: %w", err)
		}
		txProofBytes = data
	}

	// Serialize public keys.
	var clientKey, operatorKey []byte
	if intent.BoardingRequest.ClientKey != nil {
		clientPk := intent.BoardingRequest.ClientKey
		clientKey = clientPk.SerializeCompressed()
	}
	if intent.BoardingRequest.OperatorKey != nil {
		opPk := intent.BoardingRequest.OperatorKey
		operatorKey = opPk.SerializeCompressed()
	}

	var inputIdxVal sql.NullInt32
	if inputIndex >= 0 {
		inputIdxVal = sql.NullInt32{
			Int32: int32(inputIndex),
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
		ExitDelay:      int32(intent.BoardingRequest.ExitDelay),
		TxProof:        txProofBytes,
		InputIndex:     inputIdxVal,
		InputSignature: inputSigBytes,
	}, nil
}

// vtxoRequestToParams converts a types.VTXORequest to sqlc insert parameters
// for the round_vtxo_templates table.
func vtxoRequestToParams(roundID string, intent *round.BoardingIntent,
	templateIndex int,
	req *types.VTXORequest) sqlc.InsertRoundVtxoTemplateParams {

	var clientPubkey, operatorPubkey, signingPubkey []byte
	if req.ClientKey != nil {
		clientPubkey = req.ClientKey.SerializeCompressed()
	}
	if req.OperatorKey != nil {
		operatorPubkey = req.OperatorKey.SerializeCompressed()
	}
	if req.SigningKey.PubKey != nil {
		signingPubkey = req.SigningKey.PubKey.SerializeCompressed()
	}

	return sqlc.InsertRoundVtxoTemplateParams{
		RoundID:          roundID,
		OutpointHash:     intent.Outpoint.Hash[:],
		OutpointIndex:    int32(intent.Outpoint.Index),
		TemplateIndex:    int32(templateIndex),
		Amount:           int64(req.Amount),
		PkScript:         req.PkScript,
		Expiry:           int32(req.Expiry),
		ClientPubkey:     clientPubkey,
		OperatorPubkey:   operatorPubkey,
		SigningKeyFamily: int32(req.SigningKey.KeyLocator.Family),
		SigningKeyIndex:  int32(req.SigningKey.KeyLocator.Index),
		SigningPubkey:    signingPubkey,
	}
}

// dbTemplateToVTXORequest converts a database template row to a VTXORequest.
func dbTemplateToVTXORequest(
	t RoundVtxoTemplateRow) (*types.VTXORequest, error) {

	var clientKey, operatorKey, signingPubkey *btcec.PublicKey
	var err error

	if len(t.ClientPubkey) > 0 {
		clientKey, err = btcec.ParsePubKey(t.ClientPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse client pubkey: %w", err)
		}
	}
	if len(t.OperatorPubkey) > 0 {
		operatorKey, err = btcec.ParsePubKey(t.OperatorPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse operator pubkey: %w", err)
		}
	}
	if len(t.SigningPubkey) > 0 {
		signingPubkey, err = btcec.ParsePubKey(t.SigningPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse signing pubkey: %w", err)
		}
	}

	return &types.VTXORequest{
		Amount:      btcutil.Amount(t.Amount),
		PkScript:    t.PkScript,
		Expiry:      uint32(t.Expiry),
		ClientKey:   clientKey,
		OperatorKey: operatorKey,
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
// parameters.
func (s *RoundPersistenceStore) domainVTXOToInsertParams(
	vtxo *round.ClientVTXO) (InsertVTXOParams, error) {

	// Serialize tree path.
	var treePathBytes []byte
	if vtxo.TreePath != nil {
		data, err := SerializeTree(vtxo.TreePath)
		if err != nil {
			return InsertVTXOParams{}, fmt.Errorf(
				"serialize tree path: %w", err,
			)
		}

		treePathBytes = data
	}

	roundIDStr := ""
	vtxo.RoundID.WhenSome(func(rid round.RoundID) {
		roundIDStr = rid.String()
	})

	var operatorPubkey []byte
	if vtxo.OperatorKey != nil {
		operatorPubkey = vtxo.OperatorKey.SerializeCompressed()
	}

	var clientPubkey []byte
	if vtxo.ClientKey.PubKey != nil {
		clientPubkey = vtxo.ClientKey.PubKey.SerializeCompressed()
	}

	nowUnix := s.clock.Now().Unix()

	return InsertVTXOParams{
		OutpointHash:    vtxo.Outpoint.Hash[:],
		OutpointIndex:   int32(vtxo.Outpoint.Index),
		RoundID:         roundIDStr,
		Amount:          int64(vtxo.Amount),
		PkScript:        vtxo.PkScript,
		Expiry:          int32(vtxo.Expiry),
		ClientKeyFamily: int32(vtxo.ClientKey.Family),
		ClientKeyIndex:  int32(vtxo.ClientKey.Index),
		ClientPubkey:    clientPubkey,
		OperatorPubkey:  operatorPubkey,
		TreePath:        treePathBytes,
		Spent:           false,
		CreationTime:    nowUnix,
		LastUpdateTime:  nowUnix,
	}, nil
}

// dbVTXOToDomainVTXO converts a database VTXO row to a domain ClientVTXO.
func (s *RoundPersistenceStore) dbVTXOToDomainVTXO(
	dbVTXO VTXORow) (*round.ClientVTXO, error) {

	var outpointHash chainhash.Hash
	copy(outpointHash[:], dbVTXO.OutpointHash)

	outpoint := wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(dbVTXO.OutpointIndex),
	}

	// Parse client public key.
	var clientPubkey *btcec.PublicKey
	if len(dbVTXO.ClientPubkey) > 0 {
		key, err := btcec.ParsePubKey(dbVTXO.ClientPubkey)
		if err != nil {
			return nil, fmt.Errorf(
				"parse client pubkey: %w", err,
			)
		}

		clientPubkey = key
	}

	// Parse operator public key.
	var operatorPubkey *btcec.PublicKey
	if len(dbVTXO.OperatorPubkey) > 0 {
		key, err := btcec.ParsePubKey(dbVTXO.OperatorPubkey)
		if err != nil {
			return nil, fmt.Errorf(
				"parse operator pubkey: %w", err,
			)
		}

		operatorPubkey = key
	}

	// Deserialize tree path.
	var treePath *tree.Tree
	if len(dbVTXO.TreePath) > 0 {
		t, err := DeserializeTree(dbVTXO.TreePath)
		if err != nil {
			return nil, fmt.Errorf(
				"deserialize tree path: %w", err,
			)
		}

		treePath = t
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

	return &round.ClientVTXO{
		Outpoint: outpoint,
		Amount:   btcutil.Amount(dbVTXO.Amount),
		PkScript: dbVTXO.PkScript,
		Expiry:   uint32(dbVTXO.Expiry),
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientPubkey,
			KeyLocator: keychain.KeyLocator{
				Family: keyFamily,
				Index:  uint32(dbVTXO.ClientKeyIndex),
			},
		},
		OperatorKey: operatorPubkey,
		TreePath:    treePath,
		RoundID:     roundIDOpt,
	}, nil
}

// serializeCommitmentTx serializes a commitment transaction PSBT if present.
// Returns the serialized bytes and txid, or nil slices if the Option is None.
func serializeCommitmentTx(
	txOpt fn.Option[*psbt.Packet]) ([]byte, []byte, error) {

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
func serializeVTXOTreePaths(
	treesOpt fn.Option[map[int]*tree.Tree],
) ([]byte, error) {

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

// Compile-time checks that RoundPersistenceStore implements the interfaces.
var _ round.RoundStore = (*RoundPersistenceStore)(nil)
var _ round.VTXOStore = (*RoundPersistenceStore)(nil)
