package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/clock"
)

// RoundStoreDB implements rounds.RoundStore using sqlc-generated queries.
type RoundStoreDB struct {
	*TransactionExecutor[*sqlc.Queries]

	q     *sqlc.Queries
	clock clock.Clock
}

// ConfirmedRound is a persisted round that has confirmed on-chain.
type ConfirmedRound struct {
	// Round is the full round payload reconstructed from persistence.
	Round *rounds.Round

	// ConfirmationHeight is the height where the round transaction
	// confirmed.
	ConfirmationHeight int32
}

// NewRoundStoreDB creates a new RoundStoreDB from a Store.
func NewRoundStoreDB(store *Store, clk clock.Clock) *RoundStoreDB {
	txExec := NewTransactionExecutor(
		store, func(tx *sql.Tx) *sqlc.Queries {
			return store.WithTx(tx)
		}, store.log,
	)

	return &RoundStoreDB{
		TransactionExecutor: txExec,
		q:                   store.Queries,
		clock:               clk,
	}
}

// PersistRound saves a completed round to persistent storage.
// This is called after all signatures have been collected and the transaction
// is ready for broadcast.
func (r *RoundStoreDB) PersistRound(ctx context.Context,
	round *rounds.Round) error {

	now := r.clock.Now().Unix()

	return r.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		if round.FinalTx == nil {
			return fmt.Errorf("final tx is nil")
		}

		// Serialize final tx.
		var finalTxBytes []byte
		{
			var buf bytes.Buffer
			if err := round.FinalTx.Serialize(&buf); err != nil {
				return fmt.Errorf("serialize final tx: %w", err)
			}
			finalTxBytes = buf.Bytes()
		}

		// Get commitment txid as hex string (byte-reversed for
		// standard display).
		commitmentTxid := round.FinalTx.TxHash().String()

		// Serialize sweep parameters.
		if round.SweepKey == nil {
			return fmt.Errorf("sweep key is nil")
		}
		sweepKeyBytes := round.SweepKey.SerializeCompressed()

		// Insert main round. ChangeOutputIdx is captured here so a
		// rounds-actor restart can reload the ledger attribution
		// data on the reconstructed FinalizedState; defaulting to
		// -1 via the column default handles pre-migration rows.
		err := q.InsertRound(ctx, sqlc.InsertRoundParams{
			RoundID:         round.RoundID[:],
			FinalTx:         finalTxBytes,
			CommitmentTxid:  commitmentTxid,
			Status:          "pending",
			SweepKey:        sweepKeyBytes,
			CsvDelay:        int32(round.CSVDelay),
			CreatedAt:       now,
			UpdatedAt:       now,
			ChangeOutputIdx: round.ChangeOutputIdx,
		})
		if err != nil {
			return fmt.Errorf("insert round: %w", err)
		}

		// Persist the connector output index set so the
		// classifier can short-circuit external_deposit booking
		// on round-minted dust after a restart. Sort for
		// deterministic insertion order -- the table's PRIMARY
		// KEY already enforces uniqueness but stable order keeps
		// the on-disk row layout reproducible for test
		// snapshots.
		connectorIdxs := make(
			[]int32, len(round.ConnectorOutputIndices),
		)
		copy(connectorIdxs, round.ConnectorOutputIndices)
		sort.Slice(connectorIdxs, func(i, j int) bool {
			return connectorIdxs[i] < connectorIdxs[j]
		})
		for _, idx := range connectorIdxs {
			err := q.InsertRoundConnectorOutput(ctx,
				sqlc.InsertRoundConnectorOutputParams{
					RoundID:     round.RoundID[:],
					OutputIndex: idx,
				})
			if err != nil {
				return fmt.Errorf(
					"insert connector output %d: %w",
					idx, err,
				)
			}
		}

		// Insert VTXO trees.
		// Sort indices for deterministic insertion order.
		vtxoTreeIndices := make([]int, 0, len(round.VTXOTrees))
		for idx := range round.VTXOTrees {
			vtxoTreeIndices = append(vtxoTreeIndices, idx)
		}
		sort.Ints(vtxoTreeIndices)

		for _, idx := range vtxoTreeIndices {
			vtxoTree := round.VTXOTrees[idx]

			// Insert tree metadata.
			err := q.InsertRoundVTXOTree(ctx,
				sqlc.InsertRoundVTXOTreeParams{
					RoundID:          round.RoundID[:],
					BatchOutputIndex: int32(idx),
				})
			if err != nil {
				return fmt.Errorf("insert tree metadata %d: %w",
					idx, err)
			}

			// Insert tree structure recursively.
			err = SerializeTreeRecursive(
				ctx, q, round.RoundID, idx, vtxoTree,
			)
			if err != nil {
				return fmt.Errorf("serialize tree %d: %w",
					idx, err)
			}
		}

		// Insert connector descriptors.
		for _, desc := range round.ConnectorDescriptors {
			err = q.InsertRoundConnectorDescriptor(ctx,
				sqlc.InsertRoundConnectorDescriptorParams{
					RoundID: round.RoundID[:],
					OutputIndex: int32(
						desc.OutputIndex,
					),
					NumLeaves:     int32(desc.NumLeaves),
					ForfeitScript: desc.ForfeitScript,
					Radix:         int32(desc.Radix),
				})
			if err != nil {
				return fmt.Errorf(
					"insert connector descriptor at "+
						"output %d: %w",
					desc.OutputIndex, err,
				)
			}
		}

		// Insert client registrations.
		// Sort client IDs for deterministic insertion order.
		clientIDs := make(
			[]clientconn.ClientID, 0,
			len(round.ClientRegistrations),
		)
		for clientID := range round.ClientRegistrations {
			clientIDs = append(clientIDs, clientID)
		}
		sort.Slice(clientIDs, func(i, j int) bool {
			return clientIDs[i] < clientIDs[j]
		})

		for _, clientID := range clientIDs {
			reg := round.ClientRegistrations[clientID]
			regData, err := SerializeClientRegistration(reg)
			if err != nil {
				return fmt.Errorf(
					"serialize registration for %v: %w",
					clientID, err,
				)
			}

			err = q.InsertRoundClientRegistration(ctx,
				sqlc.InsertRoundClientRegistrationParams{
					RoundID:          round.RoundID[:],
					ClientID:         []byte(clientID),
					RegistrationData: regData,
				})
			if err != nil {
				return fmt.Errorf(
					"insert client registration %v: %w",
					clientID, err,
				)
			}
		}

		// Insert forfeit infos.
		// Sort outpoints for deterministic insertion order.
		outpoints := make(
			[]wire.OutPoint, 0, len(round.ForfeitInfos),
		)
		for outpoint := range round.ForfeitInfos {
			outpoints = append(outpoints, outpoint)
		}
		sort.Slice(outpoints, func(i, j int) bool {
			cmp := bytes.Compare(
				outpoints[i].Hash[:], outpoints[j].Hash[:],
			)
			if cmp == 0 {
				return outpoints[i].Index < outpoints[j].Index
			}

			return cmp < 0
		})

		for _, outpoint := range outpoints {
			info := round.ForfeitInfos[outpoint]

			var buf bytes.Buffer
			if err := info.ForfeitTx.Serialize(&buf); err != nil {
				return fmt.Errorf(
					"serialize forfeit tx: %w", err,
				)
			}
			forfeitTxBytes := buf.Bytes()

			err = q.InsertRoundForfeitInfo(ctx,
				sqlc.InsertRoundForfeitInfoParams{
					RoundID:       round.RoundID[:],
					OutpointHash:  outpoint.Hash[:],
					OutpointIndex: int32(outpoint.Index),
					ForfeitTx:     forfeitTxBytes,
					ConnectorOutputIndex: int32(
						info.ConnectorOutputIndex,
					),
					LeafIndex: int32(info.LeafIndex),
				})
			if err != nil {
				return fmt.Errorf("insert forfeit info: %w",
					err)
			}
		}

		return nil
	})
}

// MarkRoundConfirmed marks a pending round as confirmed with the block details
// for the broadcast commitment transaction.
func (r *RoundStoreDB) MarkRoundConfirmed(ctx context.Context,
	roundID rounds.RoundID, blockHeight int32,
	blockHash chainhash.Hash) error {

	return r.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.UpdateRoundConfirmed(
			ctx, sqlc.UpdateRoundConfirmedParams{
				RoundID: roundID[:],
				ConfirmationHeight: sql.NullInt32{
					Int32: blockHeight, Valid: true,
				},
				ConfirmationBlockHash: blockHash[:],
				UpdatedAt:             r.clock.Now().Unix(),
			},
		)
	})
}

// LoadPendingRounds returns all rounds that have been finalized but not yet
// confirmed on-chain. These rounds need to be reloaded into memory on restart
// so we can continue tracking them until confirmation.
func (r *RoundStoreDB) LoadPendingRounds(
	ctx context.Context) ([]*rounds.Round, error) {

	var result []*rounds.Round

	err := r.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := q.ListPendingRounds(ctx)
		if err != nil {
			return fmt.Errorf("list pending rounds: %w", err)
		}

		for _, row := range rows {
			round, err := loadRound(ctx, q, row.RoundID)
			if err != nil {
				return fmt.Errorf("load round %x: %w",
					row.RoundID, err)
			}
			result = append(result, round)
		}

		return nil
	})

	return result, err
}

// LoadConfirmedRounds returns all confirmed rounds with the confirmation
// height needed to restore batch watcher registrations after restart.
func (r *RoundStoreDB) LoadConfirmedRounds(
	ctx context.Context) ([]*ConfirmedRound, error) {

	const pageSize = 100

	var result []*ConfirmedRound

	err := r.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		for offset := int32(0); ; offset += pageSize {
			rows, err := q.ListRoundsByStatus(
				ctx, sqlc.ListRoundsByStatusParams{
					Status: "confirmed",
					Limit:  pageSize,
					Offset: offset,
				},
			)
			if err != nil {
				return fmt.Errorf(
					"list confirmed rounds: %w", err,
				)
			}

			for _, row := range rows {
				if !row.ConfirmationHeight.Valid {
					return fmt.Errorf(
						"confirmed round %x missing "+
							"confirmation height",
						row.RoundID,
					)
				}

				round, err := loadRound(ctx, q, row.RoundID)
				if err != nil {
					return fmt.Errorf(
						"load confirmed round %x: %w",
						row.RoundID, err,
					)
				}

				height := row.ConfirmationHeight.Int32
				result = append(result, &ConfirmedRound{
					Round:              round,
					ConfirmationHeight: height,
				})
			}

			if len(rows) < pageSize {
				break
			}
		}

		return nil
	})

	return result, err
}

// loadRound is a helper to reconstruct a Round from database rows.
func loadRound(ctx context.Context, q *sqlc.Queries,
	roundIDBytes []byte) (*rounds.Round, error) {

	// Load main round row.
	roundRow, err := q.GetRound(ctx, roundIDBytes)
	if err != nil {
		return nil, fmt.Errorf("get round: %w", err)
	}

	// Deserialize round ID.
	var roundID rounds.RoundID
	copy(roundID[:], roundRow.RoundID)

	// Deserialize final tx.
	var finalTx *wire.MsgTx
	if len(roundRow.FinalTx) > 0 {
		finalTx = &wire.MsgTx{}
		if err := finalTx.Deserialize(bytes.NewReader(
			roundRow.FinalTx,
		)); err != nil {
			return nil, fmt.Errorf("deserialize final tx: %w", err)
		}
	}
	if finalTx == nil {
		return nil, fmt.Errorf(
			"round %x missing final tx", roundIDBytes,
		)
	}

	// Deserialize sweep parameters.
	sweepKey, err := btcec.ParsePubKey(roundRow.SweepKey)
	if err != nil {
		return nil, fmt.Errorf("parse sweep key: %w", err)
	}
	csvDelay := uint32(roundRow.CsvDelay)

	// Compute sweep tapscript root for VTXO trees.
	sweepTapLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, csvDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("compute sweep tapscript: %w", err)
	}
	sweepTapRoot := sweepTapLeaf.TapHash()
	sweepTapRootBytes := sweepTapRoot[:]

	// Load and deserialize VTXO trees.
	treeMetaRows, err := q.GetRoundVTXOTrees(ctx, roundIDBytes)
	if err != nil {
		return nil, fmt.Errorf("get vtxo tree metadata: %w", err)
	}

	// Compute commitment txid for batch outpoints.
	commitmentTxid := finalTx.TxHash()

	vtxoTrees := make(map[int]*tree.Tree, len(treeMetaRows))
	for _, metaRow := range treeMetaRows {
		idx := int(metaRow.BatchOutputIndex)

		// Construct batch outpoint from commitment tx.
		batchOutpoint := wire.OutPoint{
			Hash:  commitmentTxid,
			Index: uint32(idx),
		}

		// Get batch output from commitment tx.
		if idx >= len(finalTx.TxOut) {
			return nil, fmt.Errorf(
				"batch output index %d out of range", idx,
			)
		}
		batchOutput := finalTx.TxOut[idx]

		// Deserialize tree from recursive storage.
		vtxoTree, err := DeserializeTreeRecursive(
			ctx, q, roundID, idx,
			batchOutpoint, batchOutput,
			sweepTapRootBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("deserialize tree %d: %w",
				idx, err)
		}
		vtxoTrees[idx] = vtxoTree
	}

	// Load connector descriptors.
	descRows, err := q.GetRoundConnectorDescriptors(ctx, roundIDBytes)
	if err != nil {
		return nil, fmt.Errorf("get connector descriptors: %w", err)
	}

	connectorDescriptors := make(
		[]*rounds.ConnectorTreeDescriptor, 0, len(descRows),
	)
	for _, descRow := range descRows {
		connectorDescriptors = append(connectorDescriptors,
			&rounds.ConnectorTreeDescriptor{
				OutputIndex:   int(descRow.OutputIndex),
				NumLeaves:     int(descRow.NumLeaves),
				ForfeitScript: descRow.ForfeitScript,
				Radix:         int(descRow.Radix),
			})
	}

	// Load and deserialize client registrations.
	regRows, err := q.GetRoundClientRegistrations(ctx, roundIDBytes)
	if err != nil {
		return nil, fmt.Errorf("get client registrations: %w", err)
	}

	clientRegistrations := make(
		map[rounds.ClientID]*rounds.ClientRegistration, len(regRows),
	)
	for _, regRow := range regRows {
		reg, err := DeserializeClientRegistration(
			regRow.RegistrationData,
		)
		if err != nil {
			return nil, fmt.Errorf("deserialize registration: %w",
				err)
		}
		clientRegistrations[reg.ClientID] = reg
	}

	// Load forfeit infos.
	forfeitRows, err := q.GetRoundForfeitInfos(ctx, roundIDBytes)
	if err != nil {
		return nil, fmt.Errorf("get forfeit infos: %w", err)
	}

	forfeitInfos := make(
		map[wire.OutPoint]*rounds.ForfeitInfo, len(forfeitRows),
	)
	for _, forfeitRow := range forfeitRows {
		var outpoint wire.OutPoint
		copy(outpoint.Hash[:], forfeitRow.OutpointHash)
		outpoint.Index = uint32(forfeitRow.OutpointIndex)

		var forfeitTx *wire.MsgTx
		if len(forfeitRow.ForfeitTx) > 0 {
			forfeitTx = &wire.MsgTx{}
			if err := forfeitTx.Deserialize(bytes.NewReader(
				forfeitRow.ForfeitTx,
			)); err != nil {
				return nil, fmt.Errorf(
					"deserialize forfeit tx: %w", err,
				)
			}
		}

		forfeitInfos[outpoint] = &rounds.ForfeitInfo{
			RoundID: roundID,
			ConnectorOutputIndex: int(
				forfeitRow.ConnectorOutputIndex,
			),
			LeafIndex: int(forfeitRow.LeafIndex),
			ForfeitTx: forfeitTx,
		}
	}

	// Load persisted connector output indices so the classifier
	// sees the full round-attributable output set after restart.
	connectorOutputIdxs, err := q.GetRoundConnectorOutputs(
		ctx, roundIDBytes,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"get connector output indices: %w", err,
		)
	}

	return &rounds.Round{
		RoundID:                roundID,
		FinalTx:                finalTx,
		VTXOTrees:              vtxoTrees,
		ConnectorDescriptors:   connectorDescriptors,
		ForfeitInfos:           forfeitInfos,
		ClientRegistrations:    clientRegistrations,
		SweepKey:               sweepKey,
		CSVDelay:               csvDelay,
		ChangeOutputIdx:        roundRow.ChangeOutputIdx,
		ConnectorOutputIndices: connectorOutputIdxs,
	}, nil
}
