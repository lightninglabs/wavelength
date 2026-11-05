package waved

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	"github.com/lightninglabs/wavelength/rpc/restclient"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// bootstrapRefreshTransport models a conforming operator and completes local
// bootstrap while an earlier refresh response is in flight.
type bootstrapRefreshTransport struct {
	server   *Server
	pubkey   []byte
	requests [][]uint32
}

// RoundTrip returns zero for an unbound request and one for a version-one
// request. Completing bootstrap before returning the former exposes the race
// without an invalid operator response or any live endpoint.
func (r *bootstrapRefreshTransport) RoundTrip(req *http.Request) (
	*http.Response, error) {

	defer func() {
		_ = req.Body.Close()
	}()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var request arkrpc.GetInfoRequest
	if err := protojson.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	r.requests = append(r.requests, request.SupportedArkVersions)
	selected := uint32(0)
	for _, version := range request.SupportedArkVersions {
		if version == 1 {
			selected = 1
		}
	}
	// Bootstrap is the only writer in production. Model it completing after
	// this refresh captured zero but before it validates the response.
	atomic.StoreUint32(&r.server.arkProtocolVersion, 1)
	response, err := protojson.Marshal(
		&arkrpc.GetInfoResponse{
			SelectedArkVersion: selected,
			Pubkey:             r.pubkey,
			ArkVersionPolicies: []*arkrpc.ArkVersionPolicy{
				activeArkPolicy(1),
			},
		},
	)
	if err != nil {
		return nil, err
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{
				"application/json",
			},
		},
		Body: io.NopCloser(strings.NewReader(string(response))),
	}, nil
}

// TestOperatorPubKeyBeforeBootstrapDoesNotPoisonVersion proves an optional
// subserver cannot send a refresh advertising zero before bootstrap binds the
// runtime. Otherwise a correct zero-selection response can arrive after binding
// and be mistaken for a permanent operator incompatibility.
func TestOperatorPubKeyBeforeBootstrapDoesNotPoisonVersion(t *testing.T) {
	t.Parallel()
	server := &Server{log: btclog.Disabled}
	transport := &bootstrapRefreshTransport{
		server: server,
		pubkey: testOperatorPubKeyBytes(t),
	}
	server.arkClient = restclient.NewArkServiceClient(
		"http://operator.invalid",
		restclient.WithHTTPClient(
			&http.Client{
				Transport: transport,
			},
		),
	)
	rpc := &RPCServer{server: server}
	_, err := rpc.OperatorPubKey(t.Context())
	require.Error(t, err)
	require.False(t, mailboxconn.IsPermanentVersionError(err), "%v", err)
	require.Empty(
		t, transport.requests,
		"an unbound refresh must not reach the operator",
	)

	// The same caller succeeds once bootstrap has selected the protocol.
	atomic.StoreUint32(&server.arkProtocolVersion, 1)
	key, err := rpc.OperatorPubKey(t.Context())
	require.NoError(t, err)
	require.Equal(t, transport.pubkey, key.SerializeCompressed())
	require.Equal(t, [][]uint32{{1}}, transport.requests)
}
