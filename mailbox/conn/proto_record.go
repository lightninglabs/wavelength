package conn

import (
	"fmt"
	"io"
	"reflect"

	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
)

// protoRecordType is a placeholder TLV type used inside the Record() method.
// Callers must wrap the record via tlv.NewRecordT (which assigns the real
// type), not call Record() directly — type 0 would silently conflict with
// other type-0 records.
var protoRecordType tlv.Type = 0

// WrappedProto adapts a proto.Message for use as a tlv.RecordT field. The
// proto is marshaled to bytes on encode and unmarshaled back on decode,
// keeping the TLV codec contract satisfied while storing structured proto
// payloads.
type WrappedProto[T proto.Message] struct {
	Val T
}

// isNil reports whether the generic proto value is nil. Direct nil comparison
// on a generic interface-constrained type is not allowed in Go, so we use
// reflect to check.
func isNil[T proto.Message](v T) bool {
	return reflect.ValueOf(&v).Elem().IsNil()
}

// Record returns a TLV record that serializes the proto message to bytes
// for TLV storage.
func (w *WrappedProto[T]) Record() tlv.Record {
	sizeFunc := func() uint64 {
		if isNil(w.Val) {
			return 0
		}

		return uint64(proto.Size(w.Val))
	}

	return tlv.MakeDynamicRecord(
		protoRecordType, w, sizeFunc, wrappedProtoEncoder[T],
		wrappedProtoDecoder[T],
	)
}

// wrappedProtoEncoder marshals the proto message to the TLV writer.
func wrappedProtoEncoder[T proto.Message](
	w io.Writer, val interface{}, _ *[8]byte,
) error {

	wp, ok := val.(*WrappedProto[T])
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "WrappedProto")
	}

	if isNil(wp.Val) {
		return nil
	}

	data, err := (proto.MarshalOptions{
		Deterministic: true,
	}).Marshal(wp.Val)
	if err != nil {
		return err
	}

	_, err = w.Write(data)

	return err
}

// wrappedProtoDecoder reads bytes from the TLV reader and unmarshals them
// into the proto message. The caller must pre-set Val to a typed zero value
// before decode so the correct concrete type is available.
func wrappedProtoDecoder[T proto.Message](
	r io.Reader, val interface{}, _ *[8]byte, l uint64,
) error {

	wp, ok := val.(*WrappedProto[T])
	if !ok {
		return tlv.NewTypeForDecodingErr(
			val, "WrappedProto", l, l,
		)
	}

	// Bound the declared record length against a sane per-message cap
	// before make([]byte, l). The tlv library does not cap the record
	// length on the non-p2p decode path, so a crafted length near 2^64
	// would otherwise panic with "makeslice: len out of range" or OOM.
	// Callers wrap their Decode entry point in safeTLVReader, which
	// already rejects a length larger than the buffered payload; this
	// guard is defense-in-depth for any direct use of the record.
	if l > maxConnMessageSize {
		return fmt.Errorf("%w: wrapped proto length %d exceeds max %d",
			ErrInvalidTLV, l, maxConnMessageSize)
	}

	data := make([]byte, l)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}

	// Reset the message before unmarshaling to clear any previous
	// state, then unmarshal the fresh bytes.
	if !isNil(wp.Val) {
		proto.Reset(wp.Val)
	}

	return proto.Unmarshal(data, wp.Val)
}
