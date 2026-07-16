package wavewalletdk

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// stubDaemonServer is a minimal DaemonServiceServer that records which
// wallet-bootstrap RPCs OpenWalletFromPasskey drives for a given wallet state.
type stubDaemonServer struct {
	waverpc.UnimplementedDaemonServiceServer

	state         waverpc.WalletState
	identity      string
	freeWindow    uint32
	initErr       error
	unlockErr     error
	initCalled    bool
	unlockCalled  bool
	lastInitReq   *waverpc.InitWalletRequest
	lastUnlockReq *waverpc.UnlockWalletRequest
}

// GetInfo reports the configured wallet state so the SDK can branch between
// import and unlock.
func (s *stubDaemonServer) GetInfo(context.Context, *waverpc.GetInfoRequest) (
	*waverpc.GetInfoResponse, error) {

	return &waverpc.GetInfoResponse{
		WalletState:    s.state,
		IdentityPubkey: s.identity,
		ServerInfo: &waverpc.ServerInfo{
			FreeRefreshWindowBlocks: s.freeWindow,
		},
	}, nil
}

// TestGetInfoExposesFreeRefreshWindow verifies the wallet-shaped facade does
// not discard the operator hint used by automatic expiry refresh.
func TestGetInfoExposesFreeRefreshWindow(t *testing.T) {
	t.Parallel()

	client := newStubClient(t, &stubDaemonServer{
		state:      waverpc.WalletState_WALLET_STATE_READY,
		freeWindow: 120,
	})

	info, err := client.GetInfo(t.Context())
	require.NoError(t, err)
	require.NotNil(t, info.ServerInfo)
	require.Equal(
		t, uint32(120), info.ServerInfo.FreeRefreshWindowBlocks,
	)
}

// InitWallet records the import call and its request, then returns the
// configured error (if any) so error propagation can be exercised.
func (s *stubDaemonServer) InitWallet(_ context.Context,
	req *waverpc.InitWalletRequest) (*waverpc.InitWalletResponse, error) {

	s.initCalled = true
	s.lastInitReq = req
	if s.initErr != nil {
		return nil, s.initErr
	}

	return &waverpc.InitWalletResponse{IdentityPubkey: "init-id"}, nil
}

// UnlockWallet records the unlock call, then returns the configured error (if
// any) so unlock-failure propagation can be exercised.
func (s *stubDaemonServer) UnlockWallet(_ context.Context,
	req *waverpc.UnlockWalletRequest) (*waverpc.UnlockWalletResponse,
	error) {

	s.unlockCalled = true
	s.lastUnlockReq = req
	if s.unlockErr != nil {
		return nil, s.unlockErr
	}

	return &waverpc.UnlockWalletResponse{IdentityPubkey: "unlock-id"}, nil
}

// newStubClient wires a wavewalletdk Client to an in-process stub daemon over a
// private bufconn gRPC transport, mirroring the embedded build's wiring.
func newStubClient(t *testing.T, stub *stubDaemonServer) *Client {
	t.Helper()

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	waverpc.RegisterDaemonServiceServer(server, stub)
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

// TestDeriveSeedAndPasswordGoldenVector pins the HKDF derivation, including the
// hkdfSeedInfo/hkdfDBKeyInfo info labels, to fixed output. Those labels are
// wire-format constants: silently editing one changes every derived seed and
// bricks existing passkey wallets, so any such change must fail here loudly.
func TestDeriveSeedAndPasswordGoldenVector(t *testing.T) {
	t.Parallel()

	prf := make([]byte, 32)
	for i := range prf {
		prf[i] = byte(i)
	}

	entropy, dbPassword := deriveSeedAndPassword(prf)

	require.Equal(
		t, "66309e5d3b5083a0f1c32aceb2244468",
		hex.EncodeToString(entropy[:]),
	)
	require.Equal(
		t, "477c5bd959ac98cc9c9d1cb43c6750dd3c3733bd6085597934d958db"+
			"f64adbb2", string(dbPassword),
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
		state:    waverpc.WalletState_WALLET_STATE_NONE,
		identity: "init-id",
	}
	client := newStubClient(t, stub)

	prf := []byte("prf-output-bytes-0123456789abcdef")
	res, err := client.OpenWalletFromPasskey(context.Background(), prf)
	require.NoError(t, err)
	require.True(t, stub.initCalled)
	require.False(t, stub.unlockCalled)
	require.True(t, res.Imported)
	require.Len(t, res.Mnemonic, 24)
	require.Equal(t, "init-id", res.IdentityPubKey)

	// The derived DB password must reach InitWallet so a fresh device and a
	// re-importing device produce the same locally-encrypted wallet.
	_, wantDBPassword := deriveSeedAndPassword(prf)
	require.Equal(t, wantDBPassword, stub.lastInitReq.WalletPassword)
}

// TestOpenWalletFromPasskeyUnlocksWhenLocked verifies an existing local wallet
// is unlocked, not re-created.
func TestOpenWalletFromPasskeyUnlocksWhenLocked(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:    waverpc.WalletState_WALLET_STATE_LOCKED,
		identity: "unlock-id",
	}
	client := newStubClient(t, stub)

	prf := []byte("prf-output-bytes-0123456789abcdef")
	res, err := client.OpenWalletFromPasskey(context.Background(), prf)
	require.NoError(t, err)
	require.True(t, stub.unlockCalled)
	require.False(t, stub.initCalled)
	require.False(t, res.Imported)
	require.Equal(t, "unlock-id", res.IdentityPubKey)

	// The same derived DB password must drive unlock, so the device that
	// imported the wallet can later unlock it from the same passkey.
	_, wantDBPassword := deriveSeedAndPassword(prf)
	require.Equal(t, wantDBPassword, stub.lastUnlockReq.WalletPassword)
}

// TestOpenWalletFromPasskeyErrorsWhenAlreadyUnlocked verifies that opening a
// passkey wallet when one is already unlocked returns an error and issues no
// import or unlock RPC, for both the ready and still-syncing states.
func TestOpenWalletFromPasskeyErrorsWhenAlreadyUnlocked(t *testing.T) {
	t.Parallel()

	states := []waverpc.WalletState{
		waverpc.WalletState_WALLET_STATE_READY,
		waverpc.WalletState_WALLET_STATE_SYNCING,
	}
	for _, state := range states {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()

			stub := &stubDaemonServer{
				state:    state,
				identity: "open-id",
			}
			client := newStubClient(t, stub)

			_, err := client.OpenWalletFromPasskey(
				context.Background(),
				[]byte("prf-output-bytes-0123456789abcdef"),
			)
			require.Error(t, err)
			require.False(t, stub.initCalled)
			require.False(t, stub.unlockCalled)
		})
	}
}

// TestOpenWalletFromPasskeyErrorsOnUnexpectedState verifies an unspecified
// wallet state returns an error and issues no import or unlock RPC.
func TestOpenWalletFromPasskeyErrorsOnUnexpectedState(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state: waverpc.WalletState_WALLET_STATE_UNSPECIFIED,
	}
	client := newStubClient(t, stub)

	_, err := client.OpenWalletFromPasskey(
		context.Background(),
		[]byte("prf-output-bytes-0123456789abcdef"),
	)
	require.Error(t, err)
	require.False(t, stub.initCalled)
	require.False(t, stub.unlockCalled)
}

// TestOpenWalletFromPasskeyRejectsShortPRF verifies a too-short PRF output is
// rejected before any derivation or wallet bootstrap runs, so a caller bug or
// empty input cannot import a fixed, publicly-known seed. 31 bytes is the
// boundary just below the 32-byte minimum.
func TestOpenWalletFromPasskeyRejectsShortPRF(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 9, 31} {
		t.Run(fmt.Sprintf("len-%d", n), func(t *testing.T) {
			t.Parallel()

			stub := &stubDaemonServer{
				state: waverpc.WalletState_WALLET_STATE_NONE,
			}
			client := newStubClient(t, stub)

			_, err := client.OpenWalletFromPasskey(
				context.Background(), make([]byte, n),
			)
			require.Error(t, err)
			require.False(t, stub.initCalled)
			require.False(t, stub.unlockCalled)
		})
	}
}

// TestOpenWalletFromPasskeyAcceptsMinimumLength verifies the exact 32-byte
// boundary is accepted and drives a normal import.
func TestOpenWalletFromPasskeyAcceptsMinimumLength(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:    waverpc.WalletState_WALLET_STATE_NONE,
		identity: "init-id",
	}
	client := newStubClient(t, stub)

	res, err := client.OpenWalletFromPasskey(
		context.Background(), make([]byte, 32),
	)
	require.NoError(t, err)
	require.True(t, stub.initCalled)
	require.True(t, res.Imported)
}

// TestOpenWalletFromPasskeyImportUsesEmptySeedPassphrase pins the load-bearing
// reproducibility invariant: the import path must initialize the wallet with an
// empty seed passphrase so the seed depends only on the derived entropy.
func TestOpenWalletFromPasskeyImportUsesEmptySeedPassphrase(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:    waverpc.WalletState_WALLET_STATE_NONE,
		identity: "init-id",
	}
	client := newStubClient(t, stub)

	_, err := client.OpenWalletFromPasskey(
		context.Background(),
		[]byte("prf-output-bytes-0123456789abcdef"),
	)
	require.NoError(t, err)
	require.NotNil(t, stub.lastInitReq)
	require.Empty(t, stub.lastInitReq.SeedPassphrase)
}

// TestOpenWalletFromPasskeyPropagatesInitError verifies an InitWallet failure
// on the import path surfaces to the caller rather than being swallowed.
func TestOpenWalletFromPasskeyPropagatesInitError(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:   waverpc.WalletState_WALLET_STATE_NONE,
		initErr: errors.New("init boom"),
	}
	client := newStubClient(t, stub)

	_, err := client.OpenWalletFromPasskey(
		context.Background(),
		[]byte("prf-output-bytes-0123456789abcdef"),
	)
	require.Error(t, err)
	require.True(t, stub.initCalled)
}

// TestOpenWalletFromPasskeyPropagatesUnlockError verifies an UnlockWallet
// failure (e.g. a wrong passkey) on the unlock path surfaces to the caller.
func TestOpenWalletFromPasskeyPropagatesUnlockError(t *testing.T) {
	t.Parallel()

	stub := &stubDaemonServer{
		state:     waverpc.WalletState_WALLET_STATE_LOCKED,
		unlockErr: errors.New("unlock boom"),
	}
	client := newStubClient(t, stub)

	_, err := client.OpenWalletFromPasskey(
		context.Background(),
		[]byte("prf-output-bytes-0123456789abcdef"),
	)
	require.Error(t, err)
	require.True(t, stub.unlockCalled)
}
