package lnruntime

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// fetchOnlyTower leaves all mutating and subscription methods unavailable.
type fetchOnlyTower struct {
	routing.ControlTower
	fetch func(context.Context, lntypes.Hash) (
		paymentsdb.DBMPPayment, error)
}

// FetchPayment reads the real persisted payment in the reopened database.
func (t fetchOnlyTower) FetchPayment(ctx context.Context, hash lntypes.Hash) (
	paymentsdb.DBMPPayment, error) {

	return t.fetch(ctx, hash)
}

// TestLookupChannelPayment recovers terminal private payments after restart,
// with neither a router nor a live link, and isolates shared-tower records.
func TestLookupChannelPayment(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	hub, peer := testIntentKey(t).PubKey(), testIntentKey(t).PubKey()
	const scid = uint64(923)
	const amount = btcutil.Amount(25)
	msat := lnwire.NewMSatFromSatoshis(amount)
	rt := route.Route{
		SourcePubKey: route.NewVertex(hub), TotalAmount: msat,
		Hops: []*route.Hop{{
			PubKeyBytes: route.NewVertex(peer), ChannelID: scid,
			AmtToForward: msat, LegacyPayload: true,
		}},
	}
	success := lntypes.Preimage{1}
	failed, ongoing, noRoute := lntypes.Hash{
		2,
	}, lntypes.Hash{3}, lntypes.Hash{4}
	open := func(t *testing.T) routing.ControlTower {
		t.Helper()
		store, err := paymentsdb.NewKVStore(
			channeldb.OpenForTesting(t, path),
		)
		require.NoError(t, err)

		return routing.NewControlTower(store)
	}
	t.Run("persist", func(t *testing.T) {
		ctx, tower := t.Context(), open(t)
		for _, hash := range []lntypes.Hash{
			success.Hash(),
			failed,
			ongoing,
			noRoute,
		} {
			require.NoError(
				t,
				tower.InitPayment(
					ctx, hash,
					&paymentsdb.PaymentCreationInfo{
						PaymentIdentifier: hash,
						Value:             msat,
						CreationTime:      time.Now(),
					},
				),
			)
			if hash == noRoute {
				require.NoError(
					t, tower.FailPayment(
						ctx, hash,
						paymentsdb.FailureReasonNoRoute,
					),
				)

				continue
			}
			require.NoError(
				t,
				tower.RegisterAttempt(
					ctx, hash, &paymentsdb.HTLCAttemptInfo{
						AttemptID:   1,
						Hash:        &hash,
						Route:       rt,
						AttemptTime: time.Now(),
					},
				),
			)
		}
		_, err := tower.SettleAttempt(
			ctx, success.Hash(), 1, &paymentsdb.HTLCSettleInfo{
				Preimage:   success,
				SettleTime: time.Now(),
			},
		)
		require.NoError(t, err)
		_, err = tower.FailAttempt(
			ctx, failed, 1, &paymentsdb.HTLCFailInfo{
				Reason:   paymentsdb.HTLCFailInternal,
				FailTime: time.Now(),
			},
		)
		require.NoError(t, err)
		require.NoError(
			t, tower.FailPayment(
				ctx, failed, paymentsdb.FailureReasonNoRoute,
			),
		)
	})

	node := &NativeNode{
		cfg: NativeNodeConfig{Party: arkchannel.PartyHub,
			IdentityKey: keychain.KeyDescriptor{
				PubKey: hub,
			}, RemoteNodeKey: peer},
		runtime: &Runtime{payments: &FixedRoutePayments{
			control: fetchOnlyTower{
				fetch: open(t).FetchPayment,
			},
		}},
	}
	record := arkchannel.Record{Snapshot: arkchannel.Snapshot{
		Phase: arkchannel.PhaseClosed, Backing: &arkchannel.Backing{},
		Terms: arkchannel.Terms{
			ReservedSCID:  scid,
			HubNodeKey:    route.NewVertex(hub),
			ClientNodeKey: route.NewVertex(peer),
		},
	}}
	preimage, found, err := node.LookupChannelPayment(
		t.Context(), record, success.Hash(), amount,
	)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, success, preimage)
	var terminal *TerminalPaymentError
	_, found, err = node.LookupChannelPayment(
		t.Context(), record, failed, amount,
	)
	require.ErrorAs(t, err, &terminal)
	require.False(t, found)

	for _, hash := range []lntypes.Hash{ongoing, {9}} {
		_, found, err := node.LookupChannelPayment(
			t.Context(), record, hash, amount,
		)
		require.NoError(t, err)
		require.False(t, found)
	}
	unproven := func(r arkchannel.Record, hash lntypes.Hash,
		amt btcutil.Amount) {

		t.Helper()
		p, found, err := node.LookupChannelPayment(
			t.Context(), r, hash, amt,
		)
		require.Error(t, err)
		require.NotErrorAs(t, err, &terminal)
		require.False(t, found)
		require.Equal(t, lntypes.Preimage{}, p)
	}
	unproven(record, noRoute, amount)
	other := record
	other.Snapshot.Terms.ReservedSCID++
	for _, hash := range []lntypes.Hash{success.Hash(), failed} {
		unproven(other, hash, amount)
		unproven(record, hash, amount+1)
	}
	// A different legitimate peer endpoint sharing this same tower cannot
	// recover either terminal outcome by presenting the hash.
	node.cfg.RemoteNodeKey = testIntentKey(t).PubKey()
	other = record
	other.Snapshot.Terms.ClientNodeKey = route.NewVertex(
		node.cfg.RemoteNodeKey,
	)
	unproven(other, success.Hash(), amount)
	unproven(other, failed, amount)
}
