package types

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

// FuzzDecodeJoinRoundAuthMessage drives attacker-controlled bytes through the
// join-round auth decoder. These bytes cross the client/server trust boundary
// (a client supplies them as the BIP-322-signed join intent), so the decoder
// must never panic, OOM, or allocate an unbounded buffer from a declared TLV
// length. The decoder enforces strict framing, so any successfully decoded
// value must re-encode and re-decode cleanly.
func FuzzDecodeJoinRoundAuthMessage(f *testing.F) {
	// Seed with a valid canonical message so the fuzzer starts from a
	// structurally sound corpus and mutates outward from there.
	if b := fuzzSeedJoinRoundAuthMessage(); b != nil {
		f.Add(b)
	}

	// Degenerate seeds that probe the early-return and framing guards.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x01})

	// A record that declares a huge length but carries no payload. This is
	// the canonical unbounded-make crasher: type 2, length 0xffffffff.
	f.Add([]byte{
		0x02, 0xfe, 0xff, 0xff, 0xff, 0xff,
	})

	// The canonical near-int64-max length crasher: type 0x0b followed by an
	// 8-byte BigSize (0xff prefix) declaring 0x7fffffffffffffff value bytes
	// in a 10-byte envelope. On the unhardened path this drives
	// make([]byte, declaredLength) to "makeslice: cap out of range"; the
	// framing pre-validator must reject it before any allocation. Pinned as
	// a deterministic regression seed so the guard is exercised on every
	// `go test` run, not just under active fuzzing.
	f.Add([]byte{
		0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		// The primary invariant: decoding hostile bytes must return an
		// error, never panic or exhaust memory.
		got, err := DecodeJoinRoundAuthMessage(data)
		if err != nil {
			return
		}

		// A value that decoded cleanly must round-trip. Re-encoding then
		// re-decoding catches asymmetries where the decoder accepts
		// something the encoder cannot reproduce.
		b2, err := JoinRoundAuthMessage(got)
		if err != nil {
			t.Fatalf("re-encode decoded message: %v", err)
		}

		if _, err := DecodeJoinRoundAuthMessage(b2); err != nil {
			t.Fatalf("re-decode re-encoded message: %v", err)
		}
	})
}

// fuzzSeedJoinRoundAuthMessage builds a valid canonical join-auth payload to
// seed the corpus. It mirrors testJoinRoundAuthRequest but returns only the
// bytes, swallowing errors so a seed-build failure cannot fail the fuzz target
// setup; a nil return simply means the seed is skipped.
func fuzzSeedJoinRoundAuthMessage() []byte {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil
	}

	req := &JoinRoundRequest{
		Identifier: priv.PubKey(),
	}

	b, err := JoinRoundAuthMessage(req)
	if err != nil {
		return nil
	}

	return b
}
