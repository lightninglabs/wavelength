package waved

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/virtualchannel"
	"github.com/stretchr/testify/require"
)

func TestVirtualChannelOperationContextUsesDaemonLifetime(t *testing.T) {
	runCtx, stopDaemon := context.WithCancel(context.Background())
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()

	server := &Server{runCtx: runCtx}
	operationCtx, cancelOperation := server.virtualChannelOperationContext(
		requestCtx, time.Minute,
	)
	defer cancelOperation()
	require.NoError(t, operationCtx.Err())

	stopDaemon()
	select {
	case <-operationCtx.Done():
		require.ErrorIs(t, operationCtx.Err(), context.Canceled)

	case <-time.After(time.Second):
		t.Fatal("operation context did not stop with daemon")
	}
}

func TestPromotedChannelPendingIDIsStable(t *testing.T) {
	req := &promoteVTXORequest{
		PeerNodePubkey: make([]byte, 33),
		CapacitySat:    50_000,
		IdempotencyKey: "promotion-1",
	}
	client := []byte("client-1")
	id := promotedChannelPendingID(req, client)
	require.Equal(t, id, promotedChannelPendingID(req, client))

	changed := *req
	changed.IdempotencyKey = "promotion-2"
	require.NotEqual(t, id, promotedChannelPendingID(&changed, client))
}

func TestReceiveChannelPendingIDIsStable(t *testing.T) {
	client := []byte("client-1")
	id := receiveChannelPendingID("request-1", client)
	require.Equal(t, id, receiveChannelPendingID("request-1", client))
	require.NotEqual(t, id, receiveChannelPendingID("request-2", client))
	require.NotEqual(
		t, id,
		receiveChannelPendingID(
			"request-1", []byte("client-2"),
		),
	)
}

func TestReceiveChannelRequestKey(t *testing.T) {
	provided, err := receiveChannelRequestKey(" request-1 ")
	require.NoError(t, err)
	require.Equal(t, "request-1", provided)

	generated, err := receiveChannelRequestKey("")
	require.NoError(t, err)
	require.Len(t, generated, 64)
}

func TestPromotedChannelRequestMatchesDurableBacking(t *testing.T) {
	outpoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("promoted-backing")), Index: 1,
	}
	req := &promoteVTXORequest{
		PeerNodePubkey: append([]byte{0x02}, make([]byte, 32)...),
		CapacitySat:    50_000,
	}
	var remote virtualchannel.NodePubKey
	copy(remote[:], req.PeerNodePubkey)
	backing := []virtualchannel.BackingVTXO{{
		OutPoint: outpoint,
		Amount:   btcutil.Amount(51_000),
	}}

	require.True(
		t, promotedChannelRequestMatches(
			req, remote, virtualchannel.RoleClient, 50_000, 50_000,
			0, backing,
		),
	)

	backing[0].Amount = 52_000
	require.False(
		t, promotedChannelRequestMatches(
			req, remote, virtualchannel.RoleClient, 50_000, 50_000,
			0, backing,
		),
	)
}
