package ledger

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// ledgerHugeRecordPayloads returns tiny TLV payloads that each declare
// a record length near 2^63 / 2^64. These are the canonical crashers
// for the tlv unbounded-make DoS and MUST be rejected (error) rather
// than panicking the ledger actor on durable replay.
func ledgerHugeRecordPayloads() [][]byte {
	return [][]byte{
		// Unknown odd type 11, length ~2^63 (stream.go make path).
		{0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// Type 3 (a []byte/scalar field), giant declared length.
		{0x03, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// Type 9 (idempotency / classification []byte), giant length.
		{0x09, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{},
	}
}

// fuzzLedgerRoundTrip runs the standard decode→encode→decode invariant
// for a TLV ledger message. The message must never panic on decode; a
// successful decode must re-encode and re-decode cleanly. msgFactory
// returns a fresh zero value each call so the two decode passes are
// independent.
func fuzzLedgerRoundTrip(f *testing.F, seed actor.TLVMessage,
	msgFactory func() actor.TLVMessage) {

	var buf bytes.Buffer
	if err := seed.Encode(&buf); err == nil {
		f.Add(buf.Bytes())
	}
	for _, p := range ledgerHugeRecordPayloads() {
		f.Add(p)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		v := msgFactory()

		// MUST NOT panic on any input.
		if err := v.Decode(bytes.NewReader(data)); err != nil {
			return
		}

		var out bytes.Buffer
		if err := v.Encode(&out); err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		v2 := msgFactory()
		if err := v2.Decode(bytes.NewReader(out.Bytes())); err != nil {
			t.Fatalf("re-decode: %v", err)
		}
	})
}

// FuzzFeePaidMsgDecode fuzzes the FeePaidMsg TLV decoder.
func FuzzFeePaidMsgDecode(f *testing.F) {
	seed := &FeePaidMsg{
		RoundID:        [16]byte{0x01},
		AmountSat:      1234,
		FeeType:        "boarding_fee_paid",
		BlockHeight:    100,
		IdempotencyKey: bytes.Repeat([]byte{0x07}, 32),
	}
	fuzzLedgerRoundTrip(f, seed, func() actor.TLVMessage {
		return &FeePaidMsg{}
	})
}

// FuzzVTXOReceivedMsgDecode fuzzes the VTXOReceivedMsg TLV decoder.
func FuzzVTXOReceivedMsgDecode(f *testing.F) {
	seed := &VTXOReceivedMsg{
		OutpointHash:  [32]byte{0x02},
		OutpointIndex: 1,
		AmountSat:     5000,
		Source:        "round",
		RoundID:       [16]byte{0x03},
	}
	fuzzLedgerRoundTrip(f, seed, func() actor.TLVMessage {
		return &VTXOReceivedMsg{}
	})
}

// FuzzVTXOSentMsgDecode fuzzes the VTXOSentMsg TLV decoder, which
// carries a fixed-width outpoint record alongside scalar fields.
func FuzzVTXOSentMsgDecode(f *testing.F) {
	seed := &VTXOSentMsg{
		SessionID: [32]byte{0x04},
		AmountSat: 6000,
		Outpoint:  wire.OutPoint{Index: 2},
	}
	fuzzLedgerRoundTrip(f, seed, func() actor.TLVMessage {
		return &VTXOSentMsg{}
	})
}

// FuzzExitCostMsgDecode fuzzes the ExitCostMsg TLV decoder.
func FuzzExitCostMsgDecode(f *testing.F) {
	seed := &ExitCostMsg{
		OutpointHash:  [32]byte{0x05},
		OutpointIndex: 3,
		AmountSat:     7000,
		ExitCostSat:   42,
		BlockHeight:   200,
	}
	fuzzLedgerRoundTrip(f, seed, func() actor.TLVMessage {
		return &ExitCostMsg{}
	})
}

// FuzzUTXOCreatedMsgDecode fuzzes the UTXOCreatedMsg TLV decoder.
func FuzzUTXOCreatedMsgDecode(f *testing.F) {
	seed := &UTXOCreatedMsg{
		OutpointHash:   [32]byte{0x06},
		OutpointIndex:  4,
		AmountSat:      8000,
		BlockHeight:    300,
		Classification: "deposit",
	}
	fuzzLedgerRoundTrip(f, seed, func() actor.TLVMessage {
		return &UTXOCreatedMsg{}
	})
}

// FuzzUTXOSpentMsgDecode fuzzes the UTXOSpentMsg TLV decoder.
func FuzzUTXOSpentMsgDecode(f *testing.F) {
	seed := &UTXOSpentMsg{
		OutpointHash:   [32]byte{0x07},
		OutpointIndex:  5,
		AmountSat:      9000,
		BlockHeight:    400,
		Classification: "round_funding",
	}
	fuzzLedgerRoundTrip(f, seed, func() actor.TLVMessage {
		return &UTXOSpentMsg{}
	})
}
