package lnruntime

import (
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestPeerCarriesWireMessages verifies lnd messages are delegated without a
// network peer implementation.
func TestPeerCarriesWireMessages(t *testing.T) {
	t.Parallel()

	remoteKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	transport := &recordingTransport{}
	peer, err := NewPeer(PeerConfig{
		RemoteKey: remoteKey.PubKey(),
		Transport: transport,
		AddChannel: func(*lnpeer.NewChannel, <-chan struct{}) error {
			return nil
		},
	})
	require.NoError(t, err)

	message := &lnwire.Ping{NumPongBytes: 4}
	require.NoError(t, peer.SendMessage(true, message))
	require.Equal(t, []lnwire.Message{message}, transport.messages)
	require.True(t, transport.sync)
	require.Equal(t, "swapdk", peer.Address().Network())
	serializedPeerKey := peer.PubKey()
	require.Equal(
		t, remoteKey.PubKey().SerializeCompressed(),
		serializedPeerKey[:],
	)
}

// TestPeerTracksFundingLifecycle verifies pending IDs and cancellation remain
// visible to lnd's funding manager.
func TestPeerTracksFundingLifecycle(t *testing.T) {
	t.Parallel()

	remoteKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peer, err := NewPeer(PeerConfig{
		RemoteKey: remoteKey.PubKey(),
		Transport: &recordingTransport{},
		AddChannel: func(*lnpeer.NewChannel, <-chan struct{}) error {
			return nil
		},
	})
	require.NoError(t, err)

	channelID := lnwire.ChannelID{1, 2, 3}
	cancel := make(chan struct{})
	require.NoError(t, peer.AddPendingChannel(channelID, cancel))
	require.True(t, peer.HasPendingChannel(channelID))
	require.NoError(t, peer.RemovePendingChannel(channelID))
	require.False(t, peer.HasPendingChannel(channelID))

	close(cancel)
	require.Error(t, peer.AddPendingChannel(channelID, cancel))
}

// TestPeerDisconnectIsIdempotent verifies the logical peer owns one shutdown
// signal even when independent lnd components report failure.
func TestPeerDisconnectIsIdempotent(t *testing.T) {
	t.Parallel()

	remoteKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	var disconnects int
	peer, err := NewPeer(PeerConfig{
		RemoteKey: remoteKey.PubKey(),
		Transport: &recordingTransport{},
		AddChannel: func(*lnpeer.NewChannel, <-chan struct{}) error {
			return nil
		},
		OnDisconnect: func(error) {
			disconnects++
		},
	})
	require.NoError(t, err)

	peer.Disconnect(errors.New("first"))
	peer.Disconnect(errors.New("second"))
	require.Equal(t, 1, disconnects)
	select {
	case <-peer.QuitSignal():
	default:
		t.Fatal("peer quit signal not closed")
	}
	require.Error(t, peer.SendMessage(false, &lnwire.Ping{}))
}

// recordingTransport records the last lnd message batch.
type recordingTransport struct {
	mu       sync.Mutex
	sync     bool
	messages []lnwire.Message
}

// SendMessages records one ordered message batch.
func (t *recordingTransport) SendMessages(syncSend bool,
	messages ...lnwire.Message) error {

	t.mu.Lock()
	defer t.mu.Unlock()

	t.sync = syncSend
	t.messages = append([]lnwire.Message(nil), messages...)

	return nil
}

var _ MessageTransport = (*recordingTransport)(nil)
