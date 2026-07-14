package oor

import (
	"github.com/lightninglabs/wavelength/lib/tx/checkpoint"
)

// EncodeTapTree encodes a set of tapscript leaves into a single byte blob.
func EncodeTapTree(leaves [][]byte) ([]byte, error) {
	return checkpoint.EncodeTapTree(leaves)
}

// DecodeTapTree decodes a tap tree encoding produced by EncodeTapTree.
func DecodeTapTree(data []byte) ([][]byte, error) {
	return checkpoint.DecodeTapTree(data)
}
