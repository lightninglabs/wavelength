package lnruntime

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/clock"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/stretchr/testify/require"
)

// TestFundingCancelRestart reopens an aborted channel with pending cleanup
// but no installed backing, using a fresh wallet and the native resume path.
func TestFundingCancelRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	terms, source, _, _ := statusPollChannel(t)
	path := filepath.Join(t.TempDir(), "cancel.db")
	open := func() (*arkchannel.Coordinator, func() error) {
		raw := clientdb.NewTestDBHandleFromPath(t, path)
		db := clientdb.NewStore(
			raw.DB, raw.Queries, raw.Backend(), btclog.Disabled,
		)
		store := db.NewArkChannelStore(clock.NewDefaultClock())
		c, err := arkchannel.NewCoordinator(store)
		require.NoError(t, err)

		return c, raw.DB.Close
	}
	c, closeDB := open()
	_, err := c.Request(ctx, terms)
	require.NoError(t, err)
	_, _, err = c.Apply(
		ctx, terms.ID, &arkchannel.BindVTXO{
			Binding: source,
		},
	)
	require.NoError(t, err)
	before, _, err := c.Apply(ctx, terms.ID, &arkchannel.OORAborted{
		SessionID: source.OORSessionID, Reason: "funding cancelled",
	})
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseCancelling, before.Snapshot.Phase)
	require.Nil(t, before.Snapshot.Backing)
	require.NoError(t, closeDB())

	c, _ = open()
	wallet := &lnwallet.LightningWallet{}
	native := &FundingRuntime{wallet: wallet}
	endpoint := &NativeFundingEndpoint{
		party:   arkchannel.PartyHub,
		funding: native,
	}
	edges := &statusPollNativeEdges{}
	executor, err := arkchannel.NewNativeExecutor(
		arkchannel.PartyHub, edges, endpoint, nil, nil, edges, edges,
		edges,
	)
	require.NoError(t, err)
	service, err := arkchannel.NewService(arkchannel.PartyHub, c, executor)
	require.NoError(t, err)
	_, err = service.ResumeChannelAction(ctx, terms.ID)

	// On older pins an untyped error must leave the action replayable.
	var evidence interface{ MissingFundingIntent() [32]byte }
	probe := wallet.CancelFundingIntent(terms.PendingChannelID)
	if !errors.As(probe, &evidence) {
		require.Error(t, err)
		record, getErr := c.Get(ctx, terms.ID)
		require.NoError(t, getErr)
		require.Equal(t, before.Revision, record.Revision)
		t.Log("legacy lnd: cleanup remains retryable")

		return
	}
	require.NoError(t, err)
	record, err := c.Get(ctx, terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
	require.Equal(t, before.Revision+1, record.Revision)
	again, err := service.ResumeChannelAction(ctx, terms.ID)
	require.NoError(t, err)
	require.Equal(t, record.Revision, again.Revision)
}

// absentCancelIntent models the wallet's ID-specific missing evidence.
type absentCancelIntent [32]byte

// Error deliberately does not carry information used by the classifier.
func (a absentCancelIntent) Error() string { return "missing" }

// MissingFundingIntent names the single absent map entry.
func (a absentCancelIntent) MissingFundingIntent() [32]byte { return a }

// TestFundingCancelEvidence requires a single, ID-matched absence cause.
func TestFundingCancelEvidence(t *testing.T) {
	t.Parallel()
	id := lndfunding.PendingChanID{1}
	missing := absentCancelIntent(id)
	other := errors.New("wallet unavailable")
	require.True(
		t,
		missingFundingIntent(
			fmt.Errorf("wallet: %w", missing), id,
		),
	)
	for _, err := range []error{
		nil, other, errors.New(missing.Error()), absentCancelIntent{
			2,
		},
		errors.Join(missing, other),
	} {
		require.False(t, missingFundingIntent(err, id))
	}
}

// cancelHistory supplies a channel database observation without starting lnd.
type cancelHistory struct {
	chanstate.Store
	openErr   error
	closedErr error
	summary   *chanstate.ChannelCloseSummary
}

// FetchChannel returns the configured lookup error.
func (h cancelHistory) FetchChannel(wire.OutPoint) (*chanstate.OpenChannel,
	error) {

	return nil, h.openErr
}

// FetchClosedChannel returns the configured close evidence.
func (h cancelHistory) FetchClosedChannel(*wire.OutPoint) (
	*chanstate.ChannelCloseSummary, error) {

	return h.summary, h.closedErr
}

// TestFundingCancelStoredEvidence proves only abandonment clears admission;
// a missing channel, unrelated close or database failure leaves it retryable.
func TestFundingCancelStoredEvidence(t *testing.T) {
	t.Parallel()
	id := lndfunding.PendingChanID{1}
	point := wire.OutPoint{Index: 1}
	transient := errors.New("temporary database failure")
	for _, tc := range []struct {
		name    string
		history cancelHistory
		wantErr error
		success bool
	}{
		{
			name: "open lookup failure",
			history: cancelHistory{
				openErr: transient,
			},
			wantErr: transient,
		},
		{
			name: "closed lookup failure",
			history: cancelHistory{
				openErr:   channeldb.ErrChannelNotFound,
				closedErr: transient,
			},
			wantErr: transient,
		},
		{
			name: "no durable disposition",
			history: cancelHistory{
				openErr:   channeldb.ErrChannelNotFound,
				closedErr: channeldb.ErrClosedChannelNotFound,
			},
		},
		{
			name: "different disposition",
			history: cancelHistory{
				openErr: channeldb.ErrChannelNotFound,
				summary: &chanstate.ChannelCloseSummary{
					ChanPoint: point,
					CloseType: channeldb.CooperativeClose,
				},
			},
		},
		{
			name: "same abandonment",
			history: cancelHistory{
				openErr: channeldb.ErrChannelNotFound,
				summary: &chanstate.ChannelCloseSummary{
					ChanPoint: point,
					CloseType: channeldb.Abandoned,
				},
			},
			success: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := map[lndfunding.PendingChanID]struct{}{id: {}}
			f := &FundingRuntime{
				stateDB:         tc.history,
				wallet:          &lnwallet.LightningWallet{},
				localPendingIDs: ids,
			}
			err := f.CancelBacking(id, &point)
			if tc.success {
				require.NoError(t, err)
				require.NotContains(t, f.localPendingIDs, id)

				return
			}
			require.Error(t, err)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
			require.Contains(t, f.localPendingIDs, id)
		})
	}
}
