package waved

import (
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// TestConnectOperatorClientsREST verifies the daemon can construct all
// operator-facing clients over the grpc-gateway transport.
func TestConnectOperatorClientsREST(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPubKey := operatorKey.PubKey().SerializeCompressed()
	tempDir := t.TempDir()
	macaroonPath := filepath.Join(tempDir, "operator.macaroon")
	newTestMacaroonService(
		t, macaroonPath, "lumosd",
		map[string][]bakery.Op{
			"/arkrpc.ArkService/GetInfo": {{
				Entity: "lumosd",
				Action: "client",
			}},
		},
	)

	macHex, err := rpcauth.HexFromFile(macaroonPath)
	require.NoError(t, err)

	// Record the mailbox auth header the REST edge sent, guarded because
	// the handler runs on the test server's goroutine.
	var (
		authSigMu  sync.Mutex
		gotAuthSig string
	)
	pulledAuthSig := func() string {
		authSigMu.Lock()
		defer authSigMu.Unlock()

		return gotAuthSig
	}

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				require.Equal(
					t, macHex, r.Header.Get(
						rpcauth.MacaroonMetadataKey,
					),
				)
				w.Header().Set(
					"Content-Type", "application/json",
				)

				var (
					msg        []byte
					marshalErr error
				)
				switch r.URL.Path {
				case "/v1/ark/get-info":
					msg, marshalErr = protojson.Marshal(
						&arkrpc.GetInfoResponse{
							Pubkey: operatorPubKey,
						},
					)

				case "/v1/mailbox/pull":
					authSigMu.Lock()
					gotAuthSig = r.Header.Get(
						serverconn.AuthHeaderKey,
					)
					authSigMu.Unlock()

					msg, marshalErr = protojson.Marshal(
						&mailboxpb.PullResponse{},
					)

				default:
					http.NotFound(w, r)

					return
				}
				require.NoError(t, marshalErr)

				_, err = w.Write(msg)
				require.NoError(t, err)
			},
		),
	)
	defer server.Close()

	// The mailbox edge signs every call with the daemon identity key, so
	// the fixture needs one. Priming the memo stands in for the wallet
	// backend that would produce the signature in a running daemon.
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientMailbox := serverconn.CompoundMailboxID(
		serverconn.PubKeyMailboxID(
			operatorKey.PubKey(),
		),
		serverconn.PubKeyMailboxID(
			clientKey.PubKey(),
		),
	)

	authSig, err := serverconn.SignMailboxAuth(clientKey, clientMailbox)
	require.NoError(t, err)

	s := &Server{
		cfg: &Config{
			Server: &ServerConfig{
				Host:         server.URL,
				Transport:    RPCTransportREST,
				Insecure:     true,
				MacaroonPath: macaroonPath,
			},
		},
		clientKeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
		mailboxAuthSigs: map[string]*schnorr.Signature{
			clientMailbox: authSig,
		},
	}

	clients, err := s.connectOperatorClients()
	require.NoError(t, err)
	require.NotNil(t, clients.ark)
	require.NotNil(t, clients.mailbox)
	require.NoError(t, clients.cleanup())

	info, err := clients.ark.GetInfo(
		t.Context(), &arkrpc.GetInfoRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, operatorPubKey, info.Pubkey)

	_, err = clients.mailbox.Pull(
		t.Context(), &mailboxpb.PullRequest{
			MailboxId: clientMailbox,
		},
	)
	require.NoError(t, err)

	// The operator authorizes Pull from this header when it has no client
	// certificate to bind the caller to a mailbox, so the REST edge has to
	// carry it rather than only the gRPC one.
	require.Equal(
		t,
		hex.EncodeToString(
			authSig.Serialize(),
		),
		pulledAuthSig(),
	)
}

// mailboxAuthCapture is a stub MailboxService that records the mailbox auth
// metadata each inbound RPC carried, standing in for the operator's mailbox
// edge. The mutex is load bearing: gRPC serves the handler on its own
// goroutine, so the assertion below reads what the handler wrote.
type mailboxAuthCapture struct {
	mailboxpb.UnimplementedMailboxServiceServer

	mu      sync.Mutex
	authSig string
}

// Pull records the x-mailbox-auth-sig header the caller sent.
func (m *mailboxAuthCapture) Pull(ctx context.Context,
	_ *mailboxpb.PullRequest) (*mailboxpb.PullResponse, error) {

	md, _ := metadata.FromIncomingContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	if values := md.Get(serverconn.AuthHeaderKey); len(values) > 0 {
		m.authSig = values[0]
	}

	return &mailboxpb.PullResponse{}, nil
}

// pulledAuthSig returns the header recorded by the last Pull.
func (m *mailboxAuthCapture) pulledAuthSig() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.authSig
}

// TestConnectOperatorClientsGRPC verifies the gRPC mailbox edge stamps
// x-mailbox-auth-sig on outbound RPCs, the same property the REST test asserts
// for the gateway transport. An operator that terminates TLS at a proxy has no
// client certificate to authorize Pull with, so the header is the only thing
// standing between this daemon and a rejected mailbox RPC.
func TestConnectOperatorClientsGRPC(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	capture := &mailboxAuthCapture{}
	operator := grpc.NewServer()
	mailboxpb.RegisterMailboxServiceServer(operator, capture)

	go func() {
		_ = operator.Serve(listener)
	}()
	t.Cleanup(operator.Stop)

	// The mailbox edge signs every call with the daemon identity key, so
	// the fixture needs one. Priming the memo stands in for the wallet
	// backend that would produce the signature in a running daemon.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientMailbox := serverconn.CompoundMailboxID(
		serverconn.PubKeyMailboxID(
			operatorKey.PubKey(),
		),
		serverconn.PubKeyMailboxID(
			clientKey.PubKey(),
		),
	)

	authSig, err := serverconn.SignMailboxAuth(clientKey, clientMailbox)
	require.NoError(t, err)

	s := &Server{
		cfg: &Config{
			Server: &ServerConfig{
				Host:      listener.Addr().String(),
				Transport: RPCTransportGRPC,
				Insecure:  true,
			},
		},
		clientKeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
		mailboxAuthSigs: map[string]*schnorr.Signature{
			clientMailbox: authSig,
		},
	}

	clients, err := s.connectOperatorClients()
	require.NoError(t, err)
	require.NotNil(t, clients.ark)
	require.NotNil(t, clients.mailbox)
	t.Cleanup(func() {
		require.NoError(t, clients.cleanup())
	})

	_, err = clients.mailbox.Pull(
		t.Context(), &mailboxpb.PullRequest{
			MailboxId: clientMailbox,
		},
	)
	require.NoError(t, err)

	require.Equal(
		t,
		hex.EncodeToString(
			authSig.Serialize(),
		),
		capture.pulledAuthSig(),
	)
}

// TestConnectOperatorClientsUnknownTransport rejects typoed config early.
func TestConnectOperatorClientsUnknownTransport(t *testing.T) {
	t.Parallel()

	s := &Server{
		cfg: &Config{
			Server: &ServerConfig{
				Host:      "localhost:10010",
				Transport: "webdav",
			},
		},
	}

	_, err := s.connectOperatorClients()
	require.ErrorContains(t, err, "unknown server transport")
}

// TestMailboxAuthSigMemo covers the memo behaviour the two wiring tests above
// deliberately skip past. Both of those prime the map, so a memo hit is all
// they ever exercise; this drives the miss as well, which is the path every
// first Pull takes on a live daemon.
//
// There is no seam to inject a signer — signTaggedSchnorr dispatches on the
// concrete wallet backends — so a miss is observed through the error a Server
// with no backend returns. That is enough to prove which recipients hit and
// which miss, which is the property the compound-vs-plain split rests on.
func TestMailboxAuthSigMemo(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const (
		plain    = "02aa"
		compound = "02bb:02aa"
	)

	primed, err := serverconn.SignMailboxAuth(clientKey, plain)
	require.NoError(t, err)

	s := &Server{
		clientKeyDesc: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
		mailboxAuthSigs: map[string]*schnorr.Signature{
			plain: primed,
		},
	}

	// The primed recipient is served from memory. No wallet backend is
	// configured, so reaching the signing path at all would error.
	got, err := s.mailboxAuthSig(t.Context(), plain)
	require.NoError(t, err)
	require.Same(t, primed, got)

	// A different recipient is a distinct digest and therefore a distinct
	// entry: it misses and goes to the wallet rather than being answered
	// with the first recipient's signature. Serving the wrong signature
	// here would authorize the wrong mailbox at the operator.
	_, err = s.mailboxAuthSig(t.Context(), compound)
	require.ErrorContains(t, err, "no wallet backend available")

	// A failed sign caches nothing. Caching it would pin the failure for
	// the life of the process, so every later RPC to that mailbox would
	// fail without ever retrying the wallet.
	require.Len(t, s.mailboxAuthSigs, 1)
	require.NotContains(t, s.mailboxAuthSigs, compound)

	// The miss must not have disturbed the entry that was already there.
	got, err = s.mailboxAuthSig(t.Context(), plain)
	require.NoError(t, err)
	require.Same(t, primed, got)

	// An unset identity key is refused before any of that.
	empty := &Server{}
	_, err = empty.mailboxAuthSig(t.Context(), plain)
	require.ErrorContains(t, err, "identity key not yet derived")
}
