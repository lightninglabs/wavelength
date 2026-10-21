package tlvutil

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/lightningnetwork/lnd/tlv"
)

// ErrRecordLengthMismatch is returned by a decoder wrapped with ExactDecoder
// when the record's decoder consumes fewer bytes than the stream framed for
// it.
var ErrRecordLengthMismatch = errors.New("tlv record length mismatch")

// ExactDecoder wraps a custom record decoder so it can only read the l bytes
// the stream framed for the record, and must read all of them.
//
// tlv.Stream documents that a record decoder consumes exactly l bytes but
// does not enforce it: it hands the decoder the underlying reader. A
// count-driven decoder (a varint element count followed by that many
// elements) that over-reads therefore swallows the next record's header, and
// one that under-reads leaves value bytes to be parsed as a header. Either
// way the stream and any framing validation done ahead of it disagree about
// where records start, so a payload that passed the validation can still hand
// a later DVarBytes record an arbitrary declared length and panic or exhaust
// memory in make([]byte, length).
//
// Wrapping the decoder makes both failure modes a decode error, which keeps
// the stream in step with the framing.
func ExactDecoder(dec tlv.Decoder) tlv.Decoder {
	return func(r io.Reader, val interface{}, buf *[8]byte,
		l uint64) error {

		if l > math.MaxInt64 {
			return fmt.Errorf("%w: record length %d out of range",
				ErrRecordLengthMismatch, l)
		}

		limited := &io.LimitedReader{R: r, N: int64(l)}
		if err := dec(limited, val, buf, l); err != nil {
			return err
		}

		if limited.N != 0 {
			return fmt.Errorf("%w: decoder left %d of %d "+
				"bytes unread", ErrRecordLengthMismatch,
				limited.N, l)
		}

		return nil
	}
}
