package bip322

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMessageHashVectors asserts the BIP-322 message hash vectors.
func TestMessageHashVectors(t *testing.T) {
	t.Parallel()

	emptyMessageHashHex := "c90c269c4f8fcbe6880f72a721ddfbf1914268a79" +
		"4cbb21cfafee13770ae19f1"
	helloWorldHashHex := "f0eb03b1a75ac6d9847f55c624a99169b5dccba2a3" +
		"1f5b23bea77ba270de0a7a"

	testCases := []struct {
		name    string
		message string
		wantHex string
	}{
		{
			name:    "empty message",
			message: "",
			wantHex: emptyMessageHashHex,
		},
		{
			name:    "hello world",
			message: "Hello World",
			wantHex: helloWorldHashHex,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := MessageHash([]byte(tc.message))
			gotHex := hex.EncodeToString(got[:])
			require.Equal(t, tc.wantHex, gotHex)
		})
	}
}
