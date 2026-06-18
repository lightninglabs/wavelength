package ledger

import (
	"bytes"
	"encoding/hex"
	"io"
	"math"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// tlvMessage is the minimal codec surface every ledger message satisfies.
type tlvMessage interface {
	Encode(io.Writer) error

	Decode(io.Reader) error
}

// assertRoundTrip encodes src, decodes the bytes into dst, and asserts the two
// are deeply equal. This is the core wire-stability check: every durable
// message must survive an encode/decode cycle unchanged.
func assertRoundTrip(t require.TestingT, src, dst tlvMessage) {
	var buf bytes.Buffer
	require.NoError(t, src.Encode(&buf))
	require.NoError(t, dst.Decode(bytes.NewReader(buf.Bytes())))
	require.Equal(t, src, dst)
}

// genBytes draws a fixed-length byte slice copied into an array of size n.
func genBytes16(rt *rapid.T) [16]byte {
	var a [16]byte
	copy(a[:], rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(rt, "b16"))

	return a
}

func genBytes32(rt *rapid.T) [32]byte {
	var a [32]byte
	copy(a[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "b32"))

	return a
}

// genAmount draws a non-negative satoshi amount. Decode rejects values past
// MaxInt64 (a malformed payload guard), so the round-trippable domain is
// [0, MaxInt64].
func genAmount(rt *rapid.T, label string) int64 {
	return rapid.Int64Range(0, math.MaxInt64).Draw(rt, label)
}

// genNonEmptyBytes draws a non-empty byte slice, avoiding the nil-vs-empty
// ambiguity that an optional zero-length TLV field would introduce.
func genNonEmptyBytes(rt *rapid.T, label string) []byte {
	return rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, label)
}

// TestFeePaidMsgRoundTrip property-checks FeePaidMsg encode/decode.
func TestFeePaidMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &FeePaidMsg{
			RoundID:        genBytes16(rt),
			AmountSat:      genAmount(rt, "amt"),
			FeeType:        rapid.String().Draw(rt, "feeType"),
			BlockHeight:    rapid.Uint32().Draw(rt, "height"),
			IdempotencyKey: genNonEmptyBytes(rt, "idem"),
		}
		assertRoundTrip(rt, src, &FeePaidMsg{})
	})
}

// TestVTXOReceivedMsgRoundTrip property-checks VTXOReceivedMsg encode/decode.
func TestVTXOReceivedMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &VTXOReceivedMsg{
			OutpointHash:  genBytes32(rt),
			OutpointIndex: rapid.Uint32().Draw(rt, "idx"),
			AmountSat:     genAmount(rt, "amt"),
			Source:        rapid.String().Draw(rt, "source"),
			RoundID:       genBytes16(rt),
		}
		assertRoundTrip(rt, src, &VTXOReceivedMsg{})
	})
}

// TestVTXOSentMsgRoundTrip property-checks VTXOSentMsg encode/decode,
// exercising the shared tlvutil.OutPointRecord path.
func TestVTXOSentMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		var h chainhash.Hash
		copy(h[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(rt, "oph"))

		src := &VTXOSentMsg{
			SessionID: genBytes32(rt),
			RoundID:   genBytes16(rt),
			Outpoint: wire.OutPoint{
				Hash:  h,
				Index: rapid.Uint32().Draw(rt, "opi"),
			},
			AmountSat: genAmount(rt, "amt"),
		}
		assertRoundTrip(rt, src, &VTXOSentMsg{})
	})
}

// TestExitCostMsgRoundTrip property-checks ExitCostMsg encode/decode.
func TestExitCostMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &ExitCostMsg{
			OutpointHash:  genBytes32(rt),
			OutpointIndex: rapid.Uint32().Draw(rt, "idx"),
			AmountSat:     genAmount(rt, "amt"),
			ExitCostSat:   genAmount(rt, "cost"),
			BlockHeight:   rapid.Uint32().Draw(rt, "height"),
		}
		assertRoundTrip(rt, src, &ExitCostMsg{})
	})
}

// TestUTXOCreatedMsgRoundTrip property-checks UTXOCreatedMsg encode/decode.
func TestUTXOCreatedMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &UTXOCreatedMsg{
			OutpointHash:   genBytes32(rt),
			OutpointIndex:  rapid.Uint32().Draw(rt, "idx"),
			AmountSat:      genAmount(rt, "amt"),
			BlockHeight:    rapid.Uint32().Draw(rt, "height"),
			Classification: rapid.String().Draw(rt, "class"),
		}
		assertRoundTrip(rt, src, &UTXOCreatedMsg{})
	})
}

// TestUTXOSpentMsgRoundTrip property-checks UTXOSpentMsg encode/decode.
func TestUTXOSpentMsgRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		src := &UTXOSpentMsg{
			OutpointHash:   genBytes32(rt),
			OutpointIndex:  rapid.Uint32().Draw(rt, "idx"),
			AmountSat:      genAmount(rt, "amt"),
			BlockHeight:    rapid.Uint32().Draw(rt, "height"),
			Classification: rapid.String().Draw(rt, "class"),
		}
		assertRoundTrip(rt, src, &UTXOSpentMsg{})
	})
}

// TestVTXOSentMsgGolden pins the exact wire bytes of a fully-populated
// VTXOSentMsg. It is the cross-version guard required by the migration: a
// previously persisted durable message (these bytes) must always decode to the
// same value, and the current encoder must still produce these exact bytes. Any
// accidental wire-format change -- including a regression in the shared
// tlvutil.OutPointRecord encoding -- breaks this test loudly.
func TestVTXOSentMsgGolden(t *testing.T) {
	t.Parallel()

	// A deterministic, fully-populated message.
	var sessionID [32]byte
	for i := range sessionID {
		sessionID[i] = byte(i + 1)
	}
	var roundID [16]byte
	for i := range roundID {
		roundID[i] = byte(0xa0 + i)
	}
	var opHash chainhash.Hash
	for i := range opHash {
		opHash[i] = byte(0x10 + i)
	}

	src := &VTXOSentMsg{
		SessionID: sessionID,
		RoundID:   roundID,
		Outpoint: wire.OutPoint{
			Hash:  opHash,
			Index: 0x04030201,
		},
		AmountSat: 0x0102030405,
	}

	const golden = "01200102030405060708090a0b0c0d0e0f1011121314151617" +
		"18191a1b1c1d1e1f20030800000001020304050510a0a1a2a3a4a5" +
		"a6a7a8a9aaabacadaeaf0724101112131415161718191a1b1c1d1e" +
		"1f202122232425262728292a2b2c2d2e2f01020304"

	var buf bytes.Buffer
	require.NoError(t, src.Encode(&buf))
	require.Equal(t, golden, hex.EncodeToString(buf.Bytes()))

	// The golden bytes must decode back to the original message.
	var got VTXOSentMsg
	require.NoError(t, got.Decode(bytes.NewReader(buf.Bytes())))
	require.Equal(t, src, &got)
}
