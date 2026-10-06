package tlv

import "io"

type Type uint64

type Record struct{}

type Stream struct{}

func NewStream(...Record) (*Stream, error) {
	return &Stream{}, nil
}

func (*Stream) Encode(io.Writer) error {
	return nil
}

func (*Stream) Decode(io.Reader) error {
	return nil
}
