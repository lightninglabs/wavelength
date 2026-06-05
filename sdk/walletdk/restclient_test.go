package walletdk

import (
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// TestConnectREST verifies walletdk can use grpc-gateway clients without a
// native gRPC connection.
func TestConnectREST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1/daemon/get-info", r.URL.Path)

			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{
				"network": "regtest",
				"wallet_state": "WALLET_STATE_READY"
			}`))
			require.NoError(t, err)
		},
	))
	defer server.Close()

	client, err := Connect(t.Context(), ConnectConfig{
		Address:   server.URL,
		Transport: TransportREST,
	})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, client.Close())
	}()

	require.Nil(t, client.GRPCConn())

	info, err := client.GetInfo(t.Context())
	require.NoError(t, err)
	require.Equal(t, "regtest", info.Network)
	require.Equal(t, WalletStateReady, info.WalletState)
}

// stubDaemonServer is a minimal DaemonServiceServer that records which
// wallet-bootstrap RPCs OpenWalletFromPasskey drives for a given wallet state.
type stubDaemonServer struct {
	daemonrpc.UnimplementedDaemonServiceServer

	state        daemonrpc.WalletState
	identity     string
	initCalled   bool
	unlockCalled bool
}

// GetInfo reports the configured wallet state so the SDK can branch between
// import and unlock.
func (s *stubDaemonServer) GetInfo(context.Context, *daemonrpc.GetInfoRequest) (
	*daemonrpc.GetInfoResponse, error) {

	return &daemonrpc.GetInfoResponse{
		WalletState:    s.state,
		IdentityPubkey: s.identity,
	}, nil
}

// InitWallet records that the import branch ran and returns a fixed identity.
func (s *stubDaemonServer) InitWallet(context.Context,
	*daemonrpc.InitWalletRequest) (*daemonrpc.InitWalletResponse, error) {

	s.initCalled = true

	return &daemonrpc.InitWalletResponse{IdentityPubkey: "init-id"}, nil
}

// UnlockWallet records that the unlock branch ran and returns a fixed identity.
func (s *stubDaemonServer) UnlockWallet(context.Context,
	*daemonrpc.UnlockWalletRequest) (*daemonrpc.UnlockWalletResponse,
	error) {

	s.unlockCalled = true

	return &daemonrpc.UnlockWalletResponse{IdentityPubkey: "unlock-id"}, nil
}

// newStubClient wires a walletdk Client to an in-process stub daemon over a
// private bufconn gRPC transport, mirroring the embedded build's wiring.
func newStubClient(t *testing.T, stub *stubDaemonServer) *Client {
	t.Helper()

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	daemonrpc.RegisterDaemonServiceServer(server, stub)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (
			net.Conn, error) {

			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return newClient(
		conn, true, closedWaitChan(),
		func(context.Context) error { return conn.Close() },
	)
}

// TestDeriveSeedAndPasswordDeterministic checks that the same PRF output always
// yields the same entropy and DB password, and different PRF output does not.
func TestDeriveSeedAndPasswordDeterministic(t *testing.T) {
	t.Parallel()

	prfA := []byte("00000000000000000000000000000000")
	prfB := []byte("11111111111111111111111111111111")

	e1, p1 := deriveSeedAndPassword(prfA)
	e2, p2 := deriveSeedAndPassword(prfA)
	e3, p3 := deriveSeedAndPassword(prfB)

	require.Equal(t, e1, e2, "entropy must be stable for one PRF output")
	require.Equal(t, p1, p2, "password must be stable for one PRF output")
	require.NotEqual(
		t, e1, e3, "entropy must differ for different PRF output",
	)
	require.NotEqual(
		t, p1, p3, "password must differ for different PRF output",
	)

	require.Len(t, p1, 64)
	_, err := hex.DecodeString(string(p1))
	require.NoError(t, err)

	// Confirm the two HKDF info labels actually produce independent
	// outputs: the password's leading bytes must not equal the entropy.
	rawPW, err := hex.DecodeString(string(p1))
	require.NoError(t, err)
	require.NotEqual(
		t, e1[:], rawPW[:len(e1)],
		"entropy and password must be domain-separated",
	)
}

// TestEntropyToMnemonicRoundTrip checks the derived aezeed deciphers back to
// the exact entropy it was built from, so the same entropy always reaches the
// same HD wallet even though the 24-word string varies per call (random salt).
func TestEntropyToMnemonicRoundTrip(t *testing.T) {
	t.Parallel()

	var entropy [aezeed.EntropySize]byte
	copy(entropy[:], "walletdk-fixed-e") // exactly 16 bytes

	mnemonic, err := entropyToMnemonic(entropy)
	require.NoError(t, err)
	require.Len(t, mnemonic, aezeed.NumMnemonicWords)

	cipherSeed, err := mnemonic.ToCipherSeed(nil)
	require.NoError(t, err)
	require.Equal(t, entropy, cipherSeed.Entropy)
}

// TestEntropyToMnemonicVariesPerCall confirms the documented behavior: the same
// entropy yields different 24-word strings across calls (random KDF salt), yet
// every string deciphers back to the same entropy and thus the same wallet.
func TestEntropyToMnemonicVariesPerCall(t *testing.T) {
	t.Parallel()

	var entropy [aezeed.EntropySize]byte
	copy(entropy[:], "walletdk-fixed-e")

	m1, err := entropyToMnemonic(entropy)
	require.NoError(t, err)
	m2, err := entropyToMnemonic(entropy)
	require.NoError(t, err)

	require.NotEqual(t, m1, m2, "mnemonic must vary due to random KDF salt")

	cs1, err := m1.ToCipherSeed(nil)
	require.NoError(t, err)
	cs2, err := m2.ToCipherSeed(nil)
	require.NoError(t, err)
	require.Equal(t, entropy, cs1.Entropy)
	require.Equal(t, entropy, cs2.Entropy)
}

// TestOpenWalletFromPasskeyImportsWhenNone verifies a fresh device (no wallet)
// triggers a deterministic InitWallet import.
func TestOpenWalletFromPasskeyImportsWhenNone(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:    daemonrpc.WalletState_WALLET_STATE_NONE,
		identity: "init-id",
	}
	client := newStubClient(t, stub)

	res, err := client.OpenWalletFromPasskey(
		context.Background(), []byte("prf-output-bytes-aaaa"),
	)
	require.NoError(t, err)
	require.True(t, stub.initCalled)
	require.False(t, stub.unlockCalled)
	require.True(t, res.Imported)
	require.Len(t, res.Mnemonic, 24)
	require.Equal(t, "init-id", res.IdentityPubKey)
}

// TestOpenWalletFromPasskeyUnlocksWhenLocked verifies an existing local wallet
// is unlocked, not re-created.
func TestOpenWalletFromPasskeyUnlocksWhenLocked(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:    daemonrpc.WalletState_WALLET_STATE_LOCKED,
		identity: "unlock-id",
	}
	client := newStubClient(t, stub)

	res, err := client.OpenWalletFromPasskey(
		context.Background(), []byte("prf-output-bytes-aaaa"),
	)
	require.NoError(t, err)
	require.True(t, stub.unlockCalled)
	require.False(t, stub.initCalled)
	require.False(t, res.Imported)
	require.Equal(t, "unlock-id", res.IdentityPubKey)
}

// TestOpenWalletFromPasskeyNoOpWhenReady verifies an already-unlocked wallet is
// returned as-is without re-running init or unlock.
func TestOpenWalletFromPasskeyNoOpWhenReady(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:    daemonrpc.WalletState_WALLET_STATE_READY,
		identity: "ready-id",
	}
	client := newStubClient(t, stub)

	res, err := client.OpenWalletFromPasskey(
		context.Background(), []byte("prf-output-bytes-aaaa"),
	)
	require.NoError(t, err)
	require.False(t, stub.initCalled)
	require.False(t, stub.unlockCalled)
	require.False(t, res.Imported)
	require.Equal(t, "ready-id", res.IdentityPubKey)
}
