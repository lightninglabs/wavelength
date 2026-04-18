package ledger

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// TLV type constants for ledger actor messages. These use the
// 0x8xxx range to avoid collisions with other actor subsystems.
const (
	roundConfirmedTLVType  tlv.Type = 0x8001
	vtxosForfeitedTLVType  tlv.Type = 0x8002
	sweepCompletedTLVType  tlv.Type = 0x8003
	oorFinalizedTLVType    tlv.Type = 0x8004
	blockEpochTLVType      tlv.Type = 0x8005
)

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

func (m *RoundConfirmedMsg) TLVType() tlv.Type {
	return roundConfirmedTLVType
}

func (m *RoundConfirmedMsg) Encode(w io.Writer) error {
	return binary.Write(w, binary.BigEndian, &struct {
		RoundID            [16]byte
		TotalVTXOAmountSat int64
		VTXOCount          int32
		BoardingFeeSat     int64
		MiningFeeSat       int64
		BlockHeight        uint32
	}{
		m.RoundID, m.TotalVTXOAmountSat, m.VTXOCount,
		m.BoardingFeeSat, m.MiningFeeSat, m.BlockHeight,
	})
}

func (m *RoundConfirmedMsg) Decode(r io.Reader) error {
	var data struct {
		RoundID            [16]byte
		TotalVTXOAmountSat int64
		VTXOCount          int32
		BoardingFeeSat     int64
		MiningFeeSat       int64
		BlockHeight        uint32
	}

	if err := binary.Read(r, binary.BigEndian, &data); err != nil {
		return fmt.Errorf("decode RoundConfirmedMsg: %w", err)
	}

	m.RoundID = data.RoundID
	m.TotalVTXOAmountSat = data.TotalVTXOAmountSat
	m.VTXOCount = data.VTXOCount
	m.BoardingFeeSat = data.BoardingFeeSat
	m.MiningFeeSat = data.MiningFeeSat
	m.BlockHeight = data.BlockHeight

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

func (m *VTXOsForfeitedMsg) TLVType() tlv.Type {
	return vtxosForfeitedTLVType
}

func (m *VTXOsForfeitedMsg) Encode(w io.Writer) error {
	return binary.Write(w, binary.BigEndian, &struct {
		RoundID        [16]byte
		TotalAmountSat int64
		Count          int32
		RefreshFeeSat  int64
	}{
		m.RoundID, m.TotalAmountSat, m.Count,
		m.RefreshFeeSat,
	})
}

func (m *VTXOsForfeitedMsg) Decode(r io.Reader) error {
	var data struct {
		RoundID        [16]byte
		TotalAmountSat int64
		Count          int32
		RefreshFeeSat  int64
	}

	if err := binary.Read(r, binary.BigEndian, &data); err != nil {
		return fmt.Errorf("decode VTXOsForfeitedMsg: %w", err)
	}

	m.RoundID = data.RoundID
	m.TotalAmountSat = data.TotalAmountSat
	m.Count = data.Count
	m.RefreshFeeSat = data.RefreshFeeSat

	return nil
}

// SweepCompletedMsg is sent when the operator sweeps expired
// VTXOs back into the wallet. The ledger actor records capital
// reclamation.
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
	FeeRateSatVB int64
}

// MessageType returns the message type name for routing.
func (m *SweepCompletedMsg) MessageType() string {
	return "SweepCompletedMsg"
}

func (m *SweepCompletedMsg) TLVType() tlv.Type {
	return sweepCompletedTLVType
}

func (m *SweepCompletedMsg) Encode(w io.Writer) error {
	return binary.Write(w, binary.BigEndian, &struct {
		BatchID            [16]byte
		ReclaimedAmountSat int64
		Count              int32
		BlockHeight        uint32
		FeeRateSatVB       int64
	}{
		m.BatchID, m.ReclaimedAmountSat, m.Count,
		m.BlockHeight, m.FeeRateSatVB,
	})
}

func (m *SweepCompletedMsg) Decode(r io.Reader) error {
	var data struct {
		BatchID            [16]byte
		ReclaimedAmountSat int64
		Count              int32
		BlockHeight        uint32
		FeeRateSatVB       int64
	}

	if err := binary.Read(r, binary.BigEndian, &data); err != nil {
		return fmt.Errorf("decode SweepCompletedMsg: %w", err)
	}

	m.BatchID = data.BatchID
	m.ReclaimedAmountSat = data.ReclaimedAmountSat
	m.Count = data.Count
	m.BlockHeight = data.BlockHeight
	m.FeeRateSatVB = data.FeeRateSatVB

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

func (m *OORFinalizedMsg) TLVType() tlv.Type {
	return oorFinalizedTLVType
}

func (m *OORFinalizedMsg) Encode(w io.Writer) error {
	return binary.Write(w, binary.BigEndian, &struct {
		SessionID       [32]byte
		InputAmountSat  int64
		OutputAmountSat int64
	}{
		m.SessionID, m.InputAmountSat, m.OutputAmountSat,
	})
}

func (m *OORFinalizedMsg) Decode(r io.Reader) error {
	var data struct {
		SessionID       [32]byte
		InputAmountSat  int64
		OutputAmountSat int64
	}

	if err := binary.Read(r, binary.BigEndian, &data); err != nil {
		return fmt.Errorf("decode OORFinalizedMsg: %w", err)
	}

	m.SessionID = data.SessionID
	m.InputAmountSat = data.InputAmountSat
	m.OutputAmountSat = data.OutputAmountSat

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

func (m *BlockEpochMsg) TLVType() tlv.Type {
	return blockEpochTLVType
}

func (m *BlockEpochMsg) Encode(w io.Writer) error {
	return binary.Write(w, binary.BigEndian, &struct {
		BlockHeight uint32
		BlockHash   [32]byte
	}{
		m.BlockHeight, m.BlockHash,
	})
}

func (m *BlockEpochMsg) Decode(r io.Reader) error {
	var data struct {
		BlockHeight uint32
		BlockHash   [32]byte
	}

	if err := binary.Read(r, binary.BigEndian, &data); err != nil {
		return fmt.Errorf("decode BlockEpochMsg: %w", err)
	}

	m.BlockHeight = data.BlockHeight
	m.BlockHash = data.BlockHash

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
