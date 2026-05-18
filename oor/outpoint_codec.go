package oor

import (
	"github.com/btcsuite/btcd/wire"
)

func encodeOutpoints(outpoints []wire.OutPoint) ([]byte, error) {
	blobs := make([][]byte, 0, len(outpoints))
	for i := range outpoints {
		blobs = append(blobs, outPointBytes(outpoints[i]))
	}

	return encodeLengthPrefixedBlobList(blobs)
}
