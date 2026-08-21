package recovery

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// FuzzDecodeSessionState drives attacker-controlled bytes through the durable
// session-state decoder. SessionState persists across restarts, so a corrupt
// or hostile on-disk blob must decode to an error, never panic or allocate an
// unbounded buffer from a declared TLV length. The vulnerable surfaces are the
// outer DVarBytes records (txStates, confirmHeights, failedTxid, lastError)
// and the unknown-record discard path.
//
// Encode is deterministic and canonical, so any successful decode must
// round-trip through encode/decode.
func FuzzDecodeSessionState(f *testing.F) {
	if b := fuzzSeedSessionState(); b != nil {
		f.Add(b)
	}

	f.Add([]byte{})
	f.Add([]byte{0x00})

	// txStates is TLV type 3 and decodes via DVarBytes. A tiny payload
	// declaring a huge length is the canonical unbounded-make crasher.
	f.Add([]byte{
		0x03, 0xfe, 0xff, 0xff, 0xff, 0xff,
	})

	// lastError is TLV type 9, also DVarBytes-backed and unbounded.
	f.Add([]byte{
		0x09, 0xfe, 0xff, 0xff, 0xff, 0xff,
	})

	// The canonical near-int64-max length crasher: type 0x0b followed by an
	// 8-byte BigSize (0xff prefix) declaring 0x7fffffffffffffff value bytes
	// in a 10-byte envelope. On the unhardened path this drives
	// make([]byte, declaredLength) to "makeslice: cap out of range"; the
	// framing pre-validator must reject it before any allocation. Pinned as
	// a deterministic regression seed so the guard runs on every test pass.
	f.Add([]byte{
		0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		// The primary invariant: decoding hostile bytes must return an
		// error, never panic or exhaust memory.
		got, err := DecodeSessionState(data)
		if err != nil {
			return
		}

		// A canonical state must round-trip through encode/decode.
		b2, err := EncodeSessionState(got)
		if err != nil {
			t.Fatalf("re-encode decoded state: %v", err)
		}

		if _, err := DecodeSessionState(b2); err != nil {
			t.Fatalf("re-decode re-encoded state: %v", err)
		}
	})
}

// fuzzSeedSessionState builds a representative state and returns its encoded
// bytes to seed the corpus.
func fuzzSeedSessionState() []byte {
	var h1, h2 chainhash.Hash
	h1[0] = 1
	h2[0] = 2

	state := &SessionState{
		TxStates: map[chainhash.Hash]TxState{
			h1: TxStateConfirmed,
			h2: TxStatePending,
		},
		ConfirmHeights: map[chainhash.Hash]int32{
			h1: 123,
		},
		FailedTxid: fn.Some(h2),
		LastError:  "package rejected",
	}

	b, err := EncodeSessionState(state)
	if err != nil {
		return nil
	}

	return b
}
