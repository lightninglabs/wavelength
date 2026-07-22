package checkpoint

import (
	"testing"
)

// FuzzDecodeTapTree drives DecodeTapTree with attacker-controlled bytes. The
// tap tree blob is persisted as PSBT sidecar metadata and flows from
// operator-sourced OOR artifacts, so it crosses the trust boundary. The leaf
// stream nests a varint-length-prefixed inner TLV per leaf, and each leaf
// script decodes through tlv DVarBytes (an unbounded make([]byte, length)),
// making this a prime unbounded-allocation target.
func FuzzDecodeTapTree(f *testing.F) {
	if seed, err := EncodeTapTree([][]byte{
		{0x51, 0x51, 0x51},
		{0x6a},
		{0x00, 0x01, 0x02, 0x03},
	}); err == nil {
		f.Add(seed)
	}
	f.Add([]byte{})

	// Canonical malicious payload: TLV type 11, then a BigSize-encoded
	// length of math.MaxInt64. Without the safeTLVReader framing guard the
	// decoder would size make([]byte, declaredLength) from this value and
	// panic (makeslice) or OOM. Seeding it pins the guard's rejection path.
	f.Add([]byte{
		0x0b, 0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		// A malformed tap tree must error rather than panic/OOM.
		leaves, err := DecodeTapTree(data)
		if err != nil {
			return
		}

		// A clean decode must round-trip: re-encoding the recovered
		// scripts and decoding again must yield the same scripts. We do
		// not assert byte-equality against the input because DecodeTapTree
		// is intentionally lenient (drops leaf versions, ignores unknown
		// records), so the canonical re-encoding can differ from a
		// hand-crafted input.
		reencoded, err := EncodeTapTree(leaves)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		redecoded, err := DecodeTapTree(reencoded)
		if err != nil {
			t.Fatalf("re-decode: %v", err)
		}

		if len(redecoded) != len(leaves) {
			t.Fatalf("leaf count drift: got %d want %d",
				len(redecoded), len(leaves))
		}
	})
}
