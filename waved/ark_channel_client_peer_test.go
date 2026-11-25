package waved

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/chainbackends"
	"github.com/lightninglabs/wavelength/chainfees"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type startupFundingPeer struct {
	lnruntime.ProcessFundingPeer
	info    lnruntime.FundingPeerInfo
	err     error
	lookups atomic.Int32
}

// GetPeerInfo counts authenticated policy reads independently from replay.
func (p *startupFundingPeer) GetPeerInfo(context.Context) (
	lnruntime.FundingPeerInfo, error) {

	p.lookups.Add(1)

	return p.info, p.err
}

// BindPreparedOOR leaves one outstanding preparation waiting for the peer.
func (*startupFundingPeer) BindPreparedOOR(ctx context.Context, _ arkchannel.ID,
	_ arkchannel.VTXOBinding) (arkchannel.Record, error) {

	<-ctx.Done()

	return arkchannel.Record{}, ctx.Err()
}

// TestClientStartupPeer pins recovery to the local wallet and one hub. Only
// an empty (not unreadable or inconsistent) inventory permits first use.
func TestClientStartupPeer(t *testing.T) {
	t.Parallel()
	c := &NativeArkChannelController{
		cfg: ArkChannelControllerConfig{
			IdentityKey: testKeyDescriptor(t, 1),
		},
		keys: nativeArkChannelKeys{
			ark:     testKeyDescriptor(t, 2),
			backing: testKeyDescriptor(t, 3),
			funder:  testKeyDescriptor(t, 4),
		},
	}
	first := testPrePONRTerms(t)
	info := fundingPeerFromTerms(first)
	first, err := c.newClientFundedTerms(
		first.ID, first.PendingChannelID, first.ReservedSCID,
		first.Capacity, info,
	)
	require.NoError(t, err)
	second := first
	second.ID[0]++
	second.VTXO.FunderDelay++
	foreign := first
	foreign.ClientNodeKey = testPrePONRTerms(t).ClientNodeKey
	otherHub := first
	otherHub.HubNodeKey = testPrePONRTerms(t).HubNodeKey
	unavailable := errors.New("peer unavailable")
	for _, tc := range []struct {
		name              string
		rows              []arkchannel.Terms
		storeErr, peerErr error
		calls             int32
		delta             uint32
		wantErr           string
	}{
		{
			name:  "first use",
			calls: 1,
			delta: info.FunderDelay,
		},
		{
			name:    "offline first use",
			calls:   1,
			peerErr: unavailable,
			wantErr: "peer unavailable",
		},
		{
			name: "offline recovery",
			rows: []arkchannel.Terms{
				second,
				first,
			},
			peerErr: unavailable,
			delta:   second.VTXO.FunderDelay,
		},
		{
			name:     "inventory unavailable",
			storeErr: errors.New("store unavailable"),
			wantErr:  "store unavailable",
		},
		{
			name: "different wallet",
			rows: []arkchannel.Terms{
				foreign,
			},
			wantErr: "local wallet",
		},
		{
			name: "different hubs",
			rows: []arkchannel.Terms{
				first,
				otherHub,
			},
			wantErr: "different hub",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inventory := arkChannelResumeInventory{err: tc.storeErr}
			for _, terms := range tc.rows {
				inventory.records = append(
					inventory.records, arkchannel.Record{
						Snapshot: arkchannel.Snapshot{
							Terms: terms,
						},
					},
				)
			}
			remote := &startupFundingPeer{
				info: info,
				err:  tc.peerErr,
			}
			c.remote = remote
			c.coordinator, err = arkchannel.NewCoordinator(
				inventory,
			)
			require.NoError(t, err)
			peer, err := c.clientStartupPeer(t.Context())
			require.Equal(t, tc.calls, remote.lookups.Load())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)

				return
			}
			require.NoError(t, err)
			require.Equal(t, info.HubNodeKey, peer.nodeKey)
			require.Equal(t, tc.delta, peer.materializationDelta)
			if len(tc.rows) == 0 {
				// A fresh process must not reuse its startup
				// policy for a later promotion after the hub
				// rotates keys.
				raw, coordinator := testArkChannelResumeStore(
					t, t.TempDir(),
				)
				t.Cleanup(
					func() {
						require.NoError(t, raw.Close())
					},
				)
				c.coordinator, c.clientPeer = coordinator, peer
				for _, key := range []string{
					"first",
					"second",
				} {
					vtxo := testPrePONRTerms(t).VTXO
					remote.info.HubArkKey = vtxo.HubArkKey
					actual, old, err := c.newPromotionTerms(
						t.Context(), first.Capacity,
						key,
					)
					require.NoError(t, err)
					require.Nil(t, old)
					require.Equal(
						t, remote.info,
						fundingPeerFromTerms(actual),
					)
				}
				require.EqualValues(t, 3, remote.lookups.Load())

				return
			}
			_, err = peer.promotionPolicy(t.Context(), remote)
			require.ErrorIs(t, err, unavailable)
			remote.err = nil
			_, err = peer.promotionPolicy(t.Context(), remote)
			require.ErrorContains(
				t, err, "running channel endpoint",
			)
			remote.info.FunderDelay = tc.delta
			actual, err := peer.promotionPolicy(t.Context(), remote)
			require.NoError(t, err)
			require.Equal(t, remote.info, actual)
		})
	}
}

type startupRecovery struct {
	ArkChannelRecoveryController
	watches chan []arkchannel.Record
	checked chan arkchannel.ID
}

// BindChannelEventSink accepts the real local service's lifecycle binding.
func (*startupRecovery) BindChannelEventSink(
	arkchannel.ChannelEventSink) error {

	return nil
}

// RestoreWatches records the inventory handed to recovery after node start.
func (r *startupRecovery) RestoreWatches(_ context.Context,
	rows []arkchannel.Record) error {

	r.watches <- rows

	return nil
}

// Stop leaves the fixture owned by its test.
func (*startupRecovery) Stop() {}

// SourceExpiring records the guard's startup check without forcing a close.
func (r *startupRecovery) SourceExpiring(ctx context.Context,
	terms arkchannel.Terms, _ arkchannel.VTXOBinding, _ int32) (bool,
	error) {

	select {
	case r.checked <- terms.ID:
	case <-ctx.Done():
		return false, ctx.Err()
	}

	return false, nil
}

type startupSender struct{}

// SendPeerEvent leaves the remote link unavailable.
func (startupSender) SendPeerEvent(context.Context, lnruntime.PeerEvent) error {
	return errors.New("peer unavailable")
}

// TestHubStartupRecoveryDelta checks the actual arbitrator configuration at
// construction. The advertised offer is independent of the shared floor.
func TestHubStartupRecoveryDelta(t *testing.T) {
	t.Parallel()
	terms := testPrePONRTerms(t)
	store := arkChannelResumeInventory{records: []arkchannel.Record{
		{
			Snapshot: arkchannel.Snapshot{
				Terms: terms,
			},
		},
	}}
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)
	stop := errors.New("inspected node configuration")
	c := &NativeArkChannelController{
		coordinator: coordinator,
		cfg: ArkChannelControllerConfig{
			IdentityKey: testKeyDescriptor(t, 9),
			Wallet:      &lwwallet.Wallet{},
		},
		peerInfo: fundingPeerFromTerms(terms),
	}
	for _, tc := range []struct{ offer, shared, want uint32 }{
		{
			575,
			0,
			576,
		}, {
			575,
			600,
			600,
		}, {
			601,
			600,
			601,
		},
	} {
		c.peerInfo.FunderDelay = tc.offer
		c.nodeFactory = func(cfg lnruntime.NativeNodeConfig) (
			*lnruntime.NativeNode, error) {

			require.Equal(t, tc.want, cfg.MaterializationDelta)

			return nil, stop
		}
		err := c.startHub(
			t.Context(), terms.ClientNodeKey, startupSender{}, nil,
			nil, tc.shared,
		)
		require.ErrorIs(t, err, stop)
		require.Equal(t, tc.offer, c.peerInfo.FunderDelay)
	}
}

// TestClientStartsNativeRecoveryOffline runs local LND and both maintenance
// workers from reopened SQL state. The unfinished promotion cannot complete
// its peer binding, and no new RPC or inbound peer message triggers startup.
func TestClientStartsNativeRecoveryOffline(t *testing.T) {
	wallet := newFundedLwWallet(t)
	dir := t.TempDir()
	raw, coordinator := testArkChannelResumeStore(t, dir)
	identity, err := wallet.DeriveKey(t.Context(), keychain.KeyLocator{
		Family: keychain.KeyFamilyNodeKey,
	})
	require.NoError(t, err)
	cfg := withArkChannelControllerDefaults(
		ArkChannelControllerConfig{
			Wallet:         wallet,
			ChainBackend:   wallet.ChainBackend(),
			NetParams:      &chaincfg.RegressionNetParams,
			IdentityKey:    *identity,
			OORDestination: identity.PubKey,
			ChannelDataDir: t.TempDir(),
			OperatorTermsSource: func() *types.OperatorTerms {
				return nil
			},
			OOR:          &oorbridge.Controller{},
			Materializer: &unrollbridge.Controller{},
			Peer:         &lnruntime.MailboxCooperativeClosePeer{},
			PeerSender:   startupSender{},
		},
	)
	cfg.ChainNotifier, err = chainbackends.NewBackendChainNotifier(
		cfg.ChainBackend,
	)
	require.NoError(t, err)
	cfg.FeeEstimator, err = chainfees.NewBackendEstimator(
		cfg.ChainBackend, chainfee.FeePerKwFloor,
	)
	require.NoError(t, err)
	keys, err := deriveNativeArkChannelKeys(
		t.Context(), cfg, arkchannel.PartyClient,
	)
	require.NoError(t, err)
	recovery := &startupRecovery{
		watches: make(chan []arkchannel.Record, 1),
		checked: make(chan arkchannel.ID, 1),
	}
	cfg.Recovery = recovery
	remote := &startupFundingPeer{err: errors.New("peer unavailable")}
	c := &NativeArkChannelController{
		cfg: cfg, party: arkchannel.PartyClient, keys: keys,
		remote: remote, fundingPeer: &lnruntime.MailboxFundingPeer{},
		prePONROOR: cfg.OOR,
	}
	template := testPrePONRTerms(t)
	const pendingKey = "pending promotion"
	id, pending, scid := c.promotionIdentifiers(pendingKey)
	terms, err := c.newClientFundedTerms(
		id, pending, scid, template.Capacity,
		fundingPeerFromTerms(template),
	)
	require.NoError(t, err)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	initial, _, err := coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.BindVTXO{
			Binding: testPrePONRBinding(t, terms),
		},
	)
	require.NoError(t, err)
	clientKey, err := wallet.KeyRing().DerivePrivKey(keys.backing)
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	copy(
		template.VTXO.HubChannelKey[:],
		hubKey.PubKey().SerializeCompressed(),
	)
	id, pending, scid = c.promotionIdentifiers("active promotion")
	activeTerms, err := c.newClientFundedTerms(
		id, pending, scid, template.Capacity,
		fundingPeerFromTerms(template),
	)
	require.NoError(t, err)
	active := persistArkChannelResumeRecord(
		t, coordinator, activeTerms, clientKey, hubKey, false,
	)
	require.NoError(t, raw.Close())
	raw, c.coordinator = testArkChannelResumeStore(t, dir)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	c.initLifecycle(t.Context())
	t.Cleanup(func() { require.NoError(t, c.Stop()) })
	c.Start()
	select {
	case rows := <-recovery.watches:
		require.ElementsMatch(
			t, []arkchannel.Record{initial, active}, rows,
		)

	case <-time.After(8 * time.Second):
		t.Fatal("local LND did not reach watch restoration")
	}
	select {
	case checked := <-recovery.checked:
		require.Equal(t, activeTerms.ID, checked)

	case <-time.After(3 * time.Second):
		t.Fatal("source expiry guard did not start")
	}
	require.NoError(t, c.ensureClientStarted(t.Context()))
	restored, record, err := c.newPromotionTerms(
		t.Context(), terms.Capacity, pendingKey,
	)
	require.NoError(t, err)
	require.Equal(t, terms, restored)
	require.Equal(t, initial, *record)
	_, _, err = c.newPromotionTerms(
		t.Context(), terms.Capacity+1, pendingKey,
	)
	require.ErrorContains(t, err, "different channel terms")
	require.Zero(t, remote.lookups.Load())
}
