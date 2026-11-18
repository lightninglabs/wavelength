package lnruntime

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestRuntimePreservesAcceptedReservation proves duplicate open_channel and
// unsigned funding_created messages are rejected before native funding can
// replace or persist the existing peer's negotiated reservation.
func TestRuntimePreservesAcceptedReservation(t *testing.T) {
	t.Parallel()
	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})
	pendingID := lndfunding.PendingChanID{5, 5, 1}
	record := fundingIntentRecord(t, hub, client, pendingID)
	hub.intents.record, client.intents.record = record, record
	flow, err := client.runtime.Funding().OpenChannel(FundingOpenRequest{
		Peer: client.peer, PendingChannelID: pendingID,
		Capacity: record.Snapshot.Terms.Capacity,
	})
	require.NoError(t, err)
	_ = awaitFundingPSBT(t, flow)
	_, err = client.runtime.Funding().OpenChannel(FundingOpenRequest{
		Peer: client.peer, PendingChannelID: pendingID,
		Capacity: record.Snapshot.Terms.Capacity + 1,
	})
	require.ErrorContains(t, err, "does not match channel intent")
	_, err = client.runtime.Funding().OpenChannel(FundingOpenRequest{
		Peer: client.peer, PendingChannelID: pendingID,
		Capacity: record.Snapshot.Terms.Capacity,
	})
	require.ErrorContains(t, err, "already negotiating")
	before, err := hub.runtime.Funding().ExpectedFundingOutput(pendingID)
	require.NoError(t, err)
	require.ErrorContains(
		t,
		hub.runtime.HandlePeerMessage(
			t.Context(), &lnwire.OpenChannel{
				PendingChannelID: pendingID,
			},
			hub.peer,
		),
		"already negotiating",
	)
	require.ErrorContains(
		t,
		hub.runtime.HandlePeerMessage(
			t.Context(), &lnwire.FundingCreated{
				PendingChannelID: pendingID,
			},
			hub.peer,
		),
		"no installed signed backing",
	)
	after, err := hub.runtime.Funding().ExpectedFundingOutput(pendingID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// TestRuntimeRejectsSCIDRebinding verifies readiness cannot mistake another
// channel's eligible link for this channel, and AddLink cannot overwrite the
// shared switch's routing entry even before a new link is constructed.
func TestRuntimeRejectsSCIDRebinding(t *testing.T) {
	t.Parallel()
	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})
	first := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{7, 7, 1},
	)
	firstFlow := activateFundingFlowChannel(t, hub, client, first)
	_ = awaitFinalized(t, hub, nil)
	_ = awaitFinalized(t, client, nil)
	second := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{7, 7, 2},
	)
	second.Snapshot.Terms.ReservedSCID += 1 << 16
	secondFlow := activateFundingFlowChannel(t, hub, client, second)
	terms := firstFlow.hubSink.record.Snapshot.Terms
	terms.ReservedSCID = second.Snapshot.Terms.ReservedSCID
	active, err := hub.runtime.Funding().ChannelActive(
		t.Context(), terms, *firstFlow.hubSink.record.Snapshot.Backing,
	)
	require.False(t, active)
	require.ErrorContains(t, err, "SCID resolves to another channel")
	_, err = hub.runtime.AddLink(&chanstate.OpenChannel{
		FundingOutpoint: wire.OutPoint{Index: 9},
		ShortChannelID:  secondFlow.hubChannel.ShortChanID(),
	}, testLinkConfig(hub.peer, hub.failures))
	require.ErrorContains(t, err, "already belongs to another channel")
	link, err := hub.runtime.GetLink(secondFlow.hubChannel.ShortChanID())
	require.NoError(t, err)
	require.Equal(
		t, lnwire.NewChanIDFromOutPoint(
			secondFlow.hubChannel.FundingOutpoint,
		),
		link.ChanID(),
	)
}

// TestRuntimeRejectsCrossPeerMessages verifies authenticated ingress rejects
// another client's channel IDs before touching the shared switch or funding
// manager, including the startup reestablishment expectation.
func TestRuntimeRejectsCrossPeerMessages(t *testing.T) {
	t.Parallel()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})
	record := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{5, 6, 7},
	)
	flow := activateFundingFlowChannel(t, hub, client, record)
	channelID := lnwire.NewChanIDFromOutPoint(
		flow.hubChannel.FundingOutpoint,
	)
	attacker := newRuntimePeer(
		t, testIntentKey(t).PubKey(), &recordingTransport{},
	)
	pendingID := record.Snapshot.Terms.PendingChannelID
	messages := []lnwire.Message{
		&lnwire.CommitSig{
			ChanID: channelID,
		},
		&lnwire.RevokeAndAck{
			ChanID: channelID,
		},
		&lnwire.UpdateAddHTLC{
			ChanID: channelID,
		},
		&lnwire.UpdateFulfillHTLC{
			ChanID: channelID,
		},
		&lnwire.UpdateFailHTLC{
			ChanID: channelID,
		},
		&lnwire.UpdateFailMalformedHTLC{
			ChanID: channelID,
		},
		&lnwire.UpdateFee{
			ChanID: channelID,
		},
		&lnwire.Warning{
			ChanID: channelID,
		},
		&lnwire.Error{
			ChanID: channelID,
		},
		&lnwire.ChannelReestablish{
			ChanID: channelID,
		},
		&lnwire.FundingSigned{
			ChanID: channelID,
		},
		&lnwire.ChannelReady{
			ChanID: channelID,
		},
		&lnwire.AcceptChannel{
			PendingChannelID: pendingID,
		},
		&lnwire.FundingCreated{
			PendingChannelID: pendingID,
		},
	}
	for _, message := range messages {
		require.Error(
			t,
			hub.runtime.HandlePeerMessage(
				t.Context(), message, attacker,
			),
			"%T must be authenticated before native dispatch",
			message,
		)
	}
	hub.runtime.setAwaitingReestablish(channelID, true)
	require.Error(
		t,
		hub.runtime.HandleChannelReestablish(
			&lnwire.ChannelReestablish{
				ChanID: channelID,
			},
			attacker,
			func(*chanstate.OpenChannel) (LinkConfig, error) {
				t.Fatal(
					"unauthorized peer reached link " +
						"configuration",
				)

				return LinkConfig{}, nil
			},
		),
	)
	require.True(t, hub.runtime.claimExpectedReestablish(channelID))
	link, err := hub.runtime.GetLink(flow.hubChannel.ShortChanID())
	require.NoError(t, err)
	require.True(t, link.EligibleToForward())
	select {
	case err := <-hub.failures:
		t.Fatalf("unauthorized message reached native link: %v", err)

	default:
	}
}

// TestRuntimeDetachesOnlyStoppedPeer verifies member cleanup removes its
// links while retaining the native channel state needed for chain resolution.
func TestRuntimeDetachesOnlyStoppedPeer(t *testing.T) {
	t.Parallel()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})
	record := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{6, 8},
	)
	flow := activateFundingFlowChannel(t, hub, client, record)
	hub.runtime.linkMu.Lock()
	changed := hub.runtime.linkChanges
	hub.runtime.linkMu.Unlock()
	hub.peer.Disconnect(context.Canceled)
	require.NoError(t, hub.runtime.RemovePeerLinks(hub.peer))
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("member detachment did not cancel link generation")
	}
	_, err := hub.runtime.GetLink(flow.hubChannel.ShortChanID())
	require.ErrorIs(t, err, htlcswitch.ErrChannelLinkNotFound)
	state, err := hub.db.ChannelStateDB().FetchChannel(
		flow.hubChannel.FundingOutpoint,
	)
	require.NoError(t, err)
	_, err = hub.runtime.AddLink(
		state, testLinkConfig(hub.peer, hub.failures),
	)
	require.ErrorIs(t, err, htlcswitch.ErrLinkShuttingDown)
	_, err = hub.runtime.GetLink(flow.hubChannel.ShortChanID())
	require.ErrorIs(t, err, htlcswitch.ErrChannelLinkNotFound)
	id := lnwire.NewChanIDFromOutPoint(flow.hubChannel.FundingOutpoint)
	hub.runtime.setAwaitingReestablish(id, true)
	_, err = hub.runtime.takePeerHandshake(hub.peer, id, true)
	require.ErrorIs(t, err, htlcswitch.ErrLinkShuttingDown)
	require.True(t, hub.runtime.claimExpectedReestablish(id))
	require.NoError(t, hub.runtime.RemovePeerLinks(hub.peer))
}

// TestSharedEngineRejectsMissingResolutionOwner verifies pending-close rows
// cannot start resolution without the durable Ark lifecycle owner available.
func TestSharedEngineRejectsMissingResolutionOwner(t *testing.T) {
	t.Parallel()

	hub := newFundingFlowNode(t, arkchannel.PartyHub)
	client := newFundingFlowNode(t, arkchannel.PartyClient)
	connectFundingFlowNodes(t, hub, client)
	require.NoError(t, hub.runtime.Start())
	require.NoError(t, client.runtime.Start())
	t.Cleanup(func() {
		require.NoError(t, hub.runtime.Stop())
		require.NoError(t, client.runtime.Stop())
	})
	record := fundingIntentRecord(
		t, hub, client, lndfunding.PendingChanID{8, 8},
	)
	flow := activateFundingFlowChannel(t, hub, client, record)
	group := &NativeNodeGroup{
		db:    hub.db,
		nodes: make(map[[33]byte]*NativeNode),
	}
	require.NoError(t, group.validateOwnership())
	_, err := group.ChainWatchOwner(t.Context(), flow.hubChannel)
	require.ErrorIs(t, err, contractcourt.ErrAuxChannelOwnerUnavailable)
	group.nodes[hub.peer.PubKey()] = &NativeNode{peer: hub.peer}
	require.NoError(t, group.validateOwnership())
	hub.runtime.RemoveLink(flow.hubChannel.FundingOutpoint)
	require.NoError(
		t,
		flow.hubChannel.CloseChannel(
			&channeldb.ChannelCloseSummary{
				ChanPoint: flow.hubChannel.FundingOutpoint,
				RemotePub: client.key.PubKey(),
				CloseType: channeldb.LocalForceClose,
				IsPending: true,
			},
		),
	)
	delete(group.nodes, hub.peer.PubKey())
	require.NoError(t, group.validateOwnership())
	_, err = group.channelNode(flow.hubChannel.FundingOutpoint)
	require.ErrorContains(t, err, "is not registered")
}

// TestQuiesceChannelTracksLinkLifetime verifies a real held HTLC keeps the
// flush hook pending, but link replacement or engine shutdown still cancels
// the drain promptly, without enabling the replacement link.
func TestQuiesceChannelTracksLinkLifetime(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"replacement", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			hub := newFundingFlowNode(t, arkchannel.PartyHub)
			client := newFundingFlowNode(t, arkchannel.PartyClient)
			connectFundingFlowNodes(t, hub, client)
			require.NoError(t, hub.runtime.Start())
			require.NoError(t, client.runtime.Start())
			t.Cleanup(func() {
				require.NoError(t, hub.runtime.Stop())
				require.NoError(t, client.runtime.Stop())
			})
			record := fundingIntentRecord(
				t, hub, client,
				lndfunding.PendingChanID{7, 5, 9},
			)
			flow := activateFundingFlowChannel(
				t, hub, client, record,
			)
			// Return liquidity to the hub before it offers the held
			// HTLC.
			payRuntimeInvoice(
				t, client, hub, lnwire.MilliSatoshi(20_000_000),
				lntypes.Preimage{7, 5, 8},
			)
			hubNode := &NativeNode{
				runtime: hub.runtime, peer: hub.peer,
				cfg: NativeNodeConfig{
					Party: arkchannel.PartyHub,
					Chain: fixedHeightChain{
						height: 800_000,
					},
					IdentityKey: keychain.KeyDescriptor{
						PubKey: hub.key.PubKey(),
					},
				},
			}
			clientNode := &NativeNode{runtime: client.runtime}
			preimage := lntypes.Preimage{7, 5, 9}
			require.NoError(
				t,
				clientNode.AddHoldInvoice(
					t.Context(), preimage.Hash(), 10_000,
				),
			)
			ctx, cancel := context.WithTimeout(
				t.Context(), 10*time.Second,
			)
			defer cancel()
			payment := make(chan error, 1)
			go func() {
				_, err := hubNode.PayInvoiceResult(
					ctx, flow.hubSink.record,
					preimage.Hash(), 10_000,
				)
				payment <- err
			}()
			_, err := clientNode.WaitInvoiceAcceptedChannel(
				ctx, preimage.Hash(),
			)
			require.NoError(t, err)
			point := flow.hubChannel.FundingOutpoint
			link, err := hub.runtime.GetLink(
				flow.hubChannel.ShortChanID(),
			)
			require.NoError(t, err)
			drained := make(chan error, 1)
			go func() {
				_, err := hub.runtime.QuiesceChannel(ctx, point)
				drained <- err
			}()
			require.Eventually(t, func() bool {
				return link.IsFlushing(
					htlcswitch.Incoming,
				) && link.IsFlushing(
					htlcswitch.Outgoing,
				)
			}, time.Second, time.Millisecond)
			// The native HTLC manager has accepted a flush
			// registration while the HTLC remains held; neither
			// callback can fire.
			require.NoError(t, link.OnFlushedOnce(ctx, func() {}))
			if mode == "shutdown" {
				require.NoError(t, hub.runtime.Stop())
			} else {
				hub.runtime.RemoveLink(point)
				state, err := hub.db.
					ChannelStateDB().
					FetchChannel(point)
				require.NoError(t, err)
				cfg := testLinkConfig(hub.peer, hub.failures)
				cfg.SyncStates, cfg.AddsDisabled = true, true
				_, err = hub.runtime.AddLink(state, cfg)
				require.NoError(t, err)
			}
			select {
			case err := <-drained:
				require.ErrorIs(
					t, err, htlcswitch.ErrLinkShuttingDown,
				)

			case <-time.After(time.Second):
				t.Fatal(
					"link lifetime change stranded its " +
						"flush hook",
				)
			}
			require.NoError(t, ctx.Err())
			if mode == "replacement" {
				replacement, err := hub.runtime.GetLink(
					flow.hubChannel.ShortChanID(),
				)
				require.NoError(t, err)
				require.True(
					t, replacement.IsFlushing(
						htlcswitch.Incoming,
					),
				)
				require.True(
					t, replacement.IsFlushing(
						htlcswitch.Outgoing,
					),
				)
			}
			cancel()
			require.NoError(t, hub.runtime.Stop())
			select {
			case err := <-payment:
				require.Error(t, err)

			case <-time.After(time.Second):
				t.Fatal(
					"payment caller did not stop after " +
						"cancellation",
				)
			}
		})
	}
}
