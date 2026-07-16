package lndrest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/rpc/restclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const testMacHex = "0a0b6465616462656566"

// newTestConn builds a conn pointed at the test server. The server URL carries
// an http:// scheme so the REST transport talks plaintext to httptest.
func newTestConn(srvURL string) *conn {
	rc := restclient.New(
		srvURL, restclient.WithHeader(macaroonHeader, testMacHex),
	)

	return &conn{
		rc:      rc,
		http:    http.DefaultClient,
		timeout: 5 * time.Second,
		params:  &chaincfg.RegressionNetParams,
	}
}

// writeProtoJSON marshals a proto message as the grpc-gateway unary response
// body would.
func writeProtoJSON(t *testing.T, w http.ResponseWriter, msg proto.Message) {
	t.Helper()

	body, err := protojson.Marshal(msg)
	require.NoError(t, err)

	_, err = w.Write(body)
	require.NoError(t, err)
}

// TestWalletKitDeriveKey checks that DeriveKey POSTs to the right path with the
// macaroon header and locator body, and decodes the returned key descriptor.
func TestWalletKitDeriveKey(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubBytes := priv.PubKey().SerializeCompressed()

	loc := keychain.KeyLocator{Family: 6, Index: 3}

	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, pathDeriveKey, r.URL.Path)
				require.Equal(
					t, testMacHex,
					r.Header.Get(macaroonHeader),
				)

				var reqMsg signrpc.KeyLocator
				decodeBody(t, r, &reqMsg)
				require.Equal(t, int32(6), reqMsg.KeyFamily)
				require.Equal(t, int32(3), reqMsg.KeyIndex)

				writeProtoJSON(t, w, &signrpc.KeyDescriptor{
					RawKeyBytes: pubBytes,
					KeyLoc: &signrpc.KeyLocator{
						KeyFamily: 6,
						KeyIndex:  3,
					},
				})
			},
		),
	)
	defer srv.Close()

	client := &walletKitClient{conn: newTestConn(srv.URL)}
	desc, err := client.DeriveKey(context.Background(), &loc)
	require.NoError(t, err)
	require.Equal(t, loc, desc.KeyLocator)
	require.Equal(t, pubBytes, desc.PubKey.SerializeCompressed())
}

// TestChainKitGetBlockHash checks that GetBlockHash issues a GET to the right
// path with the block height bound as a query parameter, and decodes the hash.
func TestChainKitGetBlockHash(t *testing.T) {
	t.Parallel()

	var wantHash [32]byte
	for i := range wantHash {
		wantHash[i] = byte(i + 1)
	}

	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, pathGetBlockHash, r.URL.Path)
				require.Equal(
					t, "42",
					r.URL.Query().Get("block_height"),
				)

				writeProtoJSON(
					t, w, &chainrpc.GetBlockHashResponse{
						BlockHash: wantHash[:],
					},
				)
			},
		),
	)
	defer srv.Close()

	client := &chainKitClient{conn: newTestConn(srv.URL)}
	hash, err := client.GetBlockHash(context.Background(), 42)
	require.NoError(t, err)
	require.Equal(t, wantHash[:], hash[:])
}

// TestChainNotifierBlockEpoch checks that RegisterBlockEpochNtfn opens the
// streaming endpoint and forwards each streamed block height onto the returned
// channel using the grpc-gateway {"result": ...} chunk shape.
func TestChainNotifierBlockEpoch(t *testing.T) {
	t.Parallel()

	heights := []uint32{100, 101}

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, pathRegisterBlocks, r.URL.Path)

			flusher, ok := w.(http.Flusher)
			require.True(t, ok)

			for _, h := range heights {
				epoch := &chainrpc.BlockEpoch{Height: h}
				body, err := protojson.Marshal(epoch)
				require.NoError(t, err)

				_, err = fmt.Fprintf(
					w, `{"result":%s}`+"\n", body,
				)
				require.NoError(t, err)
				flusher.Flush()
			}

			// Keep the stream open until the client cancels so the
			// reader observes clean events rather than a racing
			// EOF.
			<-r.Context().Done()
		},
	))
	defer srv.Close()

	client := &chainNotifierClient{conn: newTestConn(srv.URL)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	epochChan, errChan, err := client.RegisterBlockEpochNtfn(ctx)
	require.NoError(t, err)

	for _, want := range heights {
		select {
		case got := <-epochChan:
			require.Equal(t, int32(want), got)

		case err := <-errChan:
			t.Fatalf("unexpected stream error: %v", err)

		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for block epoch")
		}
	}
}

// decodeBody decodes the JSON request body into a proto message.
func decodeBody(t *testing.T, r *http.Request, out proto.Message) {
	t.Helper()

	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	require.NoError(t, protojson.Unmarshal(body, out))
}
