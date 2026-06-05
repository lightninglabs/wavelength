package virtualchannel

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestRegisteredChannelAcceptorRejectsUnknownZeroConf verifies that the
// integrated lnd acceptor fails closed for unregistered virtual channels.
func TestRegisteredChannelAcceptorRejectsUnknownZeroConf(t *testing.T) {
	t.Parallel()

	acceptor, err := NewRegisteredChannelAcceptor(
		RegisteredChannelAcceptorConfig{
			Store: &fakeMaterializationStore{
				byPending: make(
					map[PendingChannelID]*PendingOpen,
				),
			},
		},
	)
	require.NoError(t, err)

	resp := acceptor.Accept(registeredOpenRequest(t, fixedPendingID(1)))
	require.True(t, resp.RejectChannel())
}

// TestRegisteredChannelAcceptorAcceptsRegisteredZeroConf verifies that
// zero-conf opens are accepted only when lnd's pending id, peer, capacity, and
// push amount match the durable virtual-channel negotiation.
func TestRegisteredChannelAcceptorAcceptsRegisteredZeroConf(t *testing.T) {
	t.Parallel()

	pendingID := fixedPendingID(2)
	req := registeredOpenRequest(t, pendingID)
	status := StatusNegotiating
	var remote NodePubKey
	copy(remote[:], req.Node.SerializeCompressed())

	acceptor, err := NewRegisteredChannelAcceptor(
		RegisteredChannelAcceptorConfig{
			Store: &fakeMaterializationStore{
				byPending: map[PendingChannelID]*PendingOpen{
					pendingID: {
						PendingChannelID: pendingID,
						RemoteNodePubKey: remote,
						Status:           status,
						Capacity: btcutil.Amount(
							100_000,
						),
						LocalBalance: 2_500,
						BackingVTXOs: []BackingVTXO{
							testBackingVTXO(1),
						},
					},
				},
			},
		},
	)
	require.NoError(t, err)

	resp := acceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.True(t, resp.ZeroConf)
}

// TestRegisteredChannelAcceptorAcceptsOperatorLiquidityOpen verifies the
// receive-channel invariant: an operator-opened channel with push amount zero
// is accepted only when the persisted client-local balance is zero.
func TestRegisteredChannelAcceptorAcceptsOperatorLiquidityOpen(t *testing.T) {
	t.Parallel()

	pendingID := fixedPendingID(9)
	req := registeredOpenRequestWithPush(t, pendingID, 0)
	status := StatusNegotiating
	var remote NodePubKey
	copy(remote[:], req.Node.SerializeCompressed())

	acceptor, err := NewRegisteredChannelAcceptor(
		RegisteredChannelAcceptorConfig{
			Store: &fakeMaterializationStore{
				byPending: map[PendingChannelID]*PendingOpen{
					pendingID: {
						PendingChannelID: pendingID,
						RemoteNodePubKey: remote,
						Status:           status,
						Capacity: btcutil.Amount(
							100_000,
						),
						LocalBalance:  0,
						RemoteBalance: 100_000,
						BackingVTXOs: []BackingVTXO{
							testBackingVTXO(9),
						},
					},
				},
			},
		},
	)
	require.NoError(t, err)

	resp := acceptor.Accept(req)
	require.False(t, resp.RejectChannel())
	require.True(t, resp.ZeroConf)
}

// TestRegisteredChannelAcceptorRejectsMismatchedPeer verifies that a registered
// pending id alone is not enough to accept an inbound zero-conf channel.
func TestRegisteredChannelAcceptorRejectsMismatchedPeer(t *testing.T) {
	t.Parallel()

	pendingID := fixedPendingID(3)
	status := StatusNegotiating
	acceptor, err := NewRegisteredChannelAcceptor(
		RegisteredChannelAcceptorConfig{
			Store: &fakeMaterializationStore{
				byPending: map[PendingChannelID]*PendingOpen{
					pendingID: {
						PendingChannelID: pendingID,
						Status:           status,
						Capacity: btcutil.Amount(
							100_000,
						),
						BackingVTXOs: []BackingVTXO{
							testBackingVTXO(2),
						},
					},
				},
			},
		},
	)
	require.NoError(t, err)

	resp := acceptor.Accept(registeredOpenRequest(t, pendingID))
	require.True(t, resp.RejectChannel())
}

func registeredOpenRequest(t *testing.T,
	pendingID PendingChannelID) *chanacceptor.ChannelAcceptRequest {

	t.Helper()

	return registeredOpenRequestWithPush(t, pendingID, 2_500_000)
}

func registeredOpenRequestWithPush(t *testing.T, pendingID PendingChannelID,
	push lnwire.MilliSatoshi) *chanacceptor.ChannelAcceptRequest {

	t.Helper()

	priv, _ := btcec.PrivKeyFromBytes([]byte{
		1, 2, 3, 4, 5, 6, 7, 8,
		9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24,
		25, 26, 27, 28, 29, 30, 31, 32,
	})
	channelType := new(lnwire.ChannelType)
	*channelType = lnwire.ChannelType(
		*lnwire.NewRawFeatureVector(
			lnwire.ZeroConfRequired,
		),
	)

	return &chanacceptor.ChannelAcceptRequest{
		Node: priv.PubKey(),
		OpenChanMsg: &lnwire.OpenChannel{
			PendingChannelID: pendingID,
			FundingAmount:    100_000,
			PushAmount:       push,
			ChannelType:      channelType,
		},
	}
}

func fixedPendingID(fill byte) PendingChannelID {
	var id PendingChannelID
	for i := range id {
		id[i] = fill
	}

	return id
}

func testBackingVTXO(fill byte) BackingVTXO {
	hash := chainhash.Hash{}
	for i := range hash {
		hash[i] = fill
	}

	return BackingVTXO{
		OutPoint: wire.OutPoint{
			Hash:  hash,
			Index: uint32(fill),
		},
		Amount: 100_500,
	}
}
