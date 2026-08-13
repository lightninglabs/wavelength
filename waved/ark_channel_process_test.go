package waved

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// arkChannelControllerStub records local RPC calls and returns a configured
// durable channel record.
type arkChannelControllerStub struct {
	record arkchannel.Record
	err    error
	id     arkchannel.ID
}

// PromoteVTXO is not used by close RPC tests.
func (s *arkChannelControllerStub) PromoteVTXO(context.Context,
	btcutil.Amount) (arkchannel.Record, error) {

	return s.record, s.err
}

// SendPayment is not used by close RPC tests.
func (s *arkChannelControllerStub) SendPayment(context.Context, arkchannel.ID,
	btcutil.Amount) (lntypes.Hash, error) {

	return lntypes.Hash{}, s.err
}

// ReceivePayment is not used by close RPC tests.
func (s *arkChannelControllerStub) ReceivePayment(context.Context,
	arkchannel.ID, btcutil.Amount) (lntypes.Hash, error) {

	return lntypes.Hash{}, s.err
}

// PayLightningInvoice is not used by close RPC tests.
func (s *arkChannelControllerStub) PayLightningInvoice(context.Context, string,
	btcutil.Amount) (LightningPaymentResult, error) {

	return LightningPaymentResult{}, s.err
}

// PrepareIncomingPayment is not used by close RPC tests.
func (s *arkChannelControllerStub) PrepareIncomingPayment(context.Context,
	lntypes.Preimage, btcutil.Amount) error {

	return s.err
}

// RegisterIncomingPayment is not used by close RPC tests.
func (s *arkChannelControllerStub) RegisterIncomingPayment(context.Context,
	lntypes.Hash, btcutil.Amount, uint64) error {

	return s.err
}

// WaitIncomingPayment is not used by close RPC tests.
func (s *arkChannelControllerStub) WaitIncomingPayment(context.Context,
	lntypes.Hash) error {

	return s.err
}

// PromoteIncomingVHTLC is not used by close RPC tests.
func (s *arkChannelControllerStub) PromoteIncomingVHTLC(context.Context,
	lntypes.Hash, uint64, btcutil.Amount, ArkChannelClaimSource) (
	arkchannel.Record, error) {

	return s.record, s.err
}

// MaterializeAndForceClose is not used by close RPC tests.
func (s *arkChannelControllerStub) MaterializeAndForceClose(context.Context,
	arkchannel.ID) (arkchannel.Record, chainhash.Hash, chainhash.Hash,
	error) {

	return s.record, chainhash.Hash{}, chainhash.Hash{}, s.err
}

// RequestCooperativeClose records the parsed public request.
func (s *arkChannelControllerStub) RequestCooperativeClose(_ context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	s.id = id

	return s.record, s.err
}

// GetChannel returns the configured channel record.
func (s *arkChannelControllerStub) GetChannel(_ context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	s.id = id

	return s.record, s.err
}

// PeerMessageHandler returns an inert native peer handler for RPC-only tests.
//
//nolint:ll // The concrete method name and interface type are both significant.
func (*arkChannelControllerStub) PeerMessageHandler() lnruntime.PeerEventHandler {
	return func(context.Context, lnwire.Message) error {
		return nil
	}
}

// Stop is inert for the RPC-only controller stub.
func (*arkChannelControllerStub) Stop() error {
	return nil
}

// TestArkChannelRPCRequestCooperativeClose verifies fixed-width ID parsing and
// controller dispatch on the local authenticated RPC surface.
func TestArkChannelRPCRequestCooperativeClose(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1, 2, 3}
	controller := &arkChannelControllerStub{
		record: arkchannel.Record{
			Snapshot: arkchannel.Snapshot{
				Terms: arkchannel.Terms{
					ID: id,
				},
				Phase: arkchannel.PhaseCoopClosing,
			},
			Revision: 4,
		},
	}
	server := &Server{arkChannelController: controller}
	rpcServer := &arkChannelRPCServer{server: server}

	response, err := rpcServer.RequestCooperativeClose(
		t.Context(), &arkchannelrpc.RequestCooperativeCloseRequest{
			ChannelId: id[:],
		},
	)
	require.NoError(t, err)
	require.Equal(t, id, controller.id)
	require.Equal(t, id[:], response.GetChannel().GetChannelId())
	require.Equal(t, uint64(4), response.GetChannel().GetRevision())

	_, err = rpcServer.RequestCooperativeClose(
		t.Context(), &arkchannelrpc.RequestCooperativeCloseRequest{
			ChannelId: []byte{1},
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestArkChannelRPCUnavailable verifies the local service remains registered
// but fails explicitly when no compiled-in channel runtime was installed.
func TestArkChannelRPCUnavailable(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{1}
	rpcServer := &arkChannelRPCServer{server: &Server{}}

	_, err := rpcServer.GetChannel(
		t.Context(), &arkchannelrpc.GetChannelRequest{
			ChannelId: id[:],
		},
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

var _ ArkChannelController = (*arkChannelControllerStub)(nil)
