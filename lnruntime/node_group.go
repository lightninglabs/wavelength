package lnruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/lnpeer"
)

// NativeNodeGroup owns one native engine and database for many logical peers.
// Prepare every persisted peer and backing before starting any member. Member
// Stop never shuts down the engine: its process owner must call group Stop
// before releasing the peers' Ark stores.
type NativeNodeGroup struct {
	db       *channeldb.DB
	lookup   func(context.Context, [32]byte) (arkchannel.Record, error)
	runtime  *Runtime
	notifier *VirtualFundingNotifier

	createMu    sync.Mutex
	peersMu     sync.RWMutex
	nodes       map[[33]byte]*NativeNode
	peerChanges chan struct{}
	started     bool
	stopped     bool
}

// NewNativeNodeGroup transfers database ownership to a process-scoped engine.
func NewNativeNodeGroup(db *channeldb.DB,
	lookup func(context.Context, [32]byte) (arkchannel.Record, error)) (
	*NativeNodeGroup, error) {

	if db == nil || lookup == nil {
		return nil, fmt.Errorf("shared channel database and intent " +
			"lookup required")
	}

	return &NativeNodeGroup{
		db: db, lookup: lookup, nodes: make(map[[33]byte]*NativeNode),
		peerChanges: make(chan struct{}),
	}, nil
}

// NewNode prepares one authenticated hub peer without starting the engine.
// DB ownership and common dependencies remain with the group/process owner.
func (g *NativeNodeGroup) NewNode(cfg NativeNodeConfig) (*NativeNode, error) {
	g.createMu.Lock()
	defer g.createMu.Unlock()
	if g.stopped {
		return nil, fmt.Errorf("shared native engine is stopped")
	}
	if cfg.Party != arkchannel.PartyHub || cfg.RemoteNodeKey == nil ||
		cfg.IdentityKey.PubKey == nil {
		return nil, fmt.Errorf("shared native engine requires a hub " +
			"peer")
	}
	var remote [33]byte
	copy(remote[:], cfg.RemoteNodeKey.SerializeCompressed())
	if prior, err := g.node(remote); err == nil {
		select {
		case <-prior.peer.QuitSignal():
			// Keep the old owner until construction succeeds.
			// Replacing a transport must not create a gap in chain
			// callbacks.

		default:
			return nil, fmt.Errorf("native hub peer is already " +
				"registered")
		}
	}
	if g.runtime != nil && g.runtime.cfg.SelfNode !=
		[33]byte(cfg.IdentityKey.PubKey.SerializeCompressed()) {
		return nil, fmt.Errorf("shared native engine identity mismatch")
	}
	if g.runtime != nil && g.runtime.cfg.Onchain.MaterializationDelta !=
		cfg.MaterializationDelta {
		return nil, fmt.Errorf("shared native recovery window mismatch")
	}
	cfg.DB, cfg.OwnsDB = g.db, false

	node, err := newNativeNode(cfg, g)
	if err != nil {
		return nil, err
	}
	g.peersMu.Lock()
	close(g.peerChanges)
	g.peerChanges = make(chan struct{})
	g.peersMu.Unlock()

	return node, nil
}

// start runs native restoration once, after all durable peers are prepared.
func (g *NativeNodeGroup) start() error {
	g.createMu.Lock()
	defer g.createMu.Unlock()
	if g.stopped {
		return fmt.Errorf("shared native engine is stopped")
	}
	if g.started {
		return nil
	}
	if g.runtime == nil {
		return fmt.Errorf("shared native engine has no prepared peers")
	}
	if err := g.validateOwnership(); err != nil {
		return err
	}
	if err := g.runtime.Start(); err != nil {
		return err
	}
	g.started = true

	return nil
}

// validateOwnership keeps orphaned pending funding out of lnd's funding
// timeout/abandon path. Active ownership admission and pending-close barriers
// can be deferred and retried independently by the chain arbitrator.
func (g *NativeNodeGroup) validateOwnership() error {
	channels, err := g.db.ChannelStateDB().FetchAllChannels()
	if err != nil {
		return err
	}
	for _, channel := range channels {
		if !channel.IsPending {
			continue
		}
		if _, err := g.nodeForKey(channel.IdentityPub); err != nil {
			return fmt.Errorf("restore channel %v ownership: %w",
				channel.FundingOutpoint, err)
		}
	}

	return nil
}

// Stop joins native workers before closing their shared database.
func (g *NativeNodeGroup) Stop() error {
	g.createMu.Lock()
	defer g.createMu.Unlock()
	if g.stopped {
		return nil
	}
	g.stopped = true
	var err error
	if g.runtime != nil {
		err = g.runtime.Stop()
	}

	return errors.Join(err, g.db.Close())
}

// node resolves an authenticated peer without holding a lock across callbacks.
func (g *NativeNodeGroup) node(remote [33]byte) (*NativeNode, error) {
	g.peersMu.RLock()
	node := g.nodes[remote]
	g.peersMu.RUnlock()
	if node == nil {
		return nil, fmt.Errorf("%w: native channel peer %x is not "+
			"registered",
			contractcourt.ErrAuxChannelOwnerUnavailable, remote[:])
	}

	return node, nil
}

// notifyWhenOnline supplies the exact registered peer to lnd funding recovery.
func (g *NativeNodeGroup) notifyWhenOnline(remote [33]byte,
	peerChan chan<- lnpeer.Peer) {

	for {
		g.peersMu.RLock()
		node, changed := g.nodes[remote], g.peerChanges
		g.peersMu.RUnlock()
		if node != nil {
			select {
			case <-node.peer.QuitSignal():
				// Retain its lifecycle owner, but wait for a
				// new authenticated session before funding
				// resumes.

			default:
				select {
				case peerChan <- node.peer:
					return

				case <-node.peer.QuitSignal():
				case <-g.runtime.quit:
					return
				}
			}
		}
		select {
		case <-changed:
		case <-g.runtime.quit:
			return
		}
	}
}

// FindByPendingChannelID reads the process-owned unique funding index. The
// acceptor independently checks the BOLT initiator against the immutable terms.
func (g *NativeNodeGroup) FindByPendingChannelID(ctx context.Context,
	id [32]byte) (arkchannel.Record, error) {

	record, err := g.lookup(ctx, id)
	if err != nil {
		return arkchannel.Record{}, err
	}
	if _, err := g.node(record.Snapshot.Terms.ClientNodeKey); err != nil {
		return arkchannel.Record{}, err
	}

	return record, nil
}

// retireOwner unregisters this exact terminal peer, never a replacement.
func (n *NativeNode) retireOwner() {
	g := n.shared
	if g == nil {
		return
	}
	g.peersMu.Lock()
	if key := n.peer.PubKey(); g.nodes[key] == n {
		delete(g.nodes, key)
		close(g.peerChanges)
		g.peerChanges = make(chan struct{})
	}
	g.peersMu.Unlock()
	n.ownerCalls.Wait()
}

// acquireOwner pins the peer while lnd is executing a recovery callback.
// Removing the peer before waiting excludes any subsequent Add on the group.
func (g *NativeNodeGroup) acquireOwner(n *NativeNode) error {
	g.peersMu.RLock()
	defer g.peersMu.RUnlock()
	if g.nodes[n.peer.PubKey()] != n {
		return contractcourt.ErrAuxChannelOwnerUnavailable
	}
	n.ownerCalls.Add(1)

	return nil
}

// Retire stops the peer's native links and joins its final owner callbacks.
// Transport loss uses Stop instead, retaining the durable lifecycle owner.
func (n *NativeNode) Retire() error {
	err := n.Stop()
	n.retireOwner()

	return err
}

// channelNode uses lnd's durable remote identity, including closed channels.
func (g *NativeNodeGroup) channelNode(point wire.OutPoint) (*NativeNode,
	error) {

	channel, err := g.db.ChannelStateDB().FetchChannel(point)
	if err == nil {
		return g.nodeForKey(channel.IdentityPub)
	}
	if !errors.Is(err, channeldb.ErrChannelNotFound) {
		return nil, err
	}
	closed, err := g.db.ChannelStateDB().FetchClosedChannel(&point)
	if err != nil {
		return nil, err
	}

	return g.nodeForKey(closed.RemotePub)
}

// nodeForKey converts lnd's remote identity into an authenticated session key.
func (g *NativeNodeGroup) nodeForKey(key *btcec.PublicKey) (*NativeNode,
	error) {

	if key == nil {
		return nil, fmt.Errorf("native channel has no remote identity")
	}
	var remote [33]byte
	copy(remote[:], key.SerializeCompressed())

	return g.node(remote)
}

// ChainWatchOwner preserves each peer's Ark-to-lnd chain ownership barrier.
func (g *NativeNodeGroup) ChainWatchOwner(ctx context.Context,
	channel *chanstate.OpenChannel) (contractcourt.ChainWatchOwner, error) {

	if channel == nil || channel.IdentityPub == nil {
		return 0, fmt.Errorf("native channel has no remote identity")
	}
	node, err := g.nodeForKey(channel.IdentityPub)
	if err != nil {
		return 0, err
	}

	if err := g.acquireOwner(node); err != nil {
		return 0, err
	}
	defer node.ownerCalls.Done()

	return node.cfg.ChannelLifecycle.ChainWatchOwner(ctx, channel)
}

// PrepareCommitmentPublish materializes through the owning peer's Ark FSM.
func (g *NativeNodeGroup) PrepareCommitmentPublish(ctx context.Context,
	point wire.OutPoint) error {

	node, err := g.channelNode(point)
	if err != nil {
		return err
	}

	if err := g.acquireOwner(node); err != nil {
		return err
	}
	defer node.ownerCalls.Done()

	return node.cfg.ChannelLifecycle.PrepareCommitmentPublish(ctx, point)
}

// WaitForChannelFinalization records native resolution in the owning Ark FSM.
func (g *NativeNodeGroup) WaitForChannelFinalization(ctx context.Context,
	point wire.OutPoint) error {

	node, err := g.channelNode(point)
	if err != nil {
		return err
	}

	if err := g.acquireOwner(node); err != nil {
		return err
	}
	defer node.ownerCalls.Done()

	return node.cfg.ChannelLifecycle.WaitForChannelFinalization(ctx, point)
}
