package darepod

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/indexer"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestGetIndexedVTXOByPkScriptTreatsUnregisteredScriptAsNotFound verifies
// that unregistered scripts are mapped to an empty VTXO lookup result.
func TestGetIndexedVTXOByPkScriptTreatsUnregisteredScriptAsNotFound(
	t *testing.T) {

	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := append(
		[]byte{0x51, 0x20},
		privKey.PubKey().SerializeCompressed()[1:]...,
	)

	walletReady := make(chan struct{})
	close(walletReady)

	unregisteredErr := fmt.Errorf("lookup failed: %w",
		indexer.ErrScriptNotRegisteredForPrincipal)
	rpcServer := NewRPCServer(&Server{
		walletReady: walletReady,
		indexer: indexer.New(
			&failingIndexerRPCClient{
				err: unregisteredErr,
			},
			&indexer.PrivKeySchnorrSigner{
				Key: privKey,
			},
			"arkd", "client:test", fn.None[btclog.Logger](),
		),
	})

	resp, err := rpcServer.GetIndexedVTXOByPkScript(
		t.Context(), &daemonrpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: pkScript,
		},
	)
	require.NoError(t, err)
	require.Nil(t, resp.GetVtxo())
}

type failingIndexerRPCClient struct {
	err error
}

// SendRPC returns a static correlation id for the generated mailbox client.
func (f *failingIndexerRPCClient) SendRPC(_ context.Context,
	_ mailboxrpc.ServiceMethod, _ proto.Message, _ mailboxrpc.RPCOptions) (
	mailboxrpc.SendResult, error) {

	return mailboxrpc.SendResult{
		CorrelationID: "corr-1",
	}, nil
}

// AwaitRPC returns the configured error to simulate an indexer failure.
func (f *failingIndexerRPCClient) AwaitRPC(_ context.Context, _ string,
	_ proto.Message) error {

	return f.err
}
