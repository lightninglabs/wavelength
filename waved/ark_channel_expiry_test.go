package waved

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testPrePONROORController returns one configured abort result.
type testPrePONROORController struct {
	aborts int
	result oorbridge.TerminalResult
}

// ValidatePreparedOOR accepts a fixture already validated by the FSM.
func (*testPrePONROORController) ValidatePreparedOOR(context.Context,
	arkchannel.Terms, arkchannel.VTXOBinding) error {

	return nil
}

// CommitPreparedOOR is unused by maintenance tests.
func (*testPrePONROORController) CommitPreparedOOR(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding) error {

	return nil
}

// AbortPreparedOOR is unused by the result-bearing maintenance path.
func (*testPrePONROORController) AbortPreparedOOR(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding, string) error {

	return nil
}

// AbortPreparedOORResult records one definitive pre-PONR abort.
func (c *testPrePONROORController) AbortPreparedOORResult(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding, string) (
	oorbridge.TerminalResult, error) {

	c.aborts++

	return c.result, nil
}

// TestMaintainPrePONRExpiresAbsentPreparation proves an armed request releases
// its durable FSM reservation without selecting replacement wallet inputs.
func TestMaintainPrePONRExpiresAbsentPreparation(t *testing.T) {
	t.Parallel()

	now := time.Unix(10_000, 0).UTC()
	controller, coordinator, terms, closeStore := testPrePONRController(
		t, now, oorbridge.PreparationLookup{
			Status: oorbridge.PreparationAbsent,
		},
	)
	defer closeStore()

	record, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.OORPreparationStarted{},
	)
	require.NoError(t, err)
	require.True(t, record.PrePONRStartedAt.IsZero())

	err = controller.maintainPrePONRChannels(
		t.Context(), nil, now.Add(defaultArkChannelPrePONRTimeout),
	)
	require.NoError(t, err)
	record, err = coordinator.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseFailed, record.Snapshot.Phase)
	require.Equal(t, arkChannelPrePONRExpiryReason,
		record.Snapshot.Failure)
}

// TestMaintainPrePONRRecoversThenAbortsPreparedOOR proves restart maintenance
// binds the deterministic winner before recording its authoritative abort.
func TestMaintainPrePONRRecoversThenAbortsPreparedOOR(t *testing.T) {
	t.Parallel()

	now := time.Unix(20_000, 0).UTC()
	terms := testPrePONRTerms(t)
	binding := testPrePONRBinding(t, terms)
	controller, coordinator, _, closeStore := testPrePONRController(
		t, now, oorbridge.PreparationLookup{
			Status: oorbridge.PreparationPrepared, Binding: binding,
		},
	)
	defer closeStore()
	resultController, ok :=
		controller.cfg.FundingOOR.(*testPrePONROORController)
	require.True(t, ok)

	_, err := coordinator.Request(t.Context(), terms)
	require.NoError(t, err)
	_, _, err = coordinator.Apply(
		t.Context(), terms.ID, &arkchannel.OORPreparationStarted{},
	)
	require.NoError(t, err)

	err = controller.maintainPrePONRChannels(
		t.Context(), nil, now.Add(defaultArkChannelPrePONRTimeout),
	)
	require.NoError(t, err)
	record, err := coordinator.Get(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseCancelling, record.Snapshot.Phase)
	require.NotNil(t, record.Snapshot.Source)
	require.True(t, record.Snapshot.OORAborted)
	require.Equal(t, 1, resultController.aborts)
}

// testPrePONRController constructs a SQL-backed coordinator at a stable time.
func testPrePONRController(t *testing.T, now time.Time,
	lookup oorbridge.PreparationLookup) (*NativeArkChannelController,
	*arkchannel.Coordinator, arkchannel.Terms, func()) {

	t.Helper()
	raw, err := db.NewStoreFromConfig(
		db.DefaultConfig(
			t.TempDir(),
		),
		btclog.Disabled,
	)
	require.NoError(t, err)
	testClock := clock.NewTestClock(now)
	channelStore := raw.NewArkChannelStore(testClock)
	coordinator, err := arkchannel.NewCoordinator(channelStore)
	require.NoError(t, err)
	resultController := &testPrePONROORController{
		result: oorbridge.TerminalResult{
			Aborted: true, Reason: arkChannelPrePONRExpiryReason,
		},
	}
	controller := &NativeArkChannelController{
		party: arkchannel.PartyHub,
		cfg: ArkChannelControllerConfig{
			FundingOOR: resultController,
			LookupOOR: func(context.Context, arkchannel.Terms,
				btcutil.Amount) (oorbridge.PreparationLookup,
				error) {

				return lookup, nil
			},
			Clock: testClock, Log: btclog.Disabled,
		},
		coordinator: coordinator,
	}

	return controller, coordinator, testPrePONRTerms(t), func() {
		require.NoError(t, raw.Close())
	}
}

// testPrePONRTerms creates valid hub-funded receive terms.
func testPrePONRTerms(t *testing.T) arkchannel.Terms {
	t.Helper()

	newKey := func() [33]byte {
		privateKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		var key [33]byte
		copy(key[:], privateKey.PubKey().SerializeCompressed())

		return key
	}

	return arkchannel.Terms{
		ID: arkchannel.ID{
			1,
			2,
			3,
		}, Kind: arkchannel.KindReceiveIntent,
		Funder: arkchannel.PartyHub,
		PendingChannelID: [32]byte{
			4,
			5,
			6,
		},
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_000, TxIndex: 1,
		}.ToUint64(),
		Capacity:      100_000,
		ClientNodeKey: newKey(),
		HubNodeKey:    newKey(),
		PaymentHash: [32]byte{
			9,
			9,
			9,
		},
		VTXO: arkchannel.VTXOTerms{
			ClientArkKey: newKey(), HubArkKey: newKey(),
			ArkOperatorKey: newKey(), ClientChannelKey: newKey(),
			HubChannelKey: newKey(), FunderKey: newKey(),
			ChannelDelay: 144, FunderDelay: 576, MinExitDelay: 144,
		},
	}
}

// testPrePONRBinding creates the exact prepared channel-policy output.
func testPrePONRBinding(t *testing.T,
	terms arkchannel.Terms) arkchannel.VTXOBinding {

	t.Helper()
	policy, pkScript, err := terms.VTXO.Artifacts()
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
		Hash: chainhash.Hash{10},
	}})
	tx.AddTxOut(&wire.TxOut{Value: int64(
		terms.Capacity + arkchannel.DefaultBackingFee,
	), PkScript: pkScript})
	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))
	sessionID := [32]byte(tx.TxHash())

	return arkchannel.VTXOBinding{
		OORSessionID: sessionID,
		OutPoint: wire.OutPoint{
			Hash: chainhash.Hash(sessionID),
		},
		Amount:         terms.Capacity + arkchannel.DefaultBackingFee,
		ArkTransaction: raw.Bytes(), PolicyTemplate: policy,
		PkScript: pkScript,
	}
}

var _ arkchannel.OORTransferController = (*testPrePONROORController)(nil)
var _ prePONRResultController = (*testPrePONROORController)(nil)
