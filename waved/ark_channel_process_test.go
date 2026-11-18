package waved

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// arkChannelControllerStub records local RPC calls and returns a configured
// durable channel record.
type arkChannelControllerStub struct {
	record        arkchannel.Record
	records       []arkchannel.Record
	err           error
	id            arkchannel.ID
	localBalance  btcutil.Amount
	remoteBalance btcutil.Amount
	balanceErr    error
}

// PromoteVTXO is not used by close RPC tests.
func (s *arkChannelControllerStub) PromoteVTXO(context.Context, btcutil.Amount,
	string) (arkchannel.Record, error) {

	return s.record, s.err
}

// ListChannels returns configured durable records.
func (s *arkChannelControllerStub) ListChannels(context.Context) (
	[]arkchannel.Record, error) {

	return s.records, s.err
}

// ChannelBalance returns configured live channel balances.
func (s *arkChannelControllerStub) ChannelBalance(context.Context,
	arkchannel.ID) (btcutil.Amount, btcutil.Amount, error) {

	return s.localBalance, s.remoteBalance, s.balanceErr
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
	lntypes.Hash, btcutil.Amount, uint64) (arkchannel.ID, uint32, error) {

	return arkchannel.ID{}, 0, s.err
}

// WaitIncomingPayment is not used by close RPC tests.
func (s *arkChannelControllerStub) WaitIncomingPayment(context.Context,
	lntypes.Hash) (arkchannel.ID, error) {

	return arkchannel.ID{}, s.err
}

// SettleIncomingPayment is not used by lifecycle RPC tests.
func (s *arkChannelControllerStub) SettleIncomingPayment(context.Context,
	lntypes.Preimage) error {

	return s.err
}

// CancelIncomingPayment is not used by lifecycle RPC tests.
func (s *arkChannelControllerStub) CancelIncomingPayment(context.Context,
	lntypes.Hash, string) error {

	return s.err
}

// MaterializeAndForceClose is not used by refresh RPC tests.
func (s *arkChannelControllerStub) MaterializeAndForceClose(context.Context,
	arkchannel.ID) (arkchannel.Record, chainhash.Hash, chainhash.Hash,
	error) {

	return s.record, chainhash.Hash{}, chainhash.Hash{}, s.err
}

// RefreshChannel records the parsed public request.
func (s *arkChannelControllerStub) RefreshChannel(_ context.Context,
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

// TestArkChannelRPCRefreshChannel verifies fixed-width ID parsing and
// controller dispatch on the local authenticated RPC surface.
func TestArkChannelRPCRefreshChannel(t *testing.T) {
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

	response, err := rpcServer.RefreshChannel(
		t.Context(), &arkchannelrpc.RefreshChannelRequest{
			ChannelId: id[:],
		},
	)
	require.NoError(t, err)
	require.Equal(t, id, controller.id)
	require.Equal(t, id[:], response.GetChannel().GetChannelId())
	require.Equal(t, uint64(4), response.GetChannel().GetRevision())

	_, err = rpcServer.RefreshChannel(
		t.Context(), &arkchannelrpc.RefreshChannelRequest{
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

// TestArkChannelRPCListIncludesBalances verifies durable listing remains the
// primary result while active lnd balances are included when available.
func TestArkChannelRPCListIncludesBalances(t *testing.T) {
	t.Parallel()

	id := arkchannel.ID{4, 5, 6}
	record := arkchannel.Record{
		Snapshot: arkchannel.Snapshot{
			Terms: arkchannel.Terms{
				ID: id, Capacity: 50_000,
			},
			Phase: arkchannel.PhaseActive,
		},
	}
	controller := &arkChannelControllerStub{
		records: []arkchannel.Record{
			record,
		},
		localBalance: 20_000, remoteBalance: 30_000,
	}
	rpcServer := &arkChannelRPCServer{server: &Server{
		arkChannelController: controller,
	}}

	response, err := rpcServer.ListChannels(
		t.Context(), &arkchannelrpc.ListChannelsRequest{},
	)
	require.NoError(t, err)
	require.Len(t, response.GetChannels(), 1)
	channel := response.GetChannels()[0]
	require.Equal(t, id[:], channel.GetChannelId())
	require.True(t, channel.GetBalanceAvailable())
	require.EqualValues(t, 20_000, channel.GetLocalBalanceSat())
	require.EqualValues(t, 30_000, channel.GetRemoteBalanceSat())
}

// TestArkChannelRPCRejectsLongIdempotencyKey verifies malformed retry keys are
// rejected before channel startup or wallet selection.
func TestArkChannelRPCRejectsLongIdempotencyKey(t *testing.T) {
	t.Parallel()

	rpcServer := &arkChannelRPCServer{server: &Server{}}
	_, err := rpcServer.PromoteVTXO(
		t.Context(), &arkchannelrpc.PromoteVTXORequest{
			AmountSat:      1_000,
			IdempotencyKey: strings.Repeat("a", 129),
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestArkChannelRPCRequiresIdempotencyKey verifies every funding request has a
// stable recovery identity before the daemon can select wallet inputs.
func TestArkChannelRPCRequiresIdempotencyKey(t *testing.T) {
	t.Parallel()

	rpcServer := &arkChannelRPCServer{server: &Server{}}
	_, err := rpcServer.PromoteVTXO(
		t.Context(), &arkchannelrpc.PromoteVTXORequest{
			AmountSat: 1_000,
		},
	)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestArkChannelRPCPreservesRetryablePromotionStatus verifies the CLI can
// distinguish a resumable transport failure from a terminal funding error.
func TestArkChannelRPCPreservesRetryablePromotionStatus(t *testing.T) {
	t.Parallel()

	controller := &arkChannelControllerStub{
		err: status.Error(codes.Unavailable, "operator offline"),
	}
	rpcServer := &arkChannelRPCServer{server: &Server{
		cfg: &Config{
			Swap: &SwapConfig{
				ArkChannelsEnabled: true,
			},
		},
		arkChannelMailboxRuntime: &serverconn.Runtime{},
		arkChannelController:     controller,
	}}
	_, err := rpcServer.PromoteVTXO(
		t.Context(), &arkchannelrpc.PromoteVTXORequest{
			AmountSat: 1_000, IdempotencyKey: "create-44",
		},
	)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// TestChannelAdmissionDoesNotStartRecovery verifies that permission alone,
// rejected opens and all pay attempts leave an empty wallet dormant.
func TestChannelAdmissionDoesNotStartRecovery(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		s := &Server{cfg: &Config{Swap: &SwapConfig{
			ArkChannelsEnabled: enabled,
			ArkChannelMailbox: mailboxpb.NewMailboxServiceClient(
				nil,
			),
		}}}
		s.armArkChannelStartup(t.Context())
		require.NotNil(t, s.arkChannelStart)
		require.Nil(t, s.arkChannelController)
		calls := 0
		s.arkChannelStart = func() {
			calls++
			s.arkChannelController = &arkChannelControllerStub{}
			s.arkChannelMailboxRuntime = &serverconn.Runtime{}
		}
		_, err := s.channelActionController(t.Context(), false)
		require.Error(t, err)
		require.Zero(t, calls)
		for range 2 {
			_, err = s.channelActionController(t.Context(), true)
			if enabled {
				require.NoError(t, err)
				require.Equal(t, 1, calls)
			} else {
				require.Equal(
					t, codes.FailedPrecondition,
					status.Code(err),
				)
				require.Zero(t, calls)
			}
		}
	}
}

// TestChannelRecoveryInventory verifies that a read failure is not an empty
// inventory, and admission is irrelevant to protecting persisted channels.
func TestChannelRecoveryInventory(t *testing.T) {
	readErr := errors.New("unreadable channel inventory")
	for _, disabled := range []bool{false, true} {
		cfg := &SwapConfig{SuppressArkChannelRuntime: disabled}
		for _, live := range []bool{false, true} {
			list := func(context.Context) ([]arkchannel.Record,
				error) {

				if live {
					return []arkchannel.Record{{}}, nil
				}

				return nil, nil
			}
			needed, err := channelRecoveryRequired(
				t.Context(), cfg, list,
			)
			if live && disabled {
				require.ErrorContains(t, err, "cannot disable")
			} else {
				require.NoError(t, err)
				require.Equal(t, live, needed)
			}
		}
		_, err := channelRecoveryRequired(t.Context(), cfg,
			func(context.Context) ([]arkchannel.Record, error) {
				return nil, readErr
			})
		require.ErrorIs(t, err, readErr)
	}
}

// TestArkChannelInventoryWithoutController uses only durable storage, even
// after reopening the wallet with admission off and no peer transport.
func TestArkChannelInventoryWithoutController(t *testing.T) {
	dir := t.TempDir()
	raw, coordinator := testArkChannelResumeStore(t, dir)
	record := testArkChannelResumeRecord(t, coordinator, false)
	require.NoError(t, raw.Close())

	raw, _ = testArkChannelResumeStore(t, dir)
	store := raw.NewArkChannelStore(clock.NewDefaultClock())
	cfg := &SwapConfig{SuppressArkChannelRuntime: true}
	_, err := channelRecoveryRequired(
		t.Context(), cfg, store.ListNonTerminal,
	)
	require.ErrorContains(t, err, "cannot disable")

	s := &Server{arkChannelStore: store}
	rpc := &arkChannelRPCServer{server: s}
	id := record.Snapshot.Terms.ID
	req := &arkchannelrpc.GetChannelRequest{ChannelId: id[:]}
	resp, err := rpc.GetChannel(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, id[:], resp.GetChannel().GetChannelId())
	require.Nil(t, s.getArkChannelController())

	require.NoError(t, raw.Close())
	_, err = channelRecoveryRequired(
		t.Context(), cfg, store.ListNonTerminal,
	)
	require.ErrorContains(t, err, "read recoverable Ark channels")
	_, err = rpc.GetChannel(t.Context(), req)
	require.Equal(t, codes.Internal, status.Code(err))
}

// TestRunRetriedArkChannelStartupRecovers verifies a transient optional
// channel failure cannot permanently strand the process after wallet unlock.
func TestRunRetriedArkChannelStartupRecovers(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	var failures atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)

		runRetriedArkChannelStartup(
			t.Context(), time.Millisecond,
			func(context.Context) error {
				if attempts.Add(1) == 1 {
					return errors.New("indexer " +
						"temporarily offline")
				}

				return nil
			},
			func(error) {
				failures.Add(1)
			},
		)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("channel startup did not recover")
	}
	require.EqualValues(t, 2, attempts.Load())
	require.EqualValues(t, 1, failures.Load())
}

// TestRunRetriedArkChannelStartupStops verifies shutdown interrupts the retry
// delay instead of racing daemon resource teardown.
func TestRunRetriedArkChannelStartupStops(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	failed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)

		runRetriedArkChannelStartup(
			ctx, time.Hour,
			func(context.Context) error {
				return errors.New("operator offline")
			},
			func(error) {
				close(failed)
			},
		)
	}()

	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("channel startup did not attempt")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("channel startup did not stop")
	}
}

var _ ArkChannelController = (*arkChannelControllerStub)(nil)
