package ledger

import (
	"fmt"
	"io"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type constants for client-side ledger actor messages.
// These use the 0x9xxx range to avoid collisions with the
// server-side ledger actor (0x8xxx) and other actor subsystems.
const (
	feePaidTLVType      tlv.Type = 0x9001
	vtxoReceivedTLVType tlv.Type = 0x9002
	vtxoSentTLVType     tlv.Type = 0x9003
	exitCostTLVType     tlv.Type = 0x9004
	utxoCreatedTLVType  tlv.Type = 0x9005
	utxoSpentTLVType    tlv.Type = 0x9006
)

// Per-message TLV record types. Each message defines its own
// field types using odd values for forward-compatible
// extensibility.
const (
	// FeePaidMsg field types.
	feePaidRoundIDType     tlv.Type = 1
	feePaidAmountSatType   tlv.Type = 3
	feePaidFeeTypeType     tlv.Type = 5
	feePaidBlockHeightType tlv.Type = 7

	// VTXOReceivedMsg field types.
	vtxoRecvOutpointHashType  tlv.Type = 1
	vtxoRecvOutpointIndexType tlv.Type = 3
	vtxoRecvAmountSatType     tlv.Type = 5
	vtxoRecvSourceType        tlv.Type = 7
	vtxoRecvRoundIDType       tlv.Type = 9

	// VTXOSentMsg field types. The codec accepts either
	// session_id (OOR sends) or round_id (in-round sends), not
	// both; handleVTXOSent enforces the mutual exclusion.
	vtxoSentSessionIDType tlv.Type = 1
	vtxoSentAmountSatType tlv.Type = 3
	vtxoSentRoundIDType   tlv.Type = 5

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
)

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

// FeePaidMsg is sent when the client pays a fee during a round
// (boarding or refresh). The ledger actor records the expense
// as fees_paid += AmountSat / vtxo_balance -= AmountSat.
//
// Caller contract: FeePaidMsg accumulates the fee on top of a
// paired VTXOReceivedMsg. For a boarding or refresh round, the
// VTXOReceivedMsg for the same round MUST carry the GROSS
// (pre-fee) amount -- the FeePaidMsg then nets vtxo_balance down
// to the delivered post-fee value. Sending a net VTXOReceivedMsg
// together with a FeePaidMsg will under-count vtxo_balance by
// the fee. OOR sends and receives are already net-of-fee and do
// not need a separate FeePaidMsg.
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
	// FeeType* constants (FeeTypeBoarding, FeeTypeRefresh);
	// any other value is rejected.
	FeeType string

	// BlockHeight is the confirmation block height.
	BlockHeight uint32
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
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode FeePaidMsg: %w", err)
	}

	copy(m.RoundID[:], roundID)
	m.AmountSat = int64(amountSat)
	m.FeeType = string(feeType)
	m.BlockHeight = blockHeight

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
		return fmt.Errorf(
			"decode VTXOReceivedMsg: %w", err,
		)
	}

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = int64(amountSat)
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

	// AmountSat is the total value sent in satoshis.
	AmountSat int64
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
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the message.
func (m *VTXOSentMsg) Decode(r io.Reader) error {
	var (
		sessionID []byte
		amountSat uint64
		roundID   []byte
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
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return fmt.Errorf("decode VTXOSentMsg: %w", err)
	}

	copy(m.SessionID[:], sessionID)
	copy(m.RoundID[:], roundID)
	m.AmountSat = int64(amountSat)

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

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = int64(amountSat)
	m.ExitCostSat = int64(exitCostSat)
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

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = int64(amountSat)
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

	copy(m.OutpointHash[:], outpointHash)
	m.OutpointIndex = outpointIndex
	m.AmountSat = int64(amountSat)
	m.BlockHeight = blockHeight
	m.Classification = string(classification)

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
	_ LedgerMsg = (*ExitCostMsg)(nil)
	_ LedgerMsg = (*UTXOCreatedMsg)(nil)
	_ LedgerMsg = (*UTXOSpentMsg)(nil)
)
