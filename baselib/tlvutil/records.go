package tlvutil

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

// This file centralizes the encode/decode logic for the domain types that
// tlv.MakePrimitiveRecord does NOT already handle. The lnd tlv package natively
// covers uint8/16/32/64, [32]byte, [33]byte, **btcec.PublicKey, [64]byte, and
// []byte, so those types need no helper here -- call MakePrimitiveRecord
// directly:
//
//   - *btcec.PublicKey : tlv.MakePrimitiveRecord(typ, &pubKey)   // 33-byte
//   - chainhash.Hash   : tlv.MakePrimitiveRecord(typ, (*[32]byte)(&hash))
//   - [16]byte etc.    : slice it ([:]) into a *[]byte, as the codecs do today
//
// wire.OutPoint is the one recurring field with no native primitive (a 32-byte
// hash plus a 4-byte index in a single record), so it gets a shared constructor
// below. The byte format matches the encoding already on the wire and on disk.

// outPointSize is the fixed encoded size of an outpoint: a 32-byte hash
// followed by a 4-byte index.
const outPointSize = chainhash.HashSize + 4

// OutPointRecord returns a TLV record under the given type that encodes op as
// 32 hash bytes followed by a 4-byte little-endian index (36 bytes total). This
// is the canonical client-side outpoint encoding shared by the ledger and OOR
// message codecs. The caller's field stays a plain wire.OutPoint.
func OutPointRecord(typ tlv.Type, op *wire.OutPoint) tlv.Record {
	return tlv.MakeStaticRecord(
		typ, op, outPointSize, encodeOutPoint, decodeOutPoint,
	)
}

// encodeOutPoint writes the outpoint as 32 hash bytes plus a 4-byte
// little-endian index.
func encodeOutPoint(w io.Writer, val interface{}, _ *[8]byte) error {
	op, ok := val.(*wire.OutPoint)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "wire.OutPoint")
	}

	if _, err := w.Write(op.Hash[:]); err != nil {
		return err
	}

	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], op.Index)

	_, err := w.Write(buf[:])

	return err
}

// decodeOutPoint reverses encodeOutPoint. It rejects any payload that is not
// exactly 36 bytes so a corrupt stream surfaces at the decode boundary rather
// than producing a truncated or zero-padded outpoint.
func decodeOutPoint(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	op, ok := val.(*wire.OutPoint)
	if !ok {
		return tlv.NewTypeForDecodingErr(
			val, "wire.OutPoint", l, outPointSize,
		)
	}

	if l != outPointSize {
		return fmt.Errorf("outpoint TLV payload must be %d "+
			"bytes, got %d", outPointSize, l)
	}

	if _, err := io.ReadFull(r, op.Hash[:]); err != nil {
		return err
	}

	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return err
	}

	op.Index = binary.LittleEndian.Uint32(buf[:])

	return nil
}
