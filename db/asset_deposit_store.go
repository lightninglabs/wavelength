package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/keychain"
)

// AssetDepositStatus is the lifecycle stage of an asset deposit.
type AssetDepositStatus string

const (
	// AssetDepositFunding is a deposit tapd has not published yet. A
	// failed attempt stays here, and a retry replays it.
	AssetDepositFunding AssetDepositStatus = "funding"

	// AssetDepositPublished is a deposit whose output tapd published.
	AssetDepositPublished AssetDepositStatus = "published"

	// AssetDepositConfirmed is a deposit whose output's proof confirmed.
	AssetDepositConfirmed AssetDepositStatus = "confirmed"
)

// maxAssetDepositBlob bounds one encoded proof file or witness item.
const maxAssetDepositBlob = 16 << 20

var (
	// ErrAssetDepositNotFound reports an unknown deposit request ID.
	ErrAssetDepositNotFound = errors.New("asset deposit not found")

	// ErrAssetDepositMismatch reports a request ID reused for a
	// different deposit, or a lifecycle update that contradicts the
	// stored one.
	ErrAssetDepositMismatch = errors.New("asset deposit does not match " +
		"the stored deposit")
)

// AssetDepositRequest is everything an asset deposit is funded from. It is
// replayed verbatim on retry, because the onboarding journal is bound to it.
type AssetDepositRequest struct {
	RequestID          string
	AssetRef           string
	AssetAmount        uint64
	CarrierValueSat    uint64
	FeeRateSatPerVByte uint64
	TargetConf         uint32
	MaxFeeSat          uint64
	OperatorKey        *btcec.PublicKey
	ExitDelay          uint32
	FundingProofs      [][]byte
}

// AssetDepositOutput is the published deposit output and the claim that
// boards it into a round.
type AssetDepositOutput struct {
	Outpoint         wire.OutPoint
	ValueSat         int64
	FeeSat           uint64
	PkScript         []byte
	PolicyTemplate   []byte
	OwnerKey         keychain.KeyDescriptor
	Digest           [32]byte
	ScriptKey        []byte
	TaprootAssetRoot chainhash.Hash
	OPTrueWitness    [][]byte
}

// AssetDeposit is one stored asset deposit.
type AssetDeposit struct {
	Request         AssetDepositRequest
	Status          AssetDepositStatus
	Output          *AssetDepositOutput
	ProofFile       []byte
	ConfirmedHeight uint32
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AssetDepositStore persists asset deposits.
type AssetDepositStore struct {
	*TransactionExecutor[*sqlc.Queries]
	now func() time.Time
}

// NewAssetDepositStore creates an AssetDepositStore from a Store.
func NewAssetDepositStore(store *Store) *AssetDepositStore {
	return &AssetDepositStore{
		TransactionExecutor: NewTransactionExecutor(
			store.BaseDB(),
			func(tx *sql.Tx) *sqlc.Queries {
				return store.Queries().WithTx(tx)
			},
			store.log,
		),
		now: time.Now,
	}
}

// CreateAssetDeposit records a deposit request before it is funded. Reusing a
// request ID returns the stored deposit if its request is the same, and
// ErrAssetDepositMismatch otherwise.
func (s *AssetDepositStore) CreateAssetDeposit(ctx context.Context,
	req *AssetDepositRequest) (*AssetDeposit, error) {

	if err := validateAssetDepositRequest(req); err != nil {
		return nil, err
	}
	proofs, err := encodeAssetDepositBlobs(req.FundingProofs)
	if err != nil {
		return nil, err
	}

	var deposit *AssetDeposit
	err = s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		_, err := q.InsertAssetDeposit(
			ctx, sqlc.InsertAssetDepositParams{
				RequestID: req.RequestID,
				AssetRef:  req.AssetRef,
				AssetAmount: encodeAssetDepositAmount(
					req.AssetAmount,
				),
				CarrierValueSat: int64(req.CarrierValueSat),
				FeeRateSatPerVbyte: int64(
					req.FeeRateSatPerVByte,
				),
				TargetConf: int32(req.TargetConf),
				MaxFeeSat:  int64(req.MaxFeeSat),
				OperatorKey: req.OperatorKey.
					SerializeCompressed(),
				ExitDelay:     int32(req.ExitDelay),
				FundingProofs: proofs,
				CreatedAt:     s.now().Unix(),
			},
		)
		if err != nil {
			return err
		}

		row, err := q.GetAssetDeposit(ctx, req.RequestID)
		if err != nil {
			return err
		}
		deposit, err = assetDepositFromRow(row)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("create asset deposit: %w", err)
	}
	if !assetDepositRequestsEqual(&deposit.Request, req) {
		return nil, fmt.Errorf("%w: request %s",
			ErrAssetDepositMismatch, req.RequestID)
	}

	return deposit, nil
}

// GetAssetDeposit loads one deposit by request ID.
func (s *AssetDepositStore) GetAssetDeposit(ctx context.Context,
	requestID string) (*AssetDeposit, error) {

	var deposit *AssetDeposit
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		row, err := q.GetAssetDeposit(ctx, requestID)
		if err != nil {
			return err
		}
		deposit, err = assetDepositFromRow(row)

		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAssetDepositNotFound
	}
	if err != nil {
		return nil, err
	}

	return deposit, nil
}

// ListAssetDeposits returns every deposit, oldest first.
func (s *AssetDepositStore) ListAssetDeposits(ctx context.Context) (
	[]*AssetDeposit, error) {

	return s.listAssetDeposits(ctx,
		func(q *sqlc.Queries) ([]sqlc.AssetDeposit, error) {
			return q.ListAssetDeposits(ctx)
		},
	)
}

// ListAssetDepositsByStatus returns the deposits at one status, oldest first.
func (s *AssetDepositStore) ListAssetDepositsByStatus(ctx context.Context,
	status AssetDepositStatus) ([]*AssetDeposit, error) {

	return s.listAssetDeposits(ctx,
		func(q *sqlc.Queries) ([]sqlc.AssetDeposit, error) {
			return q.ListAssetDepositsByStatus(ctx, string(status))
		},
	)
}

// listAssetDeposits decodes the rows a list query returns.
func (s *AssetDepositStore) listAssetDeposits(ctx context.Context,
	list func(*sqlc.Queries) ([]sqlc.AssetDeposit, error)) ([]*AssetDeposit,
	error) {

	var deposits []*AssetDeposit
	err := s.ExecTx(ctx, ReadTxOption(), func(q *sqlc.Queries) error {
		rows, err := list(q)
		if err != nil {
			return err
		}
		deposits = make([]*AssetDeposit, 0, len(rows))
		for _, row := range rows {
			deposit, err := assetDepositFromRow(row)
			if err != nil {
				return err
			}
			deposits = append(deposits, deposit)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return deposits, nil
}

// MarkAssetDepositPublished stores the published output of a funding deposit.
// Repeating the same update is a no-op.
func (s *AssetDepositStore) MarkAssetDepositPublished(ctx context.Context,
	requestID string, output *AssetDepositOutput) error {

	if err := validateAssetDepositOutput(output); err != nil {
		return err
	}
	witness, err := encodeAssetDepositBlobs(output.OPTrueWitness)
	if err != nil {
		return err
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		moved, err := q.MarkAssetDepositPublished(
			ctx, sqlc.MarkAssetDepositPublishedParams{
				RequestID: requestID,
				OutpointHash: bytes.Clone(
					output.Outpoint.Hash[:],
				),
				OutpointIndex: sqlInt32(
					int32(output.Outpoint.Index),
				),
				ValueSat:       sqlInt64(output.ValueSat),
				FeeSat:         sqlInt64(int64(output.FeeSat)),
				PkScript:       output.PkScript,
				PolicyTemplate: output.PolicyTemplate,
				OwnerKeyFamily: sqlInt32(
					int32(output.OwnerKey.Family),
				),
				OwnerKeyIndex: sqlInt32(
					int32(output.OwnerKey.Index),
				),
				OwnerPubkey: output.OwnerKey.PubKey.
					SerializeCompressed(),
				Digest:           bytes.Clone(output.Digest[:]),
				ScriptKey:        output.ScriptKey,
				TaprootAssetRoot: output.TaprootAssetRoot[:],
				OpTrueWitness:    witness,
				UpdatedAt:        s.now().Unix(),
			},
		)
		if err != nil || moved == 1 {
			return err
		}

		row, err := q.GetAssetDeposit(ctx, requestID)
		if err != nil {
			return err
		}
		stored, err := assetDepositFromRow(row)
		if err != nil {
			return err
		}
		if stored.Output == nil ||
			stored.Output.Outpoint != output.Outpoint {
			return fmt.Errorf("%w: %s is %s",
				ErrAssetDepositMismatch, requestID,
				stored.Status)
		}

		return nil
	})
}

// MarkAssetDepositConfirmed stores the confirmed proof of a published
// deposit. Repeating the same update is a no-op.
func (s *AssetDepositStore) MarkAssetDepositConfirmed(ctx context.Context,
	requestID string, proofFile []byte, height uint32) error {

	if len(proofFile) == 0 || height == 0 || height > math.MaxInt32 {
		return fmt.Errorf("confirmed asset deposit needs a proof and " +
			"a height")
	}

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		moved, err := q.MarkAssetDepositConfirmed(
			ctx, sqlc.MarkAssetDepositConfirmedParams{
				RequestID:       requestID,
				ProofFile:       proofFile,
				ConfirmedHeight: sqlInt32(int32(height)),
				UpdatedAt:       s.now().Unix(),
			},
		)
		if err != nil || moved == 1 {
			return err
		}

		row, err := q.GetAssetDeposit(ctx, requestID)
		if err != nil {
			return err
		}
		if row.Status != string(AssetDepositConfirmed) ||
			!bytes.Equal(row.ProofFile, proofFile) {
			return fmt.Errorf("%w: %s is %s",
				ErrAssetDepositMismatch, requestID, row.Status)
		}

		return nil
	})
}

// SetAssetDepositError records the latest failure of an unconfirmed deposit.
func (s *AssetDepositStore) SetAssetDepositError(ctx context.Context, requestID,
	message string) error {

	return s.ExecTx(ctx, WriteTxOption(), func(q *sqlc.Queries) error {
		return q.SetAssetDepositError(
			ctx, sqlc.SetAssetDepositErrorParams{
				RequestID: requestID,
				LastError: message,
				UpdatedAt: s.now().Unix(),
			},
		)
	})
}

// validateAssetDepositRequest checks the fields the table constrains.
func validateAssetDepositRequest(req *AssetDepositRequest) error {
	switch {
	case req == nil:
		return fmt.Errorf("asset deposit request is required")

	case req.RequestID == "" || len(req.RequestID) > 128:
		return fmt.Errorf("asset deposit request ID must be 1 to 128 " +
			"bytes")

	case req.AssetRef == "" || req.AssetAmount == 0:
		return fmt.Errorf("asset deposit needs an asset and an amount")

	case req.CarrierValueSat == 0 ||
		req.CarrierValueSat > math.MaxInt64 ||
		req.FeeRateSatPerVByte > math.MaxInt64 ||
		req.MaxFeeSat > math.MaxInt64 ||
		req.TargetConf > math.MaxInt32:
		return fmt.Errorf("asset deposit amounts are out of range")

	case req.OperatorKey == nil || req.ExitDelay == 0 ||
		req.ExitDelay > math.MaxInt32:
		return fmt.Errorf("asset deposit needs an operator key and " +
			"an exit delay")

	case len(req.FundingProofs) == 0:
		return fmt.Errorf("asset deposit needs funding proofs")
	}

	return nil
}

// validateAssetDepositOutput checks the fields the table constrains.
func validateAssetDepositOutput(output *AssetDepositOutput) error {
	switch {
	case output == nil:
		return fmt.Errorf("asset deposit output is required")

	case output.ValueSat <= 0 || output.FeeSat > math.MaxInt64 ||
		output.Outpoint.Index > math.MaxInt32:
		return fmt.Errorf("asset deposit output amounts are out of " +
			"range")

	case len(output.PkScript) == 0 || len(output.PolicyTemplate) == 0 ||
		len(output.ScriptKey) != 33 ||
		len(output.OPTrueWitness) == 0:
		return fmt.Errorf("asset deposit output is incomplete")

	case output.OwnerKey.PubKey == nil ||
		output.OwnerKey.Index > math.MaxInt32 ||
		output.OwnerKey.Family > math.MaxInt32:
		return fmt.Errorf("asset deposit output needs an owner key")
	}

	return nil
}

// assetDepositFromRow decodes one stored deposit.
func assetDepositFromRow(row sqlc.AssetDeposit) (*AssetDeposit, error) {
	if len(row.AssetAmount) != 8 {
		return nil, fmt.Errorf("asset deposit %s amount is malformed",
			row.RequestID)
	}
	operatorKey, err := btcec.ParsePubKey(row.OperatorKey)
	if err != nil {
		return nil, fmt.Errorf("asset deposit %s operator key: %w",
			row.RequestID, err)
	}
	proofs, err := decodeAssetDepositBlobs(row.FundingProofs)
	if err != nil {
		return nil, fmt.Errorf("asset deposit %s funding proofs: %w",
			row.RequestID, err)
	}

	deposit := &AssetDeposit{
		Request: AssetDepositRequest{
			RequestID: row.RequestID,
			AssetRef:  row.AssetRef,
			AssetAmount: binary.BigEndian.Uint64(
				row.AssetAmount,
			),
			CarrierValueSat:    uint64(row.CarrierValueSat),
			FeeRateSatPerVByte: uint64(row.FeeRateSatPerVbyte),
			TargetConf:         uint32(row.TargetConf),
			MaxFeeSat:          uint64(row.MaxFeeSat),
			OperatorKey:        operatorKey,
			ExitDelay:          uint32(row.ExitDelay),
			FundingProofs:      proofs,
		},
		Status:          AssetDepositStatus(row.Status),
		ProofFile:       row.ProofFile,
		ConfirmedHeight: uint32(row.ConfirmedHeight.Int32),
		LastError:       row.LastError,
		CreatedAt:       time.Unix(row.CreatedAt, 0),
		UpdatedAt:       time.Unix(row.UpdatedAt, 0),
	}
	if len(row.OutpointHash) == 0 {
		return deposit, nil
	}

	output, err := assetDepositOutputFromRow(row)
	if err != nil {
		return nil, fmt.Errorf("asset deposit %s output: %w",
			row.RequestID, err)
	}
	deposit.Output = output

	return deposit, nil
}

// assetDepositOutputFromRow decodes the published output of a stored deposit.
func assetDepositOutputFromRow(row sqlc.AssetDeposit) (*AssetDepositOutput,
	error) {

	hash, err := chainhash.NewHash(row.OutpointHash)
	if err != nil {
		return nil, err
	}
	ownerKey, err := btcec.ParsePubKey(row.OwnerPubkey)
	if err != nil {
		return nil, err
	}
	witness, err := decodeAssetDepositBlobs(row.OpTrueWitness)
	if err != nil {
		return nil, err
	}
	if len(row.Digest) != 32 || len(row.TaprootAssetRoot) != 32 {
		return nil, fmt.Errorf("asset commitment is malformed")
	}

	output := &AssetDepositOutput{
		Outpoint: wire.OutPoint{
			Hash:  *hash,
			Index: uint32(row.OutpointIndex.Int32),
		},
		ValueSat:       row.ValueSat.Int64,
		FeeSat:         uint64(row.FeeSat.Int64),
		PkScript:       row.PkScript,
		PolicyTemplate: row.PolicyTemplate,
		OwnerKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(
					row.OwnerKeyFamily.Int32,
				),
				Index: uint32(row.OwnerKeyIndex.Int32),
			},
			PubKey: ownerKey,
		},
		ScriptKey:     row.ScriptKey,
		OPTrueWitness: witness,
	}
	copy(output.Digest[:], row.Digest)
	copy(output.TaprootAssetRoot[:], row.TaprootAssetRoot)

	return output, nil
}

// assetDepositRequestsEqual reports whether two requests fund the same
// deposit.
func assetDepositRequestsEqual(a, b *AssetDepositRequest) bool {
	if a.RequestID != b.RequestID || a.AssetRef != b.AssetRef ||
		a.AssetAmount != b.AssetAmount ||
		a.CarrierValueSat != b.CarrierValueSat ||
		a.FeeRateSatPerVByte != b.FeeRateSatPerVByte ||
		a.TargetConf != b.TargetConf || a.MaxFeeSat != b.MaxFeeSat ||
		a.ExitDelay != b.ExitDelay ||
		!a.OperatorKey.IsEqual(b.OperatorKey) ||
		len(a.FundingProofs) != len(b.FundingProofs) {
		return false
	}
	for idx := range a.FundingProofs {
		if !bytes.Equal(a.FundingProofs[idx], b.FundingProofs[idx]) {
			return false
		}
	}

	return true
}

// encodeAssetDepositAmount encodes a uint64 asset amount for storage.
func encodeAssetDepositAmount(amount uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, amount)
}

// encodeAssetDepositBlobs encodes a list of byte strings as a witness vector.
func encodeAssetDepositBlobs(blobs [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := wire.WriteVarInt(&buf, 0, uint64(len(blobs))); err != nil {
		return nil, err
	}
	for _, blob := range blobs {
		if len(blob) > maxAssetDepositBlob {
			return nil, fmt.Errorf("asset deposit item of %d "+
				"bytes exceeds %d", len(blob),
				maxAssetDepositBlob)
		}
		if err := wire.WriteVarBytes(&buf, 0, blob); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeAssetDepositBlobs decodes a witness vector of byte strings.
func decodeAssetDepositBlobs(data []byte) ([][]byte, error) {
	reader := bytes.NewReader(data)
	count, err := wire.ReadVarInt(reader, 0)
	if err != nil {
		return nil, err
	}
	if count > uint64(reader.Len()) {
		return nil, fmt.Errorf("item count %d exceeds the encoding",
			count)
	}

	blobs := make([][]byte, 0, count)
	for range count {
		blob, err := wire.ReadVarBytes(
			reader, 0, maxAssetDepositBlob, "asset deposit item",
		)
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, blob)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes", reader.Len())
	}

	return blobs, nil
}
