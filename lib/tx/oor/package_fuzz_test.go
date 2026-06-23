package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// fuzzSeedSubmitPackage builds a representative, valid submit package whose
// marshaled bytes serve as a structured seed for the fuzzer. Mirroring the
// shape exercised in package_test.go gives the fuzzer a real TLV framing to
// mutate from rather than purely random noise.
func fuzzSeedSubmitPackage() ([]byte, bool) {
	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{})
	checkpointTx.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	checkpointTx.AddTxOut(arkscript.AnchorOutput())

	checkpointPSBT, err := psbt.NewFromUnsignedTx(checkpointTx)
	if err != nil {
		return nil, false
	}

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  checkpointTx.TxHash(),
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{Value: 5, PkScript: []byte{0x51}})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPSBT, err := psbt.NewFromUnsignedTx(arkTx)
	if err != nil {
		return nil, false
	}

	pkg := &SubmitPackage{
		ArkPSBT:         arkPSBT,
		CheckpointPSBTs: []*psbt.Packet{checkpointPSBT},
	}

	raw, err := MarshalSubmitPackage(pkg)
	if err != nil {
		return nil, false
	}

	return raw, true
}

// FuzzUnmarshalSubmitPackage drives UnmarshalSubmitPackage with attacker
// controlled bytes. These bytes cross the OOR submit RPC trust boundary and
// are persisted durably, so a decode must never panic, OOM, or read OOB. The
// blob-list framing (count + per-blob length prefixes) is the prime suspect:
// both the outer count and each inner length feed make() calls.
func FuzzUnmarshalSubmitPackage(f *testing.F) {
	if seed, ok := fuzzSeedSubmitPackage(); ok {
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
		// A successful decode is not required; we only require that a
		// malformed payload fails cleanly instead of crashing the
		// process.
		pkg, err := UnmarshalSubmitPackage(data)
		if err != nil {
			return
		}

		// On a clean decode the re-marshal must succeed and round-trip
		// back to an equivalent structure, proving the decoder produced
		// a self-consistent value.
		out, err := MarshalSubmitPackage(pkg)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}

		if _, err := UnmarshalSubmitPackage(out); err != nil {
			t.Fatalf("re-unmarshal: %v", err)
		}
	})
}

// FuzzDecodeBlobList targets the internal blob-list codec directly so the
// fuzzer can reach the count/length make() sites without first satisfying the
// outer package TLV framing.
func FuzzDecodeBlobList(f *testing.F) {
	if seed, err := encodeBlobList([][]byte{
		{0x01, 0x02}, {}, {0xff},
	}); err == nil {
		f.Add(seed)
	}
	f.Add([]byte{})

	// Canonical malicious payload: a BigSize-encoded count of math.MaxInt64.
	// decodeBlobList must reject this against the remaining-bytes bound
	// before reaching make([][]byte, count).
	f.Add([]byte{
		0xff, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		blobs, err := decodeBlobList(data)
		if err != nil {
			return
		}

		out, err := encodeBlobList(blobs)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}

		if !bytes.Equal(out, data) {
			t.Fatalf("blob list not byte-stable:\n got %x\nwant %x",
				out, data)
		}
	})
}
