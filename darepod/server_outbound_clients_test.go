package darepod

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestConnectOperatorClientsREST verifies the daemon can construct all
// operator-facing clients over the grpc-gateway transport.
func TestConnectOperatorClientsREST(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPubKey := operatorKey.PubKey().SerializeCompressed()

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
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

	s := &Server{
		cfg: &Config{
			Server: &ServerConfig{
				Host:      server.URL,
				Transport: RPCTransportREST,
				Insecure:  true,
			},
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
		t.Context(), &mailboxpb.PullRequest{},
	)
	require.NoError(t, err)
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
