package lnruntime

import (
	"fmt"
	"net"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lnpeer"
	"github.com/lightningnetwork/lnd/lnwire"
)

// MessageTransport carries lnd wire messages over the embedding application's
// authenticated connection instead of opening a BOLT 8 socket.
type MessageTransport interface {
	SendMessages(sync bool, messages ...lnwire.Message) error
}

// NewChannelHandler installs a channel link after lnd's funding manager has
// completed channel opening.
type NewChannelHandler func(*lnpeer.NewChannel, <-chan struct{}) error

// PeerConfig describes one virtual lnd peer backed by an application
// transport.
type PeerConfig struct {
	RemoteKey      *btcec.PublicKey
	Address        net.Addr
	Transport      MessageTransport
	LocalFeatures  *lnwire.FeatureVector
	RemoteFeatures *lnwire.FeatureVector
	AddChannel     NewChannelHandler
	WipeChannel    func(*wire.OutPoint)
	OnDisconnect   func(error)
}

// Peer adapts an authenticated swapdk connection to lnd's existing peer
// interface. It deliberately owns no socket, handshake, channel state, or
// message journal.
type Peer struct {
	cfg PeerConfig

	mu      sync.RWMutex
	pending map[lnwire.ChannelID]struct{}

	quit     chan struct{}
	stopOnce sync.Once
}

// NewPeer validates and creates a transport-backed lnd peer.
func NewPeer(cfg PeerConfig) (*Peer, error) {
	if cfg.RemoteKey == nil {
		return nil, fmt.Errorf("remote peer key is required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("peer message transport is required")
	}
	if cfg.AddChannel == nil {
		return nil, fmt.Errorf("new channel handler is required")
	}

	if cfg.Address == nil {
		// lnd persists this diagnostic field in its channel record and
		// its schema only supports standard peer address encodings.
		// Message delivery still uses Transport; no TCP connection is
		// opened.
		cfg.Address = &net.TCPAddr{IP: net.IPv4zero}
	}
	if cfg.LocalFeatures == nil {
		cfg.LocalFeatures = emptyFeatureVector()
	}
	if cfg.RemoteFeatures == nil {
		cfg.RemoteFeatures = emptyFeatureVector()
	}

	return &Peer{
		cfg:     cfg,
		pending: make(map[lnwire.ChannelID]struct{}),
		quit:    make(chan struct{}),
	}, nil
}

// SendMessage sends high-priority lnd messages through the application
// transport.
func (p *Peer) SendMessage(sync bool, messages ...lnwire.Message) error {
	return p.send(sync, messages...)
}

// SendMessageLazy sends low-priority lnd messages through the same ordered
// transport. Transport implementations may choose a lower delivery priority
// when sync is false.
func (p *Peer) SendMessageLazy(sync bool, messages ...lnwire.Message) error {
	return p.send(sync, messages...)
}

// send rejects writes after disconnect before handing messages to the
// transport.
func (p *Peer) send(sync bool, messages ...lnwire.Message) error {
	select {
	case <-p.quit:
		return fmt.Errorf("virtual peer disconnected")

	default:
	}

	return p.cfg.Transport.SendMessages(sync, messages...)
}

// AddNewChannel delegates link installation to the composed channel runtime.
func (p *Peer) AddNewChannel(channel *lnpeer.NewChannel,
	cancel <-chan struct{}) error {

	select {
	case <-cancel:
		return fmt.Errorf("new channel installation canceled")

	case <-p.quit:
		return fmt.Errorf("virtual peer disconnected")

	default:
	}

	return p.cfg.AddChannel(channel, cancel)
}

// AddPendingChannel records the temporary channel identifier used by lnd's
// funding manager.
func (p *Peer) AddPendingChannel(channelID lnwire.ChannelID,
	cancel <-chan struct{}) error {

	select {
	case <-cancel:
		return fmt.Errorf("pending channel registration canceled")

	case <-p.quit:
		return fmt.Errorf("virtual peer disconnected")

	default:
	}

	p.mu.Lock()
	p.pending[channelID] = struct{}{}
	p.mu.Unlock()

	return nil
}

// RemovePendingChannel clears a completed or failed funding reservation.
func (p *Peer) RemovePendingChannel(channelID lnwire.ChannelID) error {
	p.mu.Lock()
	delete(p.pending, channelID)
	p.mu.Unlock()

	return nil
}

// HasPendingChannel reports whether lnd currently associates the temporary
// identifier with this peer.
func (p *Peer) HasPendingChannel(channelID lnwire.ChannelID) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	_, ok := p.pending[channelID]

	return ok
}

// WipeChannel removes application indexes after lnd abandons a channel.
func (p *Peer) WipeChannel(channelPoint *wire.OutPoint) {
	if p.cfg.WipeChannel != nil {
		p.cfg.WipeChannel(channelPoint)
	}
}

// PubKey returns the compressed remote node key.
func (p *Peer) PubKey() [33]byte {
	var serialized [33]byte
	copy(serialized[:], p.cfg.RemoteKey.SerializeCompressed())

	return serialized
}

// IdentityKey returns the remote node key.
func (p *Peer) IdentityKey() *btcec.PublicKey {
	return p.cfg.RemoteKey
}

// Address returns a logical address for diagnostics only.
func (p *Peer) Address() net.Addr {
	return p.cfg.Address
}

// QuitSignal closes when the application transport disconnects the peer.
func (p *Peer) QuitSignal() <-chan struct{} {
	return p.quit
}

// LocalFeatures returns the features offered by this composed runtime.
func (p *Peer) LocalFeatures() *lnwire.FeatureVector {
	return p.cfg.LocalFeatures
}

// RemoteFeatures returns the negotiated operator feature vector.
func (p *Peer) RemoteFeatures() *lnwire.FeatureVector {
	return p.cfg.RemoteFeatures
}

// Disconnect terminates the logical peer and notifies the transport owner.
func (p *Peer) Disconnect(reason error) {
	p.stopOnce.Do(func() {
		close(p.quit)
		if p.cfg.OnDisconnect != nil {
			p.cfg.OnDisconnect(reason)
		}
	})
}

// emptyFeatureVector returns a feature vector with no optional behavior.
func emptyFeatureVector() *lnwire.FeatureVector {
	return lnwire.NewFeatureVector(lnwire.NewRawFeatureVector(), nil)
}

var _ lnpeer.Peer = (*Peer)(nil)
