package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// boundedWaitRPC captures the response-wait lifecycle without delaying a test.
type boundedWaitRPC struct {
	recordingRPCClient
	deadline time.Time
	done     <-chan struct{}
}

// AwaitRPC captures the bounded response context used after request admission.
func (r *boundedWaitRPC) AwaitRPC(ctx context.Context, _ string,
	_ proto.Message) error {

	r.deadline, _ = ctx.Deadline()
	r.done = ctx.Done()

	return nil
}

// TestIndexedWaitBoundsLostReply proves a lost ephemeral operator response
// cannot inherit the ten-minute mailbox waiter TTL. Earlier callers win and
// successful responses cancel their request-owned timer immediately.
func TestIndexedWaitBoundsLostReply(t *testing.T) {
	t.Parallel()
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	script := append(
		[]byte{0x51, 0x20}, key.PubKey().SerializeCompressed()[1:]...,
	)
	for _, callerBounded := range []bool{false, true} {
		rpc := &boundedWaitRPC{}
		client := New(
			rpc, &PrivKeySchnorrSigner{
				Key: key,
			}, "test-server", "client:test",
			fn.None[btclog.Logger](),
		)
		ctx := t.Context()
		cancel := func() {}
		if callerBounded {
			ctx, cancel = context.WithTimeout(ctx, time.Second)
		}
		before := time.Now()
		_, err := client.WaitForVTXOByScriptTaproot(
			ctx, TaprootScriptScope{
				PkScript: script,
			},
			arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
			2000,
		)
		cancel()
		require.NoError(t, err)
		bound := 7 * time.Second
		if callerBounded {
			bound = time.Second
		}
		require.WithinDuration(
			t, before.Add(bound), rpc.deadline, time.Second,
		)
		select {
		case <-rpc.done:
		default:
			t.Fatal("completed query retained its request timer")
		}
	}
}
