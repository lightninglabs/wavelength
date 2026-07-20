package ledger

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// outpointRecord wraps a wire.OutPoint so it can be encoded /
// decoded as a fixed 36-byte TLV payload (32-byte hash followed
// by 4-byte little-endian index). Mirrors the pattern used in
// db/tree_codec.go so callers can thread wire.OutPoint directly
// through TLV messages instead of splitting the two halves into
// separate primitive records.
type outpointRecord struct {
	wire.OutPoint
}

// outpointEncoder serializes an outpoint as 32 hash bytes plus
// 4 little-endian index bytes.
func outpointEncoder(w io.Writer, val interface{}, _ *[8]byte) error {
	o, ok := val.(*outpointRecord)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "outpointRecord")
	}

	if _, err := w.Write(o.Hash[:]); err != nil {
		return err
	}

	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], o.Index)

	_, err := w.Write(buf[:])

	return err
}

// outpointDecoder reverses outpointEncoder. Rejects any payload
// that is not exactly 36 bytes so a corrupt TLV stream surfaces
// at the decode boundary rather than producing a truncated or
// zero-padded outpoint.
func outpointDecoder(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if l != 36 {
		return fmt.Errorf("%w: outpoint TLV payload must be 36 "+
			"bytes, got %d", ErrInvalidMessage, l)
	}

	o, ok := val.(*outpointRecord)
	if !ok {
		return tlv.NewTypeForDecodingErr(
			val, "outpointRecord", l, 36,
		)
	}

	if _, err := io.ReadFull(r, o.Hash[:]); err != nil {
		return err
	}

	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return err
	}

	o.Index = binary.LittleEndian.Uint32(buf[:])

	return nil
}

// makeOutpointRecord builds a single-field TLV record of type
// fieldType backed by outpointEncoder / outpointDecoder. Used by
// the ledger messages that carry an outpoint (VTXOSentMsg etc.)
// so the Go field type stays wire.OutPoint instead of split
// hash / index primitives.
func makeOutpointRecord(fieldType tlv.Type, rec *outpointRecord) tlv.Record {
	return tlv.MakeStaticRecord(
		fieldType, rec, 36, outpointEncoder, outpointDecoder,
	)
}

// decodeAmountSat narrows a TLV-decoded uint64 satoshi field to
// the int64 domain used everywhere downstream. A TLV stream is
// application-sourced and can carry any 64-bit value, so a
// malformed payload with the high bit set would silently
// underflow through int64(amountSat) and produce a negative
// value that later code (SQL CHECK, positivity guards in
// handlers) rejects with a cryptic error. Wrapping the check
// here gives every Decode method a single rejection point that
// maps to ErrInvalidMessage.
func decodeAmountSat(field string, amountSat uint64) (int64, error) {
	if amountSat > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %s %d exceeds int64 range",
			ErrInvalidMessage, field, amountSat)
	}

	return int64(amountSat), nil
}

// decodeFixedBytes validates that a TLV-decoded variable-length
// byte field carries exactly the expected number of bytes. The
// TLV wire format is length-prefixed so a caller can serialize
// a zero-, too-short-, or too-long blob for a field the
// receiver expects at a fixed width (e.g. 16-byte RoundID or
// 32-byte SessionID/OutpointHash). Reject those explicitly so a
// malformed payload surfaces as ErrInvalidMessage at the Decode
// boundary instead of silently producing truncated/padded state
// that corrupts downstream processing.
//
// A zero-length slice is treated as "field absent" so the
// caller-level absent semantics (zero-valued arrays) still
// survive a round-trip over TLV without triggering rejection.
func decodeFixedBytes(field string, got []byte, want int) error {
	if len(got) == 0 {
		return nil
	}

	if len(got) != want {
		return fmt.Errorf("%w: %s has %d bytes, expected %d",
			ErrInvalidMessage, field, len(got), want)
	}

	return nil
}

// TLV type constants for client-side ledger actor messages.
// These use the 0x9xxx range to avoid collisions with the
// server-side ledger actor (0x8xxx) and other actor subsystems.
const (
	feePaidTLVType                tlv.Type = 0x9001
	vtxoReceivedTLVType           tlv.Type = 0x9002
	vtxoSentTLVType               tlv.Type = 0x9003
	exitCostTLVType               tlv.Type = 0x9004
	utxoCreatedTLVType            tlv.Type = 0x9005
	utxoSpentTLVType              tlv.Type = 0x9006
	boardingSweepConfirmedTLVType tlv.Type = 0x9007
	vtxoClaimReissuedTLVType      tlv.Type = 0x9008
)

// Per-message TLV record types. Each message defines its own
// field types using odd values for forward-compatible
// extensibility.
const (
	// FeePaidMsg field types.
	feePaidRoundIDType        tlv.Type = 1
	feePaidAmountSatType      tlv.Type = 3
	feePaidFeeTypeType        tlv.Type = 5
	feePaidBlockHeightType    tlv.Type = 7
	feePaidIdempotencyKeyType tlv.Type = 9

	// VTXOReceivedMsg field types.
	vtxoRecvOutpointHashType  tlv.Type = 1
	vtxoRecvOutpointIndexType tlv.Type = 3
	vtxoRecvAmountSatType     tlv.Type = 5
	vtxoRecvSourceType        tlv.Type = 7
	vtxoRecvRoundIDType       tlv.Type = 9

	// VTXOSentMsg field types. The codec accepts either
	// session_id (OOR sends) or round_id (in-round sends), not
	// both; handleVTXOSent enforces the mutual exclusion. The
	// optional Outpoint field disambiguates per-VTXO entries
	// within a single round so two in-round refreshes don't
	// collide on idx_client_ledger_idempotent_round (same
	// round_id + event_type + accounts otherwise).
	vtxoSentSessionIDType   tlv.Type = 1
	vtxoSentAmountSatType   tlv.Type = 3
	vtxoSentRoundIDType     tlv.Type = 5
	vtxoSentOutpointType    tlv.Type = 7
	vtxoSentIdempotencyType tlv.Type = 9

	// VTXOClaimReissuedMsg field types.
	claimReissueSourceType      tlv.Type = 1
	claimReissueReplacementType tlv.Type = 3
	claimReissueAmountSatType   tlv.Type = 5
	claimReissueRoundIDType     tlv.Type = 7

	// ExitCostMsg field types.
	exitCostOutpointHashType  tlv.Type = 1
	exitCostOutpointIndexType tlv.Type = 3
	exitCostAmountSatType     tlv.Type = 5
	exitCostCostSatType       tlv.Type = 7
	exitCostBlockHeightType   tlv.Type = 9

	// UTXOCreatedMsg / UTXOSpentMsg field types.
	utxoOutpointHashType   tlv.Type = 1
	utxoOutpointIndexType  tlv.Type = 3
	utxoAmountSatType      tlv.Type = 5
	utxoBlockHeightType    tlv.Type = 7
	utxoClassificationType tlv.Type = 9

	// BoardingSweepConfirmedMsg field types.
	sweepConfirmedTxidType        tlv.Type = 1
	sweepConfirmedBlockHeightType tlv.Type = 3
	sweepConfirmedChainCostType   tlv.Type = 5
	sweepConfirmedInputsType      tlv.Type = 7
	sweepConfirmedDestSatType     tlv.Type = 9
	sweepConfirmedDestExtType     tlv.Type = 11
)

// sweepInputRecordSize is the fixed wire width of one serialized sweep
// input inside a BoardingSweepConfirmedMsg: a 32-byte outpoint hash, a
// 4-byte big-endian output index, and an 8-byte big-endian satoshi amount.
const sweepInputRecordSize = 32 + 4 + 8

// LedgerMsg is the message constraint for the client-side ledger
// durable actor mailbox. It embeds actor.TLVMessage so both
// application messages and the framework RestartMessage satisfy
// this interface.
type LedgerMsg interface {
	actor.TLVMessage
}

// LedgerResp is the response type for the ledger actor. The
// ledger actor is fire-and-forget, so responses are always nil.
type LedgerResp interface {
	actor.Message

	ledgerRespSealed()
}

// FeePaidMsg is sent when the client pays a fee. Two flavors:
//
//   - FeeTypeBoarding / FeeTypeRefresh: an Ark protocol fee paid
//     to the operator during a round. Booked as
//     fees_paid += AmountSat / vtxo_balance -= AmountSat. Keyed
//     by RoundID via the (round_id, event_type) partial unique
//     index.
//   - FeeTypeOnchainSweep: L1 chain cost paid by a wallet-
//     internal boarding sweep. Booked as
//     onchain_fees += AmountSat / wallet_clearing -= AmountSat.
//     Has no paired VTXOReceivedMsg; keyed by the sweep txid
//     carried in IdempotencyKey via the
//     idx_client_ledger_idempotent_key partial unique index.
//     RoundID is left zero and stored as NULL.
//
// Caller contract (FeeTypeBoarding / FeeTypeRefresh only):
// FeePaidMsg accumulates the fee on top of a paired
// VTXOReceivedMsg. The VTXOReceivedMsg for the same round MUST
// carry the GROSS (pre-fee) amount -- the FeePaidMsg then nets
// vtxo_balance down to the delivered post-fee value. Sending a
// net VTXOReceivedMsg together with a FeePaidMsg will
// under-count vtxo_balance by the fee. OOR sends and receives
// are already net-of-fee and do not need a separate FeePaidMsg.
// FeeTypeOnchainSweep is a standalone entry and never pairs
// with a VTXOReceivedMsg.
type FeePaidMsg struct {
	actor.BaseMessage

	// RoundID is the 16-byte round UUID that links this fee
	// to a specific boarding or refresh round.
	RoundID [16]byte

	// AmountSat is the fee amount in satoshis. Must be
	// positive. Callers should set the paired VTXOReceivedMsg
	// AmountSat to the gross pre-fee value for the same round
	// so the two entries combine to the correct net balance.
	AmountSat int64

	// FeeType classifies the fee. Must be one of the
	// FeeType* constants (FeeTypeBoarding, FeeTypeRefresh,
	// FeeTypeOnchainSweep); any other value is rejected.
	FeeType string

	// BlockHeight is the confirmation block height.
	BlockHeight uint32

	// IdempotencyKey is an optional natural dedup key used by
	// fee events that do not carry a RoundID — the boarding
	// sweep flow for example uses the sweep txid (32 bytes).
	// Round/refresh fees leave this empty and rely on the
	// (round_id, event_type) partial unique index instead.
	IdempotencyKey []byte
}

// MessageType returns the message type name for routing.
func (m *FeePaidMsg) MessageType() string {
	return "FeePaidMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *FeePaidMsg) TLVType() tlv.Type {
	return feePaidTLVType
}

// Encode serializes the message as a TLV stream.
func (m *FeePaidMsg) Encode(w io.Writer) error {
	roundID := m.RoundID[:]
	amountSat := uint64(m.AmountSat)
	feeType := []byte(m.FeeType)
	blockHeight := m.BlockHeight
	idempotency := m.IdempotencyKey

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			feePaidRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			feePaidAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			feePaidFeeTypeType, &feeType,
		),
		tlv.MakePrimitiveRecord(
			feePaidBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			feePaidIdempotencyKeyType, &idempotency,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *FeePaidMsg) Decode(r io.Reader) error {
	var (
		roundID     []byte
		amountSat   uint64
		feeType     []byte
		blockHeight uint32
		idempotency []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			feePaidRoundIDType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			feePaidAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			feePaidFeeTypeType, &feeType,
		),
		tlv.MakePrimitiveRecord(
			feePaidBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			feePaidIdempotencyKeyType, &idempotency,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode FeePaidMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"FeePaidMsg.RoundID", roundID, len(m.RoundID),
	); err != nil {
		return err
	}

	amt, err := decodeAmountSat("FeePaidMsg.AmountSat", amountSat)
	if err != nil {
		return err
	}

	copy(m.RoundID[:], roundID)
	m.AmountSat = amt
	m.FeeType = string(feeType)
	m.BlockHeight = blockHeight
	// Preserve nil-vs-empty distinction so TLV round-trip equality
	// matches the producer's intent. tlv.MakePrimitiveRecord on a
	// []byte field decodes a missing record as []byte{}; tests and
	// callers that compare IdempotencyKey == nil expect nil.
	if len(idempotency) == 0 {
		m.IdempotencyKey = nil
	} else {
		m.IdempotencyKey = idempotency
	}

	return nil
}

// VTXOReceivedMsg is sent when the client receives a VTXO from
// one of three sources (see Source docstring). The ledger actor
// records the movement from the appropriate counterparty
// account into vtxo_balance.
//
// Caller contract: for Source == SourceRoundBoarding, AmountSat
// MUST be the gross (pre-fee) VTXO amount paired with a
// FeePaidMsg for the same RoundID that debits fees_paid and
// nets vtxo_balance down. For SourceRoundTransfer and
// SourceOOR, AmountSat is the net received amount and no
// FeePaidMsg is expected.
type VTXOReceivedMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash of the
	// outpoint containing the received VTXO.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the VTXO value in satoshis. See the type
	// docstring for the gross-vs-net caller contract.
	AmountSat int64

	// Source classifies how the VTXO was received
	// (e.g. "round", "oor").
	Source string

	// RoundID is the 16-byte round UUID associated with this
	// VTXO.
	RoundID [16]byte
}

// MessageType returns the message type name for routing.
func (m *VTXOReceivedMsg) MessageType() string {
	return "VTXOReceivedMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *VTXOReceivedMsg) TLVType() tlv.Type {
	return vtxoReceivedTLVType
}

// Encode serializes the message as a TLV stream.
func (m *VTXOReceivedMsg) Encode(w io.Writer) error {
	outpointHash := m.OutpointHash[:]
	outpointIndex := m.OutpointIndex
	amountSat := uint64(m.AmountSat)
	source := []byte(m.Source)
	roundID := m.RoundID[:]

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxoRecvOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvSourceType, &source,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvRoundIDType, &roundID,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *VTXOReceivedMsg) Decode(r io.Reader) error {
	var (
		outpointHash  []byte
		outpointIndex uint32
		amountSat     uint64
		source        []byte
		roundID       []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxoRecvOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvSourceType, &source,
		),
		tlv.MakePrimitiveRecord(
			vtxoRecvRoundIDType, &roundID,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode VTXOReceivedMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"VTXOReceivedMsg.OutpointHash", outpointHash,
		len(m.OutpointHash),
	); err != nil {
		return err
	}

	if err := decodeFixedBytes(
		"VTXOReceivedMsg.RoundID", roundID, len(m.RoundID),
	); err != nil {
		return err
	}

	amt, err := decodeAmountSat(
		"VTXOReceivedMsg.AmountSat", amountSat,
	)
	if err != nil {
		return err
	}

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = amt
	m.Source = string(source)
	copy(m.RoundID[:], roundID)

	return nil
}

// VTXOSentMsg is sent when the client sends a VTXO to another
// participant, either out-of-round (SessionID) or inside a round
// (RoundID). Exactly one of the two identifiers must be
// non-zero; handleVTXOSent rejects messages that carry both or
// neither.
type VTXOSentMsg struct {
	actor.BaseMessage

	// SessionID is the 32-byte OOR session identifier. Zero
	// when this is an in-round send.
	SessionID [32]byte

	// RoundID is the 16-byte round UUID. Zero when this is an
	// out-of-round send.
	RoundID [16]byte

	// Outpoint identifies the specific VTXO being sent.
	// Optional -- the round-scoped idempotency index treats two
	// sends in the same round without an outpoint as duplicates,
	// so in-round refresh emissions and directed-send forfeits
	// must set this so handleVTXOSent can stamp an outpoint-
	// derived IdempotencyKey on the ledger entry. OOR single-
	// destination sends can leave the outpoint zero-valued and
	// fall back on the session_id partial index.
	Outpoint wire.OutPoint

	// AmountSat is the total value sent in satoshis.
	AmountSat int64

	// IdempotencyKey is an optional caller-supplied key for
	// round-scoped sends that do not have a VTXO outpoint, such
	// as cooperative leave outputs and foreign directed-send
	// recipient outputs. When set, it takes precedence over
	// Outpoint for ledger-entry deduplication.
	IdempotencyKey []byte
}

// MessageType returns the message type name for routing.
func (m *VTXOSentMsg) MessageType() string {
	return "VTXOSentMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *VTXOSentMsg) TLVType() tlv.Type {
	return vtxoSentTLVType
}

// Encode serializes the message as a TLV stream.
func (m *VTXOSentMsg) Encode(w io.Writer) error {
	sessionID := m.SessionID[:]
	amountSat := uint64(m.AmountSat)
	roundID := m.RoundID[:]
	outpoint := &outpointRecord{OutPoint: m.Outpoint}
	idempotencyKey := m.IdempotencyKey

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxoSentSessionIDType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			vtxoSentAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			vtxoSentRoundIDType, &roundID,
		),
		makeOutpointRecord(vtxoSentOutpointType, outpoint),
		tlv.MakePrimitiveRecord(
			vtxoSentIdempotencyType, &idempotencyKey,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *VTXOSentMsg) Decode(r io.Reader) error {
	var (
		sessionID      []byte
		amountSat      uint64
		roundID        []byte
		outpoint       outpointRecord
		idempotencyKey []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			vtxoSentSessionIDType, &sessionID,
		),
		tlv.MakePrimitiveRecord(
			vtxoSentAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			vtxoSentRoundIDType, &roundID,
		),
		makeOutpointRecord(vtxoSentOutpointType, &outpoint),
		tlv.MakePrimitiveRecord(
			vtxoSentIdempotencyType, &idempotencyKey,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode VTXOSentMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"VTXOSentMsg.SessionID", sessionID, len(m.SessionID),
	); err != nil {
		return err
	}

	if err := decodeFixedBytes(
		"VTXOSentMsg.RoundID", roundID, len(m.RoundID),
	); err != nil {
		return err
	}

	amt, err := decodeAmountSat("VTXOSentMsg.AmountSat", amountSat)
	if err != nil {
		return err
	}

	copy(m.SessionID[:], sessionID)
	copy(m.RoundID[:], roundID)
	m.AmountSat = amt
	m.Outpoint = outpoint.OutPoint
	m.IdempotencyKey = append(m.IdempotencyKey[:0], idempotencyKey...)

	return nil
}

// VTXOClaimReissuedMsg records one zero-fee claim reissue as an atomic pair:
// the expired source leaves the VTXO balance and its exact-value replacement
// enters it. A dedicated message prevents generic round-refresh accounting
// from attributing the send leg to the replacement outpoint.
type VTXOClaimReissuedMsg struct {
	actor.BaseMessage

	// Source is the expired VTXO consumed by the operator sweep claim.
	Source wire.OutPoint

	// Replacement is the exact-value VTXO created by the reissue round.
	Replacement wire.OutPoint

	// AmountSat is both the source and replacement amount. Claim reissues
	// are fee-free, so the handler rejects non-positive values and emits no
	// fee leg.
	AmountSat int64

	// RoundID is the 16-byte UUID of the round that created Replacement.
	RoundID [16]byte
}

// MessageType returns the message type name for routing.
func (m *VTXOClaimReissuedMsg) MessageType() string {
	return "VTXOClaimReissuedMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *VTXOClaimReissuedMsg) TLVType() tlv.Type {
	return vtxoClaimReissuedTLVType
}

// Encode serializes the message as an additive TLV stream.
func (m *VTXOClaimReissuedMsg) Encode(w io.Writer) error {
	source := &outpointRecord{OutPoint: m.Source}
	replacement := &outpointRecord{OutPoint: m.Replacement}
	amountSat := uint64(m.AmountSat)
	roundID := m.RoundID[:]

	stream, err := tlv.NewStream(
		makeOutpointRecord(claimReissueSourceType, source),
		makeOutpointRecord(claimReissueReplacementType, replacement),
		tlv.MakePrimitiveRecord(
			claimReissueAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			claimReissueRoundIDType, &roundID,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the claim-reissue TLV stream.
func (m *VTXOClaimReissuedMsg) Decode(r io.Reader) error {
	var (
		source      outpointRecord
		replacement outpointRecord
		amountSat   uint64
		roundID     []byte
	)

	stream, err := tlv.NewStream(
		makeOutpointRecord(claimReissueSourceType, &source),
		makeOutpointRecord(claimReissueReplacementType, &replacement),
		tlv.MakePrimitiveRecord(
			claimReissueAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			claimReissueRoundIDType, &roundID,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode VTXOClaimReissuedMsg: %w", err)
	}
	if err := decodeFixedBytes(
		"VTXOClaimReissuedMsg.RoundID", roundID, len(m.RoundID),
	); err != nil {
		return err
	}

	amount, err := decodeAmountSat(
		"VTXOClaimReissuedMsg.AmountSat", amountSat,
	)
	if err != nil {
		return err
	}

	m.Source = source.OutPoint
	m.Replacement = replacement.OutPoint
	m.AmountSat = amount
	copy(m.RoundID[:], roundID)

	return nil
}

// ExitCostMsg is sent when the client pays an on-chain exit
// cost (e.g. unilateral exit). The ledger actor records the
// on-chain fee expense.
type ExitCostMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash of the
	// exited outpoint.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the VTXO value that was exited.
	AmountSat int64

	// ExitCostSat is the on-chain fee cost of the exit.
	ExitCostSat int64

	// BlockHeight is the block height at which the exit was
	// confirmed.
	BlockHeight uint32
}

// MessageType returns the message type name for routing.
func (m *ExitCostMsg) MessageType() string {
	return "ExitCostMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *ExitCostMsg) TLVType() tlv.Type {
	return exitCostTLVType
}

// Encode serializes the message as a TLV stream.
func (m *ExitCostMsg) Encode(w io.Writer) error {
	outpointHash := m.OutpointHash[:]
	outpointIndex := m.OutpointIndex
	amountSat := uint64(m.AmountSat)
	exitCostSat := uint64(m.ExitCostSat)
	blockHeight := m.BlockHeight

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			exitCostOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			exitCostOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			exitCostAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			exitCostCostSatType, &exitCostSat,
		),
		tlv.MakePrimitiveRecord(
			exitCostBlockHeightType, &blockHeight,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *ExitCostMsg) Decode(r io.Reader) error {
	var (
		outpointHash  []byte
		outpointIndex uint32
		amountSat     uint64
		exitCostSat   uint64
		blockHeight   uint32
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			exitCostOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			exitCostOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			exitCostAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			exitCostCostSatType, &exitCostSat,
		),
		tlv.MakePrimitiveRecord(
			exitCostBlockHeightType, &blockHeight,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode ExitCostMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"ExitCostMsg.OutpointHash", outpointHash, len(m.OutpointHash),
	); err != nil {
		return err
	}

	amt, err := decodeAmountSat("ExitCostMsg.AmountSat", amountSat)
	if err != nil {
		return err
	}

	cost, err := decodeAmountSat(
		"ExitCostMsg.ExitCostSat", exitCostSat,
	)
	if err != nil {
		return err
	}

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = amt
	m.ExitCostSat = cost
	m.BlockHeight = blockHeight

	return nil
}

// UTXOCreatedMsg is sent when a new wallet UTXO is confirmed
// on-chain. The ledger actor writes an audit log entry
// classified by the UTXO's origin (deposit, change, etc.).
type UTXOCreatedMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the UTXO value in satoshis.
	AmountSat int64

	// BlockHeight is the confirmation block height.
	BlockHeight uint32

	// Classification categorizes the UTXO origin (e.g.
	// "deposit", "change", "sweep_return").
	Classification string
}

// MessageType returns the message type name for routing.
func (m *UTXOCreatedMsg) MessageType() string {
	return "UTXOCreatedMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *UTXOCreatedMsg) TLVType() tlv.Type {
	return utxoCreatedTLVType
}

// Encode serializes the message as a TLV stream.
func (m *UTXOCreatedMsg) Encode(w io.Writer) error {
	outpointHash := m.OutpointHash[:]
	outpointIndex := m.OutpointIndex
	amountSat := uint64(m.AmountSat)
	blockHeight := m.BlockHeight
	classification := []byte(m.Classification)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			utxoOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			utxoOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			utxoAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			utxoBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			utxoClassificationType, &classification,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *UTXOCreatedMsg) Decode(r io.Reader) error {
	var (
		outpointHash   []byte
		outpointIndex  uint32
		amountSat      uint64
		blockHeight    uint32
		classification []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			utxoOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			utxoOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			utxoAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			utxoBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			utxoClassificationType, &classification,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode UTXOCreatedMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"UTXOCreatedMsg.OutpointHash", outpointHash,
		len(m.OutpointHash),
	); err != nil {
		return err
	}

	amt, err := decodeAmountSat(
		"UTXOCreatedMsg.AmountSat", amountSat,
	)
	if err != nil {
		return err
	}

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = amt
	m.BlockHeight = blockHeight
	m.Classification = string(classification)

	return nil
}

// UTXOSpentMsg is sent when a wallet UTXO is spent on-chain.
// The ledger actor writes an audit log entry classified by the
// spend's purpose (round_funding, sweep_return, etc.).
type UTXOSpentMsg struct {
	actor.BaseMessage

	// OutpointHash is the 32-byte transaction hash of the
	// spent outpoint.
	OutpointHash [32]byte

	// OutpointIndex is the output index within the
	// transaction.
	OutpointIndex uint32

	// AmountSat is the UTXO value in satoshis.
	AmountSat int64

	// BlockHeight is the block height at which the spend was
	// confirmed.
	BlockHeight uint32

	// Classification categorizes the spend purpose (e.g.
	// "round_funding", "unknown").
	Classification string
}

// MessageType returns the message type name for routing.
func (m *UTXOSpentMsg) MessageType() string {
	return "UTXOSpentMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *UTXOSpentMsg) TLVType() tlv.Type {
	return utxoSpentTLVType
}

// Encode serializes the message as a TLV stream.
func (m *UTXOSpentMsg) Encode(w io.Writer) error {
	outpointHash := m.OutpointHash[:]
	outpointIndex := m.OutpointIndex
	amountSat := uint64(m.AmountSat)
	blockHeight := m.BlockHeight
	classification := []byte(m.Classification)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			utxoOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			utxoOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			utxoAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			utxoBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			utxoClassificationType, &classification,
		),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *UTXOSpentMsg) Decode(r io.Reader) error {
	var (
		outpointHash   []byte
		outpointIndex  uint32
		amountSat      uint64
		blockHeight    uint32
		classification []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			utxoOutpointHashType, &outpointHash,
		),
		tlv.MakePrimitiveRecord(
			utxoOutpointIndexType, &outpointIndex,
		),
		tlv.MakePrimitiveRecord(
			utxoAmountSatType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			utxoBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			utxoClassificationType, &classification,
		),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode UTXOSpentMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"UTXOSpentMsg.OutpointHash", outpointHash, len(m.OutpointHash),
	); err != nil {
		return err
	}

	amt, err := decodeAmountSat(
		"UTXOSpentMsg.AmountSat", amountSat,
	)
	if err != nil {
		return err
	}

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = amt
	m.BlockHeight = blockHeight
	m.Classification = string(classification)

	return nil
}

// SweepInput is one boarding UTXO spent by a confirmed sweep, carried
// inside a BoardingSweepConfirmedMsg. The amount is the value of the
// boarding outpoint as the wallet persisted it, used both for the audit
// row and for the wallet_clearing debit leg.
type SweepInput struct {
	// Outpoint is the spent boarding UTXO.
	Outpoint wire.OutPoint

	// AmountSat is the boarding UTXO value in satoshis. Must be
	// positive.
	AmountSat int64
}

// BoardingSweepConfirmedMsg records a confirmed boarding sweep as a single
// atomic accounting event. Earlier revisions emitted the fee, per-input,
// and destination legs as independent Tells; a partial failure could leave
// the wallet_clearing account stranded non-zero. Folding every leg into one
// message lets the ledger handler book them inside a single Commit, so the
// clearing account either nets to zero or nothing is written.
//
// The legs the handler derives, all keyed for idempotent replay:
//
//   - Fee: debit onchain_fees, credit wallet_clearing by ChainCostSat
//     (miner fee + P2A anchor), keyed by the sweep txid.
//   - Per input: a wallet_utxo_log "spent" audit row plus debit
//     wallet_clearing, credit wallet_balance, keyed by the input outpoint.
//   - Destination: when DestinationExternal, debit transfers_out, credit
//     wallet_clearing (the funds left the wallet); otherwise a "created"
//     audit row for the wallet-return output plus debit wallet_balance,
//     credit wallet_clearing, keyed by the sweep txid's vout 0.
//
// The clearing identity is Σ(inputs) − ChainCostSat − DestinationSat = 0.
type BoardingSweepConfirmedMsg struct {
	actor.BaseMessage

	// Txid identifies the sweep transaction.
	Txid [32]byte

	// BlockHeight is the confirmation block height.
	BlockHeight uint32

	// ChainCostSat is the on-chain cost (miner fee + P2A anchor) that
	// leaves the wallet, debited to onchain_fees. Must be positive.
	ChainCostSat int64

	// Inputs are the boarding UTXOs the sweep spent. Must be non-empty
	// and each amount positive.
	Inputs []SweepInput

	// DestinationSat is the sweep destination output value, credited out
	// of wallet_clearing. Must be positive.
	DestinationSat int64

	// DestinationExternal reports whether the destination is a
	// caller-supplied external address (true) rather than a
	// wallet-derived return output (false).
	DestinationExternal bool
}

// MessageType returns the message type name for routing.
func (m *BoardingSweepConfirmedMsg) MessageType() string {
	return "BoardingSweepConfirmedMsg"
}

// TLVType returns the TLV type tag for codec registration.
func (m *BoardingSweepConfirmedMsg) TLVType() tlv.Type {
	return boardingSweepConfirmedTLVType
}

// encodeSweepInputs flattens the input list into a single byte blob of
// fixed-width records (32-byte hash, 4-byte big-endian index, 8-byte
// big-endian amount). A flat blob keeps the TLV layout a single primitive
// record while preserving per-input granularity.
func encodeSweepInputs(inputs []SweepInput) []byte {
	out := make([]byte, 0, len(inputs)*sweepInputRecordSize)
	var rec [sweepInputRecordSize]byte
	for _, in := range inputs {
		copy(rec[:32], in.Outpoint.Hash[:])
		binary.BigEndian.PutUint32(rec[32:36], in.Outpoint.Index)
		binary.BigEndian.PutUint64(rec[36:44], uint64(in.AmountSat))
		out = append(out, rec[:]...)
	}

	return out
}

// decodeSweepInputs reverses encodeSweepInputs, rejecting a blob whose
// length is not a whole multiple of the fixed record size or that carries a
// non-positive / out-of-range amount.
func decodeSweepInputs(blob []byte) ([]SweepInput, error) {
	if len(blob)%sweepInputRecordSize != 0 {
		return nil, fmt.Errorf("%w: sweep inputs blob has %d bytes, "+
			"not a multiple of %d", ErrInvalidMessage, len(blob),
			sweepInputRecordSize)
	}

	count := len(blob) / sweepInputRecordSize
	inputs := make([]SweepInput, 0, count)
	for i := 0; i < count; i++ {
		rec := blob[i*sweepInputRecordSize : (i+1)*sweepInputRecordSize]

		var in SweepInput
		copy(in.Outpoint.Hash[:], rec[:32])
		in.Outpoint.Index = binary.BigEndian.Uint32(rec[32:36])

		amt, err := decodeAmountSat(
			"BoardingSweepConfirmedMsg.Inputs.AmountSat",
			binary.BigEndian.Uint64(rec[36:44]),
		)
		if err != nil {
			return nil, err
		}
		in.AmountSat = amt

		inputs = append(inputs, in)
	}

	return inputs, nil
}

// Encode serializes the message as a TLV stream.
func (m *BoardingSweepConfirmedMsg) Encode(w io.Writer) error {
	txid := m.Txid[:]
	blockHeight := m.BlockHeight
	chainCost := uint64(m.ChainCostSat)
	inputs := encodeSweepInputs(m.Inputs)
	destSat := uint64(m.DestinationSat)
	var destExt uint8
	if m.DestinationExternal {
		destExt = 1
	}

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(sweepConfirmedTxidType, &txid),
		tlv.MakePrimitiveRecord(
			sweepConfirmedBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			sweepConfirmedChainCostType, &chainCost,
		),
		tlv.MakePrimitiveRecord(sweepConfirmedInputsType, &inputs),
		tlv.MakePrimitiveRecord(sweepConfirmedDestSatType, &destSat),
		tlv.MakePrimitiveRecord(sweepConfirmedDestExtType, &destExt),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *BoardingSweepConfirmedMsg) Decode(r io.Reader) error {
	var (
		txid        []byte
		blockHeight uint32
		chainCost   uint64
		inputsBlob  []byte
		destSat     uint64
		destExt     uint8
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(sweepConfirmedTxidType, &txid),
		tlv.MakePrimitiveRecord(
			sweepConfirmedBlockHeightType, &blockHeight,
		),
		tlv.MakePrimitiveRecord(
			sweepConfirmedChainCostType, &chainCost,
		),
		tlv.MakePrimitiveRecord(sweepConfirmedInputsType, &inputsBlob),
		tlv.MakePrimitiveRecord(sweepConfirmedDestSatType, &destSat),
		tlv.MakePrimitiveRecord(sweepConfirmedDestExtType, &destExt),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode BoardingSweepConfirmedMsg: %w", err)
	}

	if err := decodeFixedBytes(
		"BoardingSweepConfirmedMsg.Txid", txid, len(m.Txid),
	); err != nil {
		return err
	}

	chainCostSat, err := decodeAmountSat(
		"BoardingSweepConfirmedMsg.ChainCostSat", chainCost,
	)
	if err != nil {
		return err
	}

	destAmountSat, err := decodeAmountSat(
		"BoardingSweepConfirmedMsg.DestinationSat", destSat,
	)
	if err != nil {
		return err
	}

	inputs, err := decodeSweepInputs(inputsBlob)
	if err != nil {
		return err
	}

	copy(m.Txid[:], txid)
	m.BlockHeight = blockHeight
	m.ChainCostSat = chainCostSat
	m.Inputs = inputs
	m.DestinationSat = destAmountSat
	m.DestinationExternal = destExt != 0

	return nil
}

// newLedgerCodec builds the durable mailbox codec for the
// client-side ledger actor. Each message type is registered
// individually, allowing the durable actor to serialize and
// dispatch messages without an intermediate envelope layer.
func newLedgerCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(
		feePaidTLVType,
		func() actor.TLVMessage {
			return &FeePaidMsg{}
		},
	)
	codec.MustRegister(
		vtxoReceivedTLVType,
		func() actor.TLVMessage {
			return &VTXOReceivedMsg{}
		},
	)
	codec.MustRegister(
		vtxoSentTLVType,
		func() actor.TLVMessage {
			return &VTXOSentMsg{}
		},
	)
	codec.MustRegister(
		exitCostTLVType,
		func() actor.TLVMessage {
			return &ExitCostMsg{}
		},
	)
	codec.MustRegister(
		utxoCreatedTLVType,
		func() actor.TLVMessage {
			return &UTXOCreatedMsg{}
		},
	)
	codec.MustRegister(
		utxoSpentTLVType,
		func() actor.TLVMessage {
			return &UTXOSpentMsg{}
		},
	)
	codec.MustRegister(
		boardingSweepConfirmedTLVType,
		func() actor.TLVMessage {
			return &BoardingSweepConfirmedMsg{}
		},
	)
	codec.MustRegister(
		vtxoClaimReissuedTLVType,
		func() actor.TLVMessage {
			return &VTXOClaimReissuedMsg{}
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
	_ LedgerMsg = (*FeePaidMsg)(nil)
	_ LedgerMsg = (*VTXOReceivedMsg)(nil)
	_ LedgerMsg = (*VTXOSentMsg)(nil)
	_ LedgerMsg = (*VTXOClaimReissuedMsg)(nil)
	_ LedgerMsg = (*ExitCostMsg)(nil)
	_ LedgerMsg = (*UTXOCreatedMsg)(nil)
	_ LedgerMsg = (*UTXOSpentMsg)(nil)
	_ LedgerMsg = (*BoardingSweepConfirmedMsg)(nil)
)
