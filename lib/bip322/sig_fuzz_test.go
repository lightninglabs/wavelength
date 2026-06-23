package bip322

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// FuzzDecodeSig drives attacker-controlled bytes through the full-format
// BIP-322 signature decoder. A BIP-322 signature payload is supplied by the
// remote client and crosses the trust boundary as the serialized to_sign
// transaction, so the decoder must never panic from a malformed or
// length-inflated payload.
//
// DecodeSig parses the bytes via wire.MsgTx.Deserialize, which bounds its own
// input/output/witness counts (maxTxInPerMessage et al.) and reuses a bounded
// script pool, so the framing is hardened inside btcd rather than via a TLV
// pre-validator. This target asserts the no-panic property: any successful
// decode must re-encode without error, but a strict byte round-trip is not
// required because the canonical serialization may differ from a hostile
// non-canonical encoding.
func FuzzDecodeSig(f *testing.F) {
	if b := fuzzSeedSig(); b != nil {
		f.Add(b)
	}

	f.Add([]byte{})
	f.Add([]byte{0x00})

	// A transaction header declaring a near-2^64 input count: the canonical
	// unbounded-allocation probe for the wire decoder. btcd must reject the
	// count before allocating rather than crash.
	f.Add([]byte{
		0x01, 0x00, 0x00, 0x00, // version
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // varint
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		// The primary invariant: decoding hostile bytes must return an
		// error, never panic or exhaust memory.
		got, err := DecodeSig(data)
		if err != nil {
			return
		}

		// Anything that decoded must re-encode without error. We do not
		// assert byte equality because a non-canonical witness/legacy
		// encoding can decode and re-serialize to canonical form.
		if _, err := got.Encode(); err != nil {
			t.Fatalf("re-encode decoded signature: %v", err)
		}
	})
}

// fuzzSeedSig builds a valid full-format signature payload to seed the corpus.
// Errors yield a nil return so seed-build failure cannot fail target setup.
func fuzzSeedSig() []byte {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 0},
		Witness:          wire.TxWitness{{0x01, 0x02}},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_RETURN},
	})

	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		return nil
	}

	return b.Bytes()
}
