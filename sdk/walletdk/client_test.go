package walletdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSendRejectsAmbiguousDestination ensures wrappers cannot accidentally set
// both destination fields and have walletdk pick one.
func TestSendRejectsAmbiguousDestination(t *testing.T) {
	t.Parallel()

	client := &Client{canWallet: true}

	_, err := client.Send(context.Background(), SendRequest{
		Invoice:        "lnbcrt...",
		OnchainAddress: "bcrt1...",
	})
	require.ErrorContains(t, err, "not both")
}

// TestListRejectsUnknownKind ensures SDK-side filters fail before a request is
// sent with ENTRY_KIND_UNSPECIFIED.
func TestListRejectsUnknownKind(t *testing.T) {
	t.Parallel()

	client := &Client{canWallet: true}

	_, err := client.List(context.Background(), ListRequest{
		Kinds: []EntryKind{"junk"},
	})
	require.ErrorContains(t, err, "unknown entry kind")
}

// TestSubscribeRejectsUnknownKind mirrors List validation for streaming
// subscriptions.
func TestSubscribeRejectsUnknownKind(t *testing.T) {
	t.Parallel()

	client := &Client{canWallet: true}

	_, _, err := client.Subscribe(context.Background(), SubscribeRequest{
		Kinds: []EntryKind{"junk"},
	})
	require.ErrorContains(t, err, "unknown entry kind")
}
