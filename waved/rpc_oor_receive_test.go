package waved

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/waverpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testRegistrationCompleter records completion attempts and can inject one
// durable write error after the indexer acknowledges registration.
type testRegistrationCompleter struct {
	keys []string
	err  error
}

// MarkOwnedReceiveScriptRegistered records the durable completion
// identity and returns the configured error.
func (s *testRegistrationCompleter) MarkOwnedReceiveScriptRegistered(
	_ context.Context, idempotencyKey, registrationRPCKey string) error {

	s.keys = append(s.keys, idempotencyKey+":"+registrationRPCKey)

	return s.err
}

// TestRegisterIdempotentReceiveScriptReusesRemoteKey verifies an ambiguous
// completion-write failure retries the same logical indexer registration.
func TestRegisterIdempotentReceiveScriptReusesRemoteKey(t *testing.T) {
	t.Parallel()

	key := testKeyDescriptor(t, 91)
	operator := testKeyDescriptor(t, 92)
	pkScript, err := BuildPubKeyVTXOReceiveScript(
		key.PubKey, operator.PubKey, 144,
	)
	require.NoError(t, err)

	rpcClient := &testReceiveScriptRPCClient{}
	idx := indexer.New(
		rpcClient, nil, "test-server", "client:test",
		fn.None[btclog.Logger](),
	).WithSigner(&testOwnedReceiveScriptSigner{
		keyDesc: key,
		tagSig:  []byte("test-tag-signature"),
	})

	rec := &db.OwnedReceiveScriptRecord{
		PkScript:              pkScript,
		ClientKey:             key,
		OperatorPubKey:        operator.PubKey,
		ExitDelay:             144,
		Source:                db.OwnedReceiveScriptSourceWallet,
		IdempotencyKey:        "allocation-1",
		RegistrationLabel:     "retry-safe",
		RegistrationExpiresAt: time.Now().Add(time.Hour),
		RegistrationRPCKey:    "stable-remote-key",
	}

	markErr := errors.New("completion write failed")
	store := &testRegistrationCompleter{err: markErr}
	err = registerIdempotentReceiveScript(t.Context(), idx, store, rec)
	require.ErrorIs(t, err, markErr)

	store.err = nil
	err = registerIdempotentReceiveScript(t.Context(), idx, store, rec)
	require.NoError(t, err)

	require.Len(t, rpcClient.registerOpts, 2)
	require.Equal(
		t, "stable-remote-key",
		rpcClient.registerOpts[0].IdempotencyKey,
	)
	require.Equal(
		t, rpcClient.registerOpts[0].IdempotencyKey,
		rpcClient.registerOpts[1].IdempotencyKey,
	)
	require.Equal(t, []string{
		"allocation-1:stable-remote-key",
		"allocation-1:stable-remote-key",
	}, store.keys)
}

// TestRegisterIdempotentReceiveScriptCompletedReplay verifies completed replay
// returns without requiring an indexer client or completion store.
func TestRegisterIdempotentReceiveScriptCompletedReplay(t *testing.T) {
	t.Parallel()

	rec := &db.OwnedReceiveScriptRecord{
		RegistrationCompletedAt: fn.Some(time.Unix(100, 0)),
	}

	err := registerIdempotentReceiveScript(
		t.Context(), nil, nil, rec,
	)
	require.NoError(t, err)
}

// TestNewReceiveScriptRejectsOversizeIdempotencyKey verifies the durable text
// bound is enforced before database or indexer initialization is consulted.
func TestNewReceiveScriptRejectsOversizeIdempotencyKey(t *testing.T) {
	t.Parallel()

	walletReady := make(chan struct{})
	close(walletReady)
	rpcServer := NewRPCServer(&Server{walletReady: walletReady})
	keyBytes := db.MaxOwnedReceiveScriptIdempotencyKeyBytes + 1

	_, err := rpcServer.NewReceiveScript(t.Context(),
		&waverpc.NewReceiveScriptRequest{
			IdempotencyKey: string(make([]byte, keyBytes)),
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestAcquireReceiveScriptLockCleansRegistry verifies keyed lock entries do not
// accumulate after the last caller releases them.
func TestAcquireReceiveScriptLockCleansRegistry(t *testing.T) {
	t.Parallel()

	rpcServer := NewRPCServer(&Server{})
	release := rpcServer.acquireReceiveScriptLock("allocation-1")
	require.Len(t, rpcServer.receiveScriptLocks, 1)

	release()
	require.Empty(t, rpcServer.receiveScriptLocks)
}
