//go:build !test_postgres

package waved

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
