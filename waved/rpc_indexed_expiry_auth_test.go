package waved

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestIndexedExpiryRPCUsesConfirmedDeadline exercises both indexer-facing RPC
// reads. A valid output with a false indexer deadline must expose only the
// deadline derived from its confirmed commitment.
func TestIndexedExpiryRPCUsesConfirmedDeadline(t *testing.T) {
	t.Parallel()

	key := testKeyDescriptor(t, 1)
	operator := testKeyDescriptor(t, 3)
	policy, script, err := arkscript.EncodeStandardVTXOArtifacts(
		key.PubKey, operator.PubKey, 144,
	)
	require.NoError(t, err)
	indexed, commitment := expiryfixture.Round(
		t, 10_000, script, 50, 100, 1,
	)
	indexed.BatchExpiryHeight = 1_000
	indexed.RelativeExpiry = 24
	stub := &recoveryIndexerStub{
		byScript: map[string]*arkrpc.VTXO{
			string(script): indexed,
		},
	}
	ready := make(chan struct{})
	close(ready)
	var authenticateCalls atomic.Int32
	var chainUnavailable bool
	server := &Server{
		walletReady:         ready,
		walletServicesReady: make(chan struct{}),
		log:                 btclog.Disabled,
		clientKeyDesc:       testKeyDescriptor(t, 4),
		chainBackend: &heightOnlyChainBackend{
			height: 100,
		},
		indexer: indexer.New(
			stub, nil, "test-server", "client:test",
			fn.None[btclog.Logger](),
		).WithSigner(&testOwnedReceiveScriptSigner{
			keyDesc: key,
			tagSig:  []byte("test-tag-signature"),
		}),
		expiryAuthenticator: func(ctx context.Context,
			ancestry []vtxo.Ancestry) (
			vtxo.AuthenticatedBatchEvidence, error) {

			authenticateCalls.Add(1)
			if chainUnavailable {
				return vtxo.AuthenticatedBatchEvidence{},
					errors.New("chain unavailable")
			}

			return vtxo.AuthenticateBatchEvidence(
				ctx, ancestry,
				func(_ context.Context, txid chainhash.Hash,
					_ []byte, _ uint32) (
					vtxo.CommitmentConfirmation, error) {

					var c vtxo.CommitmentConfirmation
					if commitment.TxHash() != txid {
						return c, fmt.Errorf(
							"unknown commitment %s",
							txid)
					}

					c.Tx = commitment
					c.BlockHeight = 100

					return c, nil
				},
			)
		},
	}
	authenticate := server.expiryAuthenticator
	server.expiryAuthenticator = nil
	rpc := NewRPCServer(server)
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	waverpc.RegisterDaemonServiceServer(grpcServer, rpc)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(grpcServer.Stop)
	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn,
			error) {

			return listener.Dial()
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := waverpc.NewDaemonServiceClient(conn)

	lookup := func() error {
		response, err := client.GetIndexedVTXOByPkScript(
			t.Context(), &waverpc.GetIndexedVTXOByPkScriptRequest{
				PkScript: script,
				StatusFilter: []waverpc.VTXOStatus{
					waverpc.VTXOStatus_VTXO_STATUS_LIVE,
				},
			},
		)
		if err != nil {
			return err
		}
		require.Equal(
			t, int32(150), response.GetVtxo().GetBatchExpiry(),
		)
		require.Equal(
			t, int32(150), response.GetVtxo().
				GetExpiryInfo().GetBatchExpiry(),
		)
		require.Zero(t, response.GetVtxo().GetRelativeExpiry())
		require.Equal(
			t, testExpiryStatusUnknown,
			response.GetVtxo().GetExpiryInfo().GetStatus(),
		)

		return nil
	}
	expiry := func() error {
		target := &waverpc.GetVTXOExpiryInfoRequest_PkScript{
			PkScript: script,
		}
		response, err := client.GetVTXOExpiryInfo(
			t.Context(), &waverpc.GetVTXOExpiryInfoRequest{
				Target: target,
			},
		)
		if err != nil {
			return err
		}
		require.Equal(
			t, int32(150), response.GetExpiryInfo().
				GetBatchExpiry(),
		)
		require.Zero(t, response.GetExpiryInfo().GetRelativeExpiry())
		require.Equal(
			t, testExpiryStatusUnknown,
			response.GetExpiryInfo().GetStatus(),
		)

		return nil
	}
	// Wallet unlock alone does not publish the indexer/actor wiring.
	require.Equal(t, codes.Unavailable, status.Code(lookup()))
	require.Equal(t, codes.Unavailable, status.Code(expiry()))
	server.expiryAuthenticator = authenticate
	server.markWalletServicesReady(nil)
	require.NoError(t, lookup())
	require.NoError(t, expiry())

	// A ready service with broken wiring must not disguise that defect as
	// an ordinary confirmation miss.
	brokenRPC := NewRPCServer(&Server{
		walletReady: ready, walletServicesReady: ready,
		indexer: server.indexer, log: btclog.Disabled,
	})
	_, err = brokenRPC.GetIndexedVTXOByPkScript(
		t.Context(), &waverpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: script,
		},
	)
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, "authenticator not initialized")

	policyReq := &waverpc.GetVTXOExpiryInfoRequest{
		Target: &waverpc.GetVTXOExpiryInfoRequest_PkScript{
			PkScript: script,
		},
		PolicyTemplate: policy,
		CurrentHeight:  100,
	}
	classified, err := client.GetVTXOExpiryInfo(t.Context(), policyReq)
	require.NoError(t, err)
	require.True(t, classified.PolicyAuthorized)
	require.Equal(t, uint32(144),
		classified.ExpiryInfo.RelativeExpiry)
	require.Equal(
		t, waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_CRITICAL,
		classified.ExpiryInfo.Status,
	)
	policyReq.CurrentHeight = 250
	classified, err = client.GetVTXOExpiryInfo(t.Context(), policyReq)
	require.NoError(t, err)
	require.Equal(
		t, waverpc.VTXOExpiryStatus_VTXO_EXPIRY_STATUS_EXPIRED,
		classified.ExpiryInfo.Status,
	)
	policyReq.CurrentHeight = 100

	indexed.AncestryPaths[0].CommitmentHeight = 0
	indexed.Status = arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED
	policyReq.StatusFilter = []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
	}
	before := authenticateCalls.Load()
	pending, err := client.GetIndexedVTXOByPkScript(
		t.Context(), &waverpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: script,
		},
	)
	require.NoError(t, err)
	require.NotNil(t, pending.Vtxo)
	require.Zero(t, pending.Vtxo.BatchExpiry)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND,
		pending.Vtxo.Status,
	)
	require.Zero(t, pending.Vtxo.ExpiryInfo.BlocksRemaining)
	require.Equal(
		t, testExpiryStatusUnknown, pending.Vtxo.ExpiryInfo.Status,
	)
	classified, err = client.GetVTXOExpiryInfo(t.Context(), policyReq)
	require.NoError(t, err)
	require.True(t, classified.Found)
	require.Zero(t, classified.ExpiryInfo.BatchExpiry)
	require.Equal(t, testExpiryStatusUnknown,
		classified.ExpiryInfo.Status)
	require.Equal(t, before, authenticateCalls.Load())

	indexed.Status = arkrpc.VTXOStatus_VTXO_STATUS_LIVE
	policyReq.StatusFilter = nil
	// A missing legacy height hint must not suppress chain evidence.
	require.NoError(t, lookup())
	require.NoError(t, expiry())
	indexed.AncestryPaths[0].CommitmentHeight = 100
	chainUnavailable = true
	classified, err = client.GetVTXOExpiryInfo(t.Context(), policyReq)
	require.NoError(t, err)
	require.True(t, classified.Found)
	require.Zero(t, classified.ExpiryInfo.BatchExpiry)
	chainUnavailable = false
	require.NoError(t, lookup())
	require.NoError(t, expiry())

	for _, terminal := range []waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_SPENT,
		waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	} {
		indexed.Status, err = daemonStatusToIndexerStatus(terminal)
		require.NoError(t, err)
		indexed.SpentByTxid = make([]byte, chainhash.HashSize)
		indexed.SpentByTxid[0] = 1
		chainUnavailable = true
		terminalResp, err := client.GetIndexedVTXOByPkScript(
			t.Context(), &waverpc.GetIndexedVTXOByPkScriptRequest{
				PkScript:     script,
				StatusFilter: []waverpc.VTXOStatus{terminal},
			},
		)
		require.NoError(t, err)
		require.Equal(t, terminal, terminalResp.Vtxo.Status)
		require.NotEmpty(t, terminalResp.Vtxo.SpentByTxid)
		require.Zero(t, terminalResp.Vtxo.BatchExpiry)
	}
	indexed.Status = arkrpc.VTXOStatus_VTXO_STATUS_LIVE
	chainUnavailable = false

	delete(stub.byScript, string(script))
	classified, err = client.GetVTXOExpiryInfo(t.Context(), policyReq)
	require.NoError(t, err)
	require.False(t, classified.Found)
	require.True(t, classified.PolicyAuthorized)
	stub.byScript[string(script)] = indexed

	otherKey := testKeyDescriptor(t, 2)
	otherScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(otherKey.PubKey),
	)
	require.NoError(t, err)
	stub.byScript[string(otherScript)] = indexed
	policyReq.Target = &waverpc.GetVTXOExpiryInfoRequest_PkScript{
		PkScript: otherScript,
	}
	_, err = client.GetVTXOExpiryInfo(t.Context(), policyReq)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = client.GetIndexedVTXOByPkScript(
		t.Context(), &waverpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: otherScript,
		},
	)
	require.Equal(t, codes.DataLoss, status.Code(err))
	_, err = client.GetVTXOExpiryInfo(
		t.Context(), &waverpc.GetVTXOExpiryInfoRequest{
			Target: &waverpc.GetVTXOExpiryInfoRequest_PkScript{
				PkScript: otherScript,
			},
		},
	)
	require.Equal(t, codes.DataLoss, status.Code(err))

	indexed.AncestryPaths[0].TreeDepth = 0
	require.Equal(t, codes.DataLoss, status.Code(lookup()))
	require.Equal(t, codes.DataLoss, status.Code(expiry()))
}
