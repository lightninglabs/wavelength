package waved

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

type arkChannelResumeExecutor func(context.Context, arkchannel.ID,
	arkchannel.Action) error

// Execute routes durable actions into a controlled materializer.
func (e arkChannelResumeExecutor) Execute(ctx context.Context, id arkchannel.ID,
	action arkchannel.Action) error {

	return e(ctx, id, action)
}

type arkChannelResumeForceCloser chan wire.OutPoint

// ResumeForceCloseChannel records the post-materialization native handoff.
func (c arkChannelResumeForceCloser) ResumeForceCloseChannel(
	point wire.OutPoint) error {

	c <- point

	return nil
}

type arkChannelResumeInventory struct {
	arkchannel.Store
	records []arkchannel.Record
	err     error
}

// ListNonTerminal injects enumeration and validation failures at bootstrap.
func (s arkChannelResumeInventory) ListNonTerminal(context.Context) (
	[]arkchannel.Record, error) {

	return s.records, s.err
}

// TestNativeArkChannelResumeLeavesMaterializationPending forces the durable
// restart window before any future blocks can complete materialization.
func TestNativeArkChannelResumeLeavesMaterializationPending(t *testing.T) {
	t.Parallel()

	raw, coordinator := testArkChannelResumeStore(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	initial := testArkChannelResumeRecord(t, coordinator, true)
	id := initial.Snapshot.Terms.ID
	started := make(chan struct{})
	release := make(chan struct{})
	forced := make(chan struct{}, 1)
	var service *arkchannel.Service
	executor := arkChannelResumeExecutor(func(ctx context.Context,
		_ arkchannel.ID, action arkchannel.Action) error {

		switch action.(type) {
		case *arkchannel.PublishChannel:
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := service.ApplyLocalEvent(ctx, id,
				&arkchannel.BackingPublished{
					TxID: initial.
						Snapshot.
						Backing.
						ChannelPoint.
						Hash,
				},
			)

			return err

		case *arkchannel.ForceCloseChannel:
			forced <- struct{}{}

			return nil

		default:
			return fmt.Errorf("unexpected recovery action %T",
				action)
		}
	})
	var err error
	service, err = arkchannel.NewService(
		arkchannel.PartyHub, coordinator, executor,
	)
	require.NoError(t, err)
	controller := testArkChannelResumeController(t)
	resumed := make(arkChannelResumeForceCloser, 1)
	controller.clientStarter = func(ctx context.Context) error {
		ids, err := nativeArkChannelResumeIDs(
			ctx, service,
		)
		if err != nil {
			return err
		}
		//nolint:contextcheck // recovery outlives the startup context
		controller.startNativeArkChannelResume(service, resumed, ids)

		return nil
	}
	requestCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startup := make(chan error, 1)
	go func() {
		startup <- controller.ensureClientStarted(requestCtx)
	}()
	select {
	case err := <-startup:
		require.NoError(t, err)

	case <-time.After(5 * time.Second):
		t.Fatal("startup waited for block-dependent materialization")
	}
	<-started
	cancel()
	actual, err := coordinator.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, initial, actual)
	close(release)
	select {
	case <-forced:
	case <-time.After(5 * time.Second):
		t.Fatal("materialization did not resume force close")
	}
	require.Equal(t, initial.Snapshot.Backing.ChannelPoint, <-resumed)
	actual, err = coordinator.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseOnChain, actual.Snapshot.Phase)
	require.True(t, actual.Snapshot.BackingPublished)
}

// TestNativeArkChannelResumeStopJoinsAndRestarts proves canceled recovery is
// joined before storage closes and remains replayable from the reopened DB.
func TestNativeArkChannelResumeStopJoinsAndRestarts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	raw, coordinator := testArkChannelResumeStore(t, dir)
	initial := testArkChannelResumeRecord(t, coordinator, true)
	started := make(chan struct{})
	canceled := make(chan struct{})
	allowExit := make(chan struct{})
	releaseExit := sync.OnceFunc(func() { close(allowExit) })
	controller := testArkChannelResumeController(t)
	t.Cleanup(releaseExit)
	service, err := arkchannel.NewService(
		arkchannel.PartyHub, coordinator,
		arkChannelResumeExecutor(func(ctx context.Context,
			_ arkchannel.ID, action arkchannel.Action) error {

			if _, ok := action.(*arkchannel.PublishChannel); !ok {
				return fmt.Errorf("unexpected action %T",
					action)
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-allowExit

			return ctx.Err()
		}),
	)
	require.NoError(t, err)
	ids, err := nativeArkChannelResumeIDs(
		t.Context(), service,
	)
	require.NoError(t, err)
	controller.startNativeArkChannelResume(service, nil, ids)
	<-started
	stopped := make(chan error, 1)
	go func() {
		stopped <- controller.Stop()
	}()
	<-canceled
	select {
	case err := <-stopped:
		require.NoError(t, err)
		t.Fatal("Stop returned before its recovery worker exited")

	default:
	}
	releaseExit()
	require.NoError(t, <-stopped)
	actual, err := coordinator.Get(t.Context(), initial.Snapshot.Terms.ID)
	require.NoError(t, err)
	require.Equal(t, initial, actual)
	require.NoError(t, raw.Close())

	raw, coordinator = testArkChannelResumeStore(t, dir)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	controller = testArkChannelResumeController(t)
	var attempts atomic.Int32
	service, err = arkchannel.NewService(
		arkchannel.PartyHub, coordinator,
		arkChannelResumeExecutor(func(ctx context.Context,
			id arkchannel.ID, action arkchannel.Action) error {

			switch action.(type) {
			case *arkchannel.PublishChannel:
				if attempts.Add(1) == 1 {
					return errors.New("materializer " +
						"temporarily unavailable")
				}
				_, _, err := coordinator.Apply(ctx, id,
					&arkchannel.BackingPublished{
						TxID: initial.
							Snapshot.
							Backing.
							ChannelPoint.
							Hash,
					},
				)

				return err

			default:
				return fmt.Errorf("unexpected action %T",
					action)
			}
		}),
	)
	require.NoError(t, err)
	ids, err = nativeArkChannelResumeIDs(
		t.Context(), service,
	)
	require.NoError(t, err)
	resumed := make(arkChannelResumeForceCloser, 1)
	controller.startNativeArkChannelResume(service, resumed, ids)
	select {
	case point := <-resumed:
		require.Equal(t, initial.Snapshot.Backing.ChannelPoint, point)

	case <-time.After(5 * time.Second):
		t.Fatal("durable recovery did not retry after restart")
	}
	require.Equal(t, int32(2), attempts.Load())
}

// TestNativeArkChannelResumeReloadsAfterValidation forces a watch transition
// after validation so a stale active snapshot cannot select synchronous replay.
func TestNativeArkChannelResumeReloadsAfterValidation(t *testing.T) {
	t.Parallel()

	raw, coordinator := testArkChannelResumeStore(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	initial := testArkChannelResumeRecord(t, coordinator, false)
	id := initial.Snapshot.Terms.ID
	started := make(chan struct{})
	service, err := arkchannel.NewService(
		arkchannel.PartyHub, coordinator,
		arkChannelResumeExecutor(func(ctx context.Context,
			_ arkchannel.ID, action arkchannel.Action) error {

			if _, ok := action.(*arkchannel.PublishChannel); !ok {
				return fmt.Errorf("unexpected action %T",
					action)
			}
			close(started)
			<-ctx.Done()

			return ctx.Err()
		}),
	)
	require.NoError(t, err)
	ids, err := nativeArkChannelResumeIDs(t.Context(), service)
	require.NoError(t, err)
	require.Equal(t, []arkchannel.ID{id}, ids)
	// A confirmed recovery watch fires after the active inventory was read.
	materializing, _, err := coordinator.Apply(t.Context(), id,
		&arkchannel.SourceSpent{
			OutPoint:     initial.Snapshot.Source.OutPoint,
			SpendingTxID: [32]byte{99},
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, arkchannel.PhaseMaterializing, materializing.Snapshot.Phase,
	)
	controller := testArkChannelResumeController(t)
	controller.startNativeArkChannelResume(service, nil, ids)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal(
			"recovery did not reload the newly materializing " +
				"record",
		)
	}
	actual, err := coordinator.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, materializing, actual)
}

// TestNativeArkChannelResumeRejectsBootstrapFailures verifies that moving the
// materializer off startup does not defer inventory or record validation.
func TestNativeArkChannelResumeRejectsBootstrapFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		store arkChannelResumeInventory
		want  string
	}{
		{
			name: "enumeration",
			store: arkChannelResumeInventory{
				err: errors.New("inventory unavailable"),
			},
			want: "inventory unavailable",
		},
		{
			name: "invalid durable record",
			store: arkChannelResumeInventory{
				records: []arkchannel.Record{
					{},
				},
			},
			want: "validate channel recovery",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator, err := arkchannel.NewCoordinator(
				test.store,
			)
			require.NoError(t, err)
			var executions atomic.Int32
			service, err := arkchannel.NewService(
				arkchannel.PartyHub, coordinator,
				arkChannelResumeExecutor(func(context.Context,
					arkchannel.ID,
					arkchannel.Action) error {

					executions.Add(1)

					return nil
				}),
			)
			require.NoError(t, err)
			ids, err := nativeArkChannelResumeIDs(
				t.Context(), service,
			)
			require.ErrorContains(t, err, test.want)
			require.Empty(t, ids)
			require.Zero(t, executions.Load())
		})
	}
}

// testArkChannelResumeController installs the real owned-worker lifecycle.
func testArkChannelResumeController(t *testing.T) *NativeArkChannelController {
	t.Helper()
	controller := &NativeArkChannelController{
		cfg: withArkChannelControllerDefaults(
			ArkChannelControllerConfig{
				Log: btclog.Disabled,
			},
		),
	}
	controller.initLifecycle(t.Context())
	t.Cleanup(func() { require.NoError(t, controller.Stop()) })

	return controller
}

// testArkChannelResumeStore opens the production SQL-backed channel store.
func testArkChannelResumeStore(t *testing.T,
	dir string) (*db.Store, *arkchannel.Coordinator) {

	t.Helper()
	raw, err := db.NewStoreFromConfig(
		db.DefaultConfig(dir), btclog.Disabled,
	)
	require.NoError(t, err)
	coordinator, err := arkchannel.NewCoordinator(
		raw.NewArkChannelStore(
			clock.NewDefaultClock(),
		),
	)
	require.NoError(t, err)

	return raw, coordinator
}

// testArkChannelResumeRecord persists a fully signed channel, optionally with
// a confirmed source-conflict trigger, without executing its pending effect.
func testArkChannelResumeRecord(t *testing.T,
	coordinator *arkchannel.Coordinator,
	materializing bool) arkchannel.Record {

	t.Helper()
	terms := testPrePONRTerms(t)
	keys := make([]*btcec.PrivateKey, 2)
	for i := range keys {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		keys[i] = key
	}
	copy(
		terms.VTXO.ClientChannelKey[:],
		keys[0].PubKey().SerializeCompressed(),
	)
	copy(
		terms.VTXO.HubChannelKey[:],
		keys[1].PubKey().SerializeCompressed(),
	)
	binding := testPrePONRBinding(t, terms)
	packet, err := psbt.New(nil, []*wire.TxOut{{
		Value: int64(terms.Capacity), PkScript: []byte{0x51, 0x20, 1},
	}}, 2, 0, nil)
	require.NoError(t, err)
	template, err := arkchannel.NewBackingTemplate(packet, terms, binding)
	require.NoError(t, err)
	sign := func(party arkchannel.Party,
		key *btcec.PrivateKey) input.Signature {

		desc, err := template.SignDescriptor(
			terms, party, keychain.KeyDescriptor{
				PubKey: key.PubKey(),
			},
		)
		require.NoError(t, err)
		sig, err := input.NewMockSigner([]*btcec.PrivateKey{key}, nil).
			SignOutputRaw(template.Packet().UnsignedTx, desc)
		require.NoError(t, err)

		return sig
	}
	backing, err := template.Complete(
		terms, binding, sign(arkchannel.PartyClient, keys[0]),
		sign(arkchannel.PartyHub, keys[1]),
	)
	require.NoError(t, err)
	_, err = coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	var record arkchannel.Record
	for _, event := range []arkchannel.Event{
		&arkchannel.BindVTXO{
			Binding: binding,
		},
		&arkchannel.FundingPeerReady{},
		&arkchannel.BackingSigned{
			Backing: backing,
		},
		&arkchannel.FundingFinalized{
			Party: arkchannel.PartyClient,
		},
		&arkchannel.FundingFinalized{
			Party: arkchannel.PartyHub,
		},
		&arkchannel.OORFinalized{
			SessionID: binding.OORSessionID,
		},
		&arkchannel.RecoveryPackageInstalled{
			Party: arkchannel.PartyClient,
		},
		&arkchannel.RecoveryPackageInstalled{
			Party: arkchannel.PartyHub,
		},
		&arkchannel.ChannelActive{
			ChannelPointHash:  backing.ChannelPoint.Hash,
			ChannelPointIndex: backing.ChannelPoint.Index,
		},
	} {
		record, _, err = coordinator.Apply(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}
	require.Equal(t, arkchannel.PhaseActive, record.Snapshot.Phase)
	if materializing {
		record, _, err = coordinator.Apply(
			t.Context(), terms.ID, &arkchannel.SourceSpent{
				OutPoint:     binding.OutPoint,
				SpendingTxID: [32]byte{99},
			},
		)
		require.NoError(t, err)
		require.Equal(
			t, arkchannel.PhaseMaterializing, record.Snapshot.Phase,
		)
	}

	return record
}
