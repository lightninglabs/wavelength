package darepod

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/unroller"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// mockUnrollerRef captures Tell calls for verification.
type mockUnrollerRef struct {
	lastMsg unroller.UnrollerMsg
	tellErr error
	calls   int
}

func (m *mockUnrollerRef) ID() string { return "mock-unroller" }

func (m *mockUnrollerRef) Tell(_ context.Context,
	msg unroller.UnrollerMsg) error {

	m.lastMsg = msg
	m.calls++

	return m.tellErr
}

// TestChainResolverAdapterID verifies the adapter returns the
// expected identifier.
func TestChainResolverAdapterID(t *testing.T) {
	t.Parallel()

	adapter := newChainResolverAdapter(&mockUnrollerRef{})
	require.Equal(t, "chain-resolver-adapter", adapter.ID())
}

// TestChainResolverExpiringNotificationMapsToUnrollRequest verifies
// that an ExpiringNotification is correctly converted to an
// UnrollRequest with the right outpoint.
func TestChainResolverExpiringNotificationMapsToUnrollRequest(
	t *testing.T) {

	t.Parallel()

	mock := &mockUnrollerRef{}
	adapter := newChainResolverAdapter(mock)

	outpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("test-vtxo-tx")),
		Index: 2,
	}

	notification := vtxo.ExpiringNotification{
		VTXO: &vtxo.Descriptor{
			Outpoint: outpoint,
		},
		BlocksRemaining: 10,
		Reason:          "critically close to expiry",
	}

	err := adapter.Tell(t.Context(), notification)
	require.NoError(t, err)

	// Verify the unroller received exactly one message.
	require.Equal(t, 1, mock.calls)

	// Verify the message is an UnrollRequest with the correct
	// outpoint.
	unrollReq, ok := mock.lastMsg.(*unroller.UnrollRequest)
	require.True(t, ok, "expected *UnrollRequest, got %T",
		mock.lastMsg)
	require.Len(t, unrollReq.TargetVTXOs, 1)
	require.Equal(t, outpoint, unrollReq.TargetVTXOs[0])
}

// TestChainResolverTellPropagatesError verifies that errors from
// the unroller ref are propagated back to the caller.
func TestChainResolverTellPropagatesError(t *testing.T) {
	t.Parallel()

	mock := &mockUnrollerRef{
		tellErr: actor.ErrActorTerminated,
	}
	adapter := newChainResolverAdapter(mock)

	notification := vtxo.ExpiringNotification{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{0x01},
				Index: 0,
			},
		},
	}

	err := adapter.Tell(t.Context(), notification)
	require.ErrorIs(t, err, actor.ErrActorTerminated)
}

// TestChainResolverImplementsTellOnlyRef verifies the compile-time
// interface assertion in chain_resolver.go is correct by exercising
// the adapter through the TellOnlyRef interface.
func TestChainResolverImplementsTellOnlyRef(t *testing.T) {
	t.Parallel()

	mock := &mockUnrollerRef{}
	adapter := newChainResolverAdapter(mock)

	// Use the adapter through the interface type.
	var ref actor.TellOnlyRef[vtxo.ExpiringNotification] = adapter

	require.Equal(t, "chain-resolver-adapter", ref.ID())

	err := ref.Tell(t.Context(), vtxo.ExpiringNotification{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{0xab},
				Index: 7,
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, mock.calls)

	unrollReq, ok := mock.lastMsg.(*unroller.UnrollRequest)
	require.True(t, ok)
	require.Equal(t, uint32(7), unrollReq.TargetVTXOs[0].Index)
}
