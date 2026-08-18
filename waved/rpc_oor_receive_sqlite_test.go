//go:build !test_postgres

package waved

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testReceiveScriptProofBackend provides deterministic receive keys and proof
// signers without starting a wallet backend.
type testReceiveScriptProofBackend struct {
	nextKey *keychain.KeyDescriptor
}

// DeriveKey returns the configured wallet key.
func (b *testReceiveScriptProofBackend) DeriveKey(_ context.Context,
	_ keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	return b.nextKey, nil
}

// DeriveNextKey returns the configured fresh receive key.
func (b *testReceiveScriptProofBackend) DeriveNextKey(_ context.Context,
	_ keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	return b.nextKey, nil
}

// ProofSigner returns a deterministic signer for the requested wallet key.
func (b *testReceiveScriptProofBackend) ProofSigner(
	keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {

	return &testOwnedReceiveScriptSigner{
		keyDesc: keyDesc,
		tagSig:  []byte("test-tag-signature"),
	}
}

// TestNewReceiveScriptCompletedReplayUsesDurableResult verifies exact replay
// returns a completed allocation without current operator terms or an indexer.
func TestNewReceiveScriptCompletedReplayUsesDurableResult(t *testing.T) {
	t.Parallel()

	walletReady := make(chan struct{})
	close(walletReady)
	now := time.Unix(1_700_000_000, 0)
	sqliteDB := db.NewTestDB(t)
	rpcServer := NewRPCServer(&Server{
		walletReady: walletReady,
		clk:         clock.NewTestClock(now),
		db:          sqliteDB,
	})

	store, err := rpcServer.newOORReceiveScriptStore()
	require.NoError(t, err)
	clientKey := testKeyDescriptor(t, 101)
	operatorKey := testKeyDescriptor(t, 102)
	pkScript, err := BuildPubKeyVTXOReceiveScript(
		clientKey.PubKey, operatorKey.PubKey, 144,
	)
	require.NoError(t, err)
	candidate := db.OwnedReceiveScriptRecord{
		PkScript:              pkScript,
		ClientKey:             clientKey,
		OperatorPubKey:        operatorKey.PubKey,
		ExitDelay:             144,
		Source:                db.OwnedReceiveScriptSourceWallet,
		CreatedAt:             now,
		LastUsedAt:            fn.None[time.Time](),
		IdempotencyKey:        "completed-allocation",
		RegistrationLabel:     "durable-label",
		RegistrationExpiresAt: now.Add(time.Hour),
		RegistrationRPCKey:    "stable-remote-key",
	}
	_, created, err := store.AdmitIdempotentOwnedReceiveScript(
		t.Context(), candidate,
	)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(
		t,
		store.MarkOwnedReceiveScriptRegistered(
			t.Context(), candidate.IdempotencyKey,
			candidate.RegistrationRPCKey,
		),
	)

	resp, err := rpcServer.NewReceiveScript(t.Context(),
		&waverpc.NewReceiveScriptRequest{
			Label:          candidate.RegistrationLabel,
			IdempotencyKey: candidate.IdempotencyKey,
		},
	)
	require.NoError(t, err)
	require.Equal(t, candidate.RegistrationLabel, resp.Label)
	require.Equal(
		t,
		uint64(
			candidate.RegistrationExpiresAt.Unix(),
		),
		resp.ExpiresAtUnixS,
	)

	_, err = rpcServer.NewReceiveScript(t.Context(),
		&waverpc.NewReceiveScriptRequest{
			Label:          "changed-label",
			IdempotencyKey: candidate.IdempotencyKey,
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestNewReceiveScriptRetryPreservesPendingAllocation verifies the public RPC
// persists one allocation before registration and resumes its exact remote
// request after an ambiguous first response.
func TestNewReceiveScriptRetryPreservesPendingAllocation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	walletReady := make(chan struct{})
	close(walletReady)
	receiveKey := testKeyDescriptor(t, 93)
	operatorKey := testKeyDescriptor(t, 94)
	rpcClient := &testReceiveScriptRPCClient{
		awaitErrors: []error{
			errors.New("ambiguous indexer response"),
		},
	}
	idx := indexer.New(
		rpcClient, nil, "test-server", "client:test",
		fn.None[btclog.Logger](),
	)
	server := &Server{
		walletReady: walletReady,
		clk:         clock.NewTestClock(now),
		db:          db.NewTestDB(t),
		indexer:     idx,
		proofKeyBackend: &testReceiveScriptProofBackend{
			nextKey: &receiveKey,
		},
		serverConn: newBufconnClient(t, &fakeArkService{
			getInfoResponse: &arkrpc.GetInfoResponse{
				Pubkey:        operatorKey.PubKey.SerializeCompressed(),
				VtxoExitDelay: 144,
			},
		}),
	}
	rpcServer := NewRPCServer(server)
	req := &waverpc.NewReceiveScriptRequest{
		Label:          "credit top-up",
		IdempotencyKey: "consumer:operation-1",
	}

	_, err := rpcServer.NewReceiveScript(t.Context(), req)
	require.Equal(t, codes.Internal, status.Code(err))
	store, err := rpcServer.newOORReceiveScriptStore()
	require.NoError(t, err)
	pending, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		t.Context(), req.IdempotencyKey,
	)
	require.NoError(t, err)
	require.True(t, pending.RegistrationCompletedAt.IsNone())

	resp, err := rpcServer.NewReceiveScript(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(pending.PkScript), resp.PkScriptHex)
	require.Equal(
		t,
		uint64(
			pending.RegistrationExpiresAt.Unix(),
		),
		resp.ExpiresAtUnixS,
	)
	require.Len(t, rpcClient.registerOpts, 2)
	require.Equal(
		t, pending.RegistrationRPCKey,
		rpcClient.registerOpts[0].IdempotencyKey,
	)
	require.Equal(
		t, rpcClient.registerOpts[0].IdempotencyKey,
		rpcClient.registerOpts[1].IdempotencyKey,
	)
	completed, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		t.Context(), req.IdempotencyKey,
	)
	require.NoError(t, err)
	require.True(t, completed.RegistrationCompletedAt.IsSome())
}

// TestNewReceiveScriptRenewsExpiredRegistration verifies completed replay
// persists a fresh registration window and remote request key before
// re-registering the exact same owned script.
func TestNewReceiveScriptRenewsExpiredRegistration(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	clk := clock.NewTestClock(now)
	walletReady := make(chan struct{})
	close(walletReady)
	clientKey := testKeyDescriptor(t, 95)
	operatorKey := testKeyDescriptor(t, 96)
	rpcClient := &testReceiveScriptRPCClient{}
	server := &Server{
		walletReady: walletReady,
		clk:         clk,
		log:         btclog.Disabled,
		db:          db.NewTestDB(t),
		indexer: indexer.New(
			rpcClient, nil, "test-server", "client:test",
			fn.None[btclog.Logger](),
		),
		proofKeyBackend: &testReceiveScriptProofBackend{
			nextKey: &clientKey,
		},
	}
	rpcServer := NewRPCServer(server)
	store, err := rpcServer.newOORReceiveScriptStore()
	require.NoError(t, err)
	pkScript, err := BuildPubKeyVTXOReceiveScript(
		clientKey.PubKey, operatorKey.PubKey, 144,
	)
	require.NoError(t, err)
	candidate := db.OwnedReceiveScriptRecord{
		PkScript:              pkScript,
		ClientKey:             clientKey,
		OperatorPubKey:        operatorKey.PubKey,
		ExitDelay:             144,
		Source:                db.OwnedReceiveScriptSourceWallet,
		CreatedAt:             now,
		LastUsedAt:            fn.None[time.Time](),
		IdempotencyKey:        "consumer:operation-expired",
		RegistrationLabel:     "credit top-up",
		RegistrationExpiresAt: now.Add(time.Second),
		RegistrationRPCKey:    "expired-remote-key",
	}
	_, created, err := store.AdmitIdempotentOwnedReceiveScript(
		t.Context(), candidate,
	)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(
		t,
		store.MarkOwnedReceiveScriptRegistered(
			t.Context(), candidate.IdempotencyKey,
			candidate.RegistrationRPCKey,
		),
	)
	clk.SetTime(now.Add(2 * time.Second))

	resp, err := rpcServer.NewReceiveScript(t.Context(),
		&waverpc.NewReceiveScriptRequest{
			Label:          candidate.RegistrationLabel,
			IdempotencyKey: candidate.IdempotencyKey,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, hex.EncodeToString(candidate.PkScript), resp.PkScriptHex,
	)
	require.Greater(
		t, resp.ExpiresAtUnixS,
		uint64(
			candidate.RegistrationExpiresAt.Unix(),
		),
	)
	require.Len(t, rpcClient.registerOpts, 1)
	require.NotEqual(
		t, candidate.RegistrationRPCKey,
		rpcClient.registerOpts[0].IdempotencyKey,
	)
	renewed, err := store.LookupOwnedReceiveScriptByIdempotencyKey(
		t.Context(), candidate.IdempotencyKey,
	)
	require.NoError(t, err)
	require.Equal(t, candidate.PkScript, renewed.PkScript)
	require.Equal(
		t, rpcClient.registerOpts[0].IdempotencyKey,
		renewed.RegistrationRPCKey,
	)
	require.True(t, renewed.RegistrationCompletedAt.IsSome())
}
