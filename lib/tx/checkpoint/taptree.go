package checkpoint

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	typeTapscriptType   tlv.Type = 1
	typeTapscriptLeaves tlv.Type = 3

	typeTapLeafVersion tlv.Type = 1
	typeTapLeafScript  tlv.Type = 2
)

// EncodeTapTree encodes a set of tapscript leaves into a single byte blob.
//
// EncodeTapTree uses the same TLV leaf encoding as waddrmgr.Tapscript, so
// durability and persistence layers can share a single representation. For v0,
// we only populate the full tree leaves with the base leaf version.
func EncodeTapTree(leaves [][]byte) ([]byte, error) {
	tapscriptLeaves := make([]txscript.TapLeaf, 0, len(leaves))
	for _, script := range leaves {
		tapscriptLeaves = append(tapscriptLeaves, txscript.TapLeaf{
			LeafVersion: txscript.BaseLeafVersion,
			Script:      script,
		})
	}

	typ := uint8(0)
	tlvRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(typeTapscriptType, &typ),
		tlv.MakeDynamicRecord(
			typeTapscriptLeaves,
			&tapscriptLeaves,
			func() uint64 {
				return recordSize(
					leavesEncoder, &tapscriptLeaves,
				)
			},
			leavesEncoder,
			leavesDecoder,
		),
	}

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// DecodeTapTree decodes a tap tree encoding produced by EncodeTapTree.
//
// DecodeTapTree is intentionally lenient about leaf version in v0: it ignores
// the version and returns the raw script bytes for each leaf.
func DecodeTapTree(data []byte) ([][]byte, error) {
	var (
		typ    uint8
		leaves []txscript.TapLeaf
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(typeTapscriptType, &typ),
		tlv.MakeDynamicRecord(
			typeTapscriptLeaves,
			&leaves,
			func() uint64 {
				return recordSize(leavesEncoder, &leaves)
			},
			leavesEncoder,
			leavesDecoder,
		),
	)
	if err != nil {
		return nil, err
	}

	// Validate the outer record framing against the bytes physically
	// present before decoding. The lnd tlv non-P2P decode path sizes
	// allocations from the declared record length without bounding it, so
	// an attacker-controlled length could otherwise panic or OOM here.
	safeReader, err := safeTLVReader(data)
	if err != nil {
		return nil, err
	}

	_, err = tlvStream.DecodeWithParsedTypes(safeReader)
	if err != nil {
		return nil, err
	}

	scripts := make([][]byte, 0, len(leaves))
	for _, leaf := range leaves {
		scripts = append(scripts, leaf.Script)
	}

	return scripts, nil
}

// leavesEncoder encodes a slice of tap leaves using the waddrmgr TLV format.
func leavesEncoder(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*[]txscript.TapLeaf); ok {
		for _, leaf := range *v {
			leafVersion := uint8(leaf.LeafVersion)
			tlvRecords := []tlv.Record{
				tlv.MakePrimitiveRecord(
					typeTapLeafVersion, &leafVersion,
				),
			}

			if len(leaf.Script) > 0 {
				tlvRecords = append(
					tlvRecords, tlv.MakePrimitiveRecord(
						typeTapLeafScript, &leaf.Script,
					),
				)
			}

			tlvStream, err := tlv.NewStream(tlvRecords...)
			if err != nil {
				return err
			}

			var leafTLVBytes bytes.Buffer
			if err := tlvStream.Encode(&leafTLVBytes); err != nil {
				return err
			}

			tlvLen := uint64(len(leafTLVBytes.Bytes()))
			if err := tlv.WriteVarInt(w, tlvLen, buf); err != nil {
				return err
			}

			if _, err := w.Write(leafTLVBytes.Bytes()); err != nil {
				return err
			}
		}

		return nil
	}

	return tlv.NewTypeForEncodingErr(val, "[]txscript.TapLeaf")
}

// leavesDecoder decodes a slice of tap leaves using the waddrmgr TLV format.
func leavesDecoder(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*[]txscript.TapLeaf); ok {
		var leaves []txscript.TapLeaf

		innerTlvReader := io.LimitedReader{
			R: r,
			N: int64(l),
		}

		for {
			blobSize, err := tlv.ReadVarInt(&innerTlvReader, buf)
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}

			// Bound the declared per-leaf blob length against the
			// bytes still available in the outer record. A leaf
			// claiming more bytes than remain is malformed; without
			// this the inner stream decode below would size a
			// make([]byte, length) from this untrusted value and
			// could panic or OOM.
			if blobSize > uint64(innerTlvReader.N) {
				return fmt.Errorf("%w: leaf blob declared %d, "+
					"%d remaining", ErrTLVRecordTooLarge,
					blobSize, innerTlvReader.N)
			}

			// Buffer the exact leaf blob and validate its inner
			// record framing before decoding, so the inner DVarBytes
			// and unknown-record paths cannot over-allocate either.
			leafBlob := make([]byte, blobSize)
			if _, err := io.ReadFull(
				&innerTlvReader, leafBlob,
			); err != nil {
				return err
			}

			leafReader, err := safeTLVReader(leafBlob)
			if err != nil {
				return err
			}

			var (
				leafVersion uint8
				script      []byte
			)
			tlvStream, err := tlv.NewStream(
				tlv.MakePrimitiveRecord(
					typeTapLeafVersion, &leafVersion,
				),
				tlv.MakePrimitiveRecord(
					typeTapLeafScript, &script,
				),
			)
			if err != nil {
				return err
			}

			parsedTypes, err := tlvStream.DecodeWithParsedTypes(
				leafReader,
			)
			if err != nil {
				return err
			}

			leaf := txscript.TapLeaf{
				LeafVersion: txscript.TapscriptLeafVersion(
					leafVersion,
				),
			}

			if _, ok := parsedTypes[typeTapLeafScript]; ok {
				leaf.Script = script
			}

			leaves = append(leaves, leaf)
		}

		*v = leaves

		return nil
	}

	return tlv.NewTypeForDecodingErr(val, "[]txscript.TapLeaf", l, l)
}

// recordSize returns the amount of bytes this TLV record will occupy when
// encoded.
func recordSize(encoder tlv.Encoder, v interface{}) uint64 {
	var (
		b   bytes.Buffer
		buf [8]byte
	)

	if err := encoder(&b, v, &buf); err != nil {
		return 0
	}

	return uint64(len(b.Bytes()))
}
