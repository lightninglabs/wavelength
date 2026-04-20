package ledger

import (
	"fmt"
	"io"
	"math"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type constants for ledger actor messages. These use the
// 0x8xxx range to avoid collisions with other actor subsystems.
const (
	roundConfirmedTLVType tlv.Type = 0x8001
	vtxosForfeitedTLVType tlv.Type = 0x8002
	sweepCompletedTLVType tlv.Type = 0x8003
	oorFinalizedTLVType   tlv.Type = 0x8004
	blockEpochTLVType     tlv.Type = 0x8005
)

// Per-message TLV record types. Each message defines its own
// field types using odd values for forward-compatible
// extensibility -- a future PR that adds a field picks the next
// unused odd number and old decoders ignore it without breaking
// durable-mailbox replay.
const (
	// RoundConfirmedMsg field types.
	roundConfirmedRoundIDType     tlv.Type = 1
	roundConfirmedTotalVTXOType   tlv.Type = 3
	roundConfirmedVTXOCountType   tlv.Type = 5
	roundConfirmedBoardingFeeType tlv.Type = 7
	roundConfirmedMiningFeeType   tlv.Type = 9
	roundConfirmedBlockHeightType tlv.Type = 11

	// VTXOsForfeitedMsg field types.
	vtxosForfeitedRoundIDType     tlv.Type = 1
	vtxosForfeitedTotalAmountType tlv.Type = 3
	vtxosForfeitedCountType       tlv.Type = 5
	vtxosForfeitedRefreshFeeType  tlv.Type = 7

	// SweepCompletedMsg field types.
	sweepCompletedBatchIDType     tlv.Type = 1
	sweepCompletedReclaimedType   tlv.Type = 3
	sweepCompletedCountType       tlv.Type = 5
	sweepCompletedBlockHeightType tlv.Type = 7
	sweepCompletedFeeRateType     tlv.Type = 9
	sweepCompletedMiningFeeType   tlv.Type = 11

	// OORFinalizedMsg field types.
	oorFinalizedSessionIDType tlv.Type = 1
	oorFinalizedInputAmtType  tlv.Type = 3
	oorFinalizedOutputAmtType tlv.Type = 5

	// BlockEpochMsg field types.
	blockEpochHeightType tlv.Type = 1
	blockEpochHashType   tlv.Type = 3
)

// decodeAmountSat narrows a TLV-decoded uint64 satoshi field to
// the int64 domain used by every Record* helper downstream. A
// TLV stream is producer-sourced and can carry any 64-bit value,
// so a malformed payload with the high bit set would silently
// underflow through int64(amountSat) and produce a negative
// value that later code (SQL CHECK, validateAmounts) would reject
// with a cryptic error. Catching it at Decode gives every message
// a single rejection point that maps to ErrInvalidMessage.
func decodeAmountSat(field string, amountSat uint64) (int64, error) {
	if amountSat > math.MaxInt64 {
		return 0, fmt.Errorf(
			"%w: %s %d exceeds int64 range",
			ErrInvalidMessage, field, amountSat,
		)
	}

	return int64(amountSat), nil
}

// decodeCount narrows a TLV-decoded uint32 count field to int32.
// Counts are never negative in practice, but downstream code
// holds them as int32 for Go-idiomatic arithmetic; a producer
// that sends a value above MaxInt32 is malformed and the message
// is rejected at Decode rather than silently overflowing into a
// negative count that feeds the treasury tracker.
func decodeCount(field string, count uint32) (int32, error) {
	if count > math.MaxInt32 {
		return 0, fmt.Errorf(
			"%w: %s %d exceeds int32 range",
			ErrInvalidMessage, field, count,
		)
	}

	return int32(count), nil
}

// decodeFixedBytes validates that a TLV-decoded variable-length
// byte field carries exactly the expected number of bytes. The
// TLV wire format is length-prefixed so a caller can serialize a
// too-short or too-long blob for a field the receiver expects at
// a fixed width (e.g. 16-byte RoundID or 32-byte SessionID).
// Reject those explicitly so a malformed payload surfaces as
// ErrInvalidMessage at the Decode boundary rather than silently
// producing truncated or zero-padded state.
//
// A zero-length slice is treated as "field absent" so the
// caller-level absent semantics (zero-valued arrays) survive a
// round-trip over TLV without triggering rejection.
func decodeFixedBytes(field string, got []byte, want int) error {
	if len(got) == 0 {
		return nil
	}

	if len(got) != want {
		return fmt.Errorf(
			"%w: %s has %d bytes, expected %d",
			ErrInvalidMessage, field, len(got), want,
		)
	}

	return nil
}

// LedgerMsg is the message constraint for the ledger durable
// actor mailbox. It embeds actor.TLVMessage so both application
// messages and the framework RestartMessage satisfy this
// interface.
type LedgerMsg interface {
	actor.TLVMessage
}

// LedgerResp is the response type for the ledger actor. The
// ledger actor is fire-and-forget, so responses are always nil.
type LedgerResp interface {
	actor.Message

	ledgerRespSealed()
}

// RoundConfirmedMsg is sent when a round is confirmed on-chain
// and VTXOs become live. The ledger actor records capital
// deployment, boarding fees collected, and mining fees paid.
type RoundConfirmedMsg struct {
	actor.BaseMessage

	// RoundID is the 16-byte round UUID.
	RoundID [16]byte

	// TotalVTXOAmountSat is the sum of all VTXO values created
	// in this round.
	TotalVTXOAmountSat int64

	// VTXOCount is the number of VTXOs created.
	VTXOCount int32

	// BoardingFeeSat is the total boarding fee collected from
	// clients (totalInput - totalOutput for boarding inputs).
	BoardingFeeSat int64

	// MiningFeeSat is the on-chain mining fee paid for the
	// round commitment transaction.
	MiningFeeSat int64

	// BlockHeight is the confirmation block height.
	BlockHeight uint32
}

// MessageType returns the message type name for routing.
func (m *RoundConfirmedMsg) MessageType() string {
	return "RoundConfirmedMsg"
}

// TLVType returns the TLV type tag used by the durable mailbox
// codec to route this message.
func (m *RoundConfirmedMsg) TLVType() tlv.Type {
	return roundConfirmedTLVType
}

// Encode serializes the message as a TLV stream.
func (m *RoundConfirmedMsg) Encode(w io.Writer) error {
	roundID := m.RoundID[:]
	totalVTXO := uint64(m.TotalVTXOAmountSat)
	vtxoCount := uint32(m.VTXOCount)
	boardingFee := uint64(m.BoardingFeeSat)
	miningFee := uint64(m.MiningFeeSat)
	blockHeight := m.BlockHeight

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			roundConfirmedRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedTotalVTXOType, &totalVTXO,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedVTXOCountType, &vtxoCount,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedBoardingFeeType, &boardingFee,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedMiningFeeType, &miningFee,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedBlockHeightType, &blockHeight,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
//
// Mechanically similar to other message Decodes; per-message
// field lists cannot share a helper without losing the
// named-argument clarity of decodeAmountSat.
//
//nolint:dupl
func (m *RoundConfirmedMsg) Decode(r io.Reader) error {
	var (
		roundID     []byte
		totalVTXO   uint64
		vtxoCount   uint32
		boardingFee uint64
		miningFee   uint64
		blockHeight uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			roundConfirmedRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedTotalVTXOType, &totalVTXO,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedVTXOCountType, &vtxoCount,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedBoardingFeeType, &boardingFee,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedMiningFeeType, &miningFee,
		),
		tlv.MakePrimitiveRecord(
			roundConfirmedBlockHeightType, &blockHeight,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode RoundConfirmedMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"RoundConfirmedMsg.RoundID", roundID, len(m.RoundID),
	); err != nil {
		return err
	}

	total, err := decodeAmountSat(
		"RoundConfirmedMsg.TotalVTXOAmountSat", totalVTXO,
	)
	if err != nil {
		return err
	}

	count, err := decodeCount(
		"RoundConfirmedMsg.VTXOCount", vtxoCount,
	)
	if err != nil {
		return err
	}

	boarding, err := decodeAmountSat(
		"RoundConfirmedMsg.BoardingFeeSat", boardingFee,
	)
	if err != nil {
		return err
	}

	mining, err := decodeAmountSat(
		"RoundConfirmedMsg.MiningFeeSat", miningFee,
	)
	if err != nil {
		return err
	}

	copy(m.RoundID[:], roundID)
	m.TotalVTXOAmountSat = total
	m.VTXOCount = count
	m.BoardingFeeSat = boarding
	m.MiningFeeSat = mining
	m.BlockHeight = blockHeight

	return nil
}

// VTXOsForfeitedMsg is sent when VTXOs are forfeited during a
// refresh or spend. The ledger actor records the refresh fee
// and updates the treasury tracker.
type VTXOsForfeitedMsg struct {
	actor.BaseMessage

	// RoundID is the round that consumed the forfeited VTXOs.
	RoundID [16]byte

	// TotalAmountSat is the sum of all forfeited VTXO values.
	TotalAmountSat int64

	// Count is the number of forfeited VTXOs.
	Count int32

	// RefreshFeeSat is the fee collected for the refresh
	// (input - output difference attributable to forfeits).
	RefreshFeeSat int64
}

// MessageType returns the message type name for routing.
func (m *VTXOsForfeitedMsg) MessageType() string {
	return "VTXOsForfeitedMsg"
}

// TLVType returns the TLV type tag used by the durable mailbox
// codec to route this message.
func (m *VTXOsForfeitedMsg) TLVType() tlv.Type {
	return vtxosForfeitedTLVType
}

// Encode serializes the message as a TLV stream.
func (m *VTXOsForfeitedMsg) Encode(w io.Writer) error {
	roundID := m.RoundID[:]
	totalAmount := uint64(m.TotalAmountSat)
	count := uint32(m.Count)
	refreshFee := uint64(m.RefreshFeeSat)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxosForfeitedRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedTotalAmountType, &totalAmount,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedCountType, &count,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedRefreshFeeType, &refreshFee,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *VTXOsForfeitedMsg) Decode(r io.Reader) error {
	var (
		roundID     []byte
		totalAmount uint64
		count       uint32
		refreshFee  uint64
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxosForfeitedRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedTotalAmountType, &totalAmount,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedCountType, &count,
		),
		tlv.MakePrimitiveRecord(
			vtxosForfeitedRefreshFeeType, &refreshFee,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode VTXOsForfeitedMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"VTXOsForfeitedMsg.RoundID", roundID, len(m.RoundID),
	); err != nil {
		return err
	}

	total, err := decodeAmountSat(
		"VTXOsForfeitedMsg.TotalAmountSat", totalAmount,
	)
	if err != nil {
		return err
	}

	c, err := decodeCount("VTXOsForfeitedMsg.Count", count)
	if err != nil {
		return err
	}

	refresh, err := decodeAmountSat(
		"VTXOsForfeitedMsg.RefreshFeeSat", refreshFee,
	)
	if err != nil {
		return err
	}

	copy(m.RoundID[:], roundID)
	m.TotalAmountSat = total
	m.Count = c
	m.RefreshFeeSat = refresh

	return nil
}

// SweepCompletedMsg is sent when the operator sweeps expired
// VTXOs back into the wallet. The ledger actor records both
// capital reclamation and the absolute on-chain mining fee paid
// for the sweep transaction.
type SweepCompletedMsg struct {
	actor.BaseMessage

	// BatchID is the 16-byte batch UUID.
	BatchID [16]byte

	// ReclaimedAmountSat is the total value reclaimed.
	ReclaimedAmountSat int64

	// Count is the number of outputs swept.
	Count int32

	// BlockHeight is the sweep confirmation height.
	BlockHeight uint32

	// FeeRateSatVB is the fee rate paid for the sweep tx.
	// Retained for observability; the absolute mining fee is
	// booked via MiningFeeSat so handlers do not need to
	// multiply feerate by vbytes themselves.
	FeeRateSatVB int64

	// MiningFeeSat is the absolute on-chain mining fee paid
	// for the sweep transaction. Producers populate this
	// directly instead of deriving it from FeeRateSatVB so the
	// ledger sees the authoritative value the producer already
	// computed. The handler books a mining_fees leg when
	// MiningFeeSat > 0; a zero value is accepted (e.g. the
	// producer has not yet captured the fee) and the leg is
	// skipped.
	MiningFeeSat int64
}

// MessageType returns the message type name for routing.
func (m *SweepCompletedMsg) MessageType() string {
	return "SweepCompletedMsg"
}

// TLVType returns the TLV type tag used by the durable mailbox
// codec to route this message.
func (m *SweepCompletedMsg) TLVType() tlv.Type {
	return sweepCompletedTLVType
}

// Encode serializes the message as a TLV stream.
func (m *SweepCompletedMsg) Encode(w io.Writer) error {
	batchID := m.BatchID[:]
	reclaimed := uint64(m.ReclaimedAmountSat)
	count := uint32(m.Count)
	blockHeight := m.BlockHeight
	feeRate := uint64(m.FeeRateSatVB)
	miningFee := uint64(m.MiningFeeSat)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			sweepCompletedBatchIDType, &batchID,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedReclaimedType, &reclaimed,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedCountType, &count,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedFeeRateType, &feeRate,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedMiningFeeType, &miningFee,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
//
// Mechanically similar to other message Decodes; per-message
// field lists cannot share a helper without losing the
// named-argument clarity of decodeAmountSat.
//
//nolint:dupl
func (m *SweepCompletedMsg) Decode(r io.Reader) error {
	var (
		batchID     []byte
		reclaimed   uint64
		count       uint32
		blockHeight uint32
		feeRate     uint64
		miningFee   uint64
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			sweepCompletedBatchIDType, &batchID,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedReclaimedType, &reclaimed,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedCountType, &count,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedFeeRateType, &feeRate,
		),
		tlv.MakePrimitiveRecord(
			sweepCompletedMiningFeeType, &miningFee,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode SweepCompletedMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"SweepCompletedMsg.BatchID", batchID, len(m.BatchID),
	); err != nil {
		return err
	}

	reclaim, err := decodeAmountSat(
		"SweepCompletedMsg.ReclaimedAmountSat", reclaimed,
	)
	if err != nil {
		return err
	}

	c, err := decodeCount("SweepCompletedMsg.Count", count)
	if err != nil {
		return err
	}

	rate, err := decodeAmountSat(
		"SweepCompletedMsg.FeeRateSatVB", feeRate,
	)
	if err != nil {
		return err
	}

	mining, err := decodeAmountSat(
		"SweepCompletedMsg.MiningFeeSat", miningFee,
	)
	if err != nil {
		return err
	}

	copy(m.BatchID[:], batchID)
	m.ReclaimedAmountSat = reclaim
	m.Count = c
	m.BlockHeight = blockHeight
	m.FeeRateSatVB = rate
	m.MiningFeeSat = mining

	return nil
}

// OORFinalizedMsg is sent when an out-of-round transfer
// completes. OOR transfers are free but tracked for volume
// auditing.
type OORFinalizedMsg struct {
	actor.BaseMessage

	// SessionID is the OOR session identifier.
	SessionID [32]byte

	// InputAmountSat is the total value of consumed VTXOs.
	InputAmountSat int64

	// OutputAmountSat is the total value of created VTXOs.
	OutputAmountSat int64
}

// MessageType returns the message type name for routing.
func (m *OORFinalizedMsg) MessageType() string {
	return "OORFinalizedMsg"
}

// TLVType returns the TLV type tag used by the durable mailbox
// codec to route this message.
func (m *OORFinalizedMsg) TLVType() tlv.Type {
	return oorFinalizedTLVType
}

// Encode serializes the message as a TLV stream.
func (m *OORFinalizedMsg) Encode(w io.Writer) error {
	sessionID := m.SessionID[:]
	inputAmt := uint64(m.InputAmountSat)
	outputAmt := uint64(m.OutputAmountSat)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			oorFinalizedSessionIDType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			oorFinalizedInputAmtType, &inputAmt,
		),
		tlv.MakePrimitiveRecord(
			oorFinalizedOutputAmtType, &outputAmt,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *OORFinalizedMsg) Decode(r io.Reader) error {
	var (
		sessionID []byte
		inputAmt  uint64
		outputAmt uint64
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			oorFinalizedSessionIDType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			oorFinalizedInputAmtType, &inputAmt,
		),
		tlv.MakePrimitiveRecord(
			oorFinalizedOutputAmtType, &outputAmt,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode OORFinalizedMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"OORFinalizedMsg.SessionID",
		sessionID, len(m.SessionID),
	); err != nil {
		return err
	}

	input, err := decodeAmountSat(
		"OORFinalizedMsg.InputAmountSat", inputAmt,
	)
	if err != nil {
		return err
	}

	output, err := decodeAmountSat(
		"OORFinalizedMsg.OutputAmountSat", outputAmt,
	)
	if err != nil {
		return err
	}

	copy(m.SessionID[:], sessionID)
	m.InputAmountSat = input
	m.OutputAmountSat = output

	return nil
}

// BlockEpochMsg is sent on each new block. The ledger actor
// diffs the wallet UTXO set and updates the treasury balance.
type BlockEpochMsg struct {
	actor.BaseMessage

	// BlockHeight is the new block height.
	BlockHeight uint32

	// BlockHash is the 32-byte block hash.
	BlockHash [32]byte
}

// MessageType returns the message type name for routing.
func (m *BlockEpochMsg) MessageType() string {
	return "BlockEpochMsg"
}

// TLVType returns the TLV type tag used by the durable mailbox
// codec to route this message.
func (m *BlockEpochMsg) TLVType() tlv.Type {
	return blockEpochTLVType
}

// Encode serializes the message as a TLV stream.
func (m *BlockEpochMsg) Encode(w io.Writer) error {
	blockHeight := m.BlockHeight
	blockHash := m.BlockHash[:]

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			blockEpochHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			blockEpochHashType, &blockHash,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *BlockEpochMsg) Decode(r io.Reader) error {
	var (
		blockHeight uint32
		blockHash   []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			blockEpochHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			blockEpochHashType, &blockHash,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode BlockEpochMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"BlockEpochMsg.BlockHash", blockHash, len(m.BlockHash),
	); err != nil {
		return err
	}

	m.BlockHeight = blockHeight
	copy(m.BlockHash[:], blockHash)

	return nil
}

// newLedgerCodec builds the durable mailbox codec for the ledger
// actor. Each message type is registered individually, allowing
// the durable actor to serialize and dispatch messages without
// an intermediate envelope layer.
func newLedgerCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(
		roundConfirmedTLVType,
		func() actor.TLVMessage {
			return &RoundConfirmedMsg{}
		},
	)
	codec.MustRegister(
		vtxosForfeitedTLVType,
		func() actor.TLVMessage {
			return &VTXOsForfeitedMsg{}
		},
	)
	codec.MustRegister(
		sweepCompletedTLVType,
		func() actor.TLVMessage {
			return &SweepCompletedMsg{}
		},
	)
	codec.MustRegister(
		oorFinalizedTLVType,
		func() actor.TLVMessage {
			return &OORFinalizedMsg{}
		},
	)
	codec.MustRegister(
		blockEpochTLVType,
		func() actor.TLVMessage {
			return &BlockEpochMsg{}
		},
	)
	codec.MustRegister(
		actor.RestartTLVType,
		func() actor.TLVMessage {
			return &actor.RestartMessage{}
		},
	)

	return codec
}

// Compile-time interface checks.
var (
	_ LedgerMsg = (*RoundConfirmedMsg)(nil)
	_ LedgerMsg = (*VTXOsForfeitedMsg)(nil)
	_ LedgerMsg = (*SweepCompletedMsg)(nil)
	_ LedgerMsg = (*OORFinalizedMsg)(nil)
	_ LedgerMsg = (*BlockEpochMsg)(nil)
)
