package virtualchannel

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

func TestBuildFundedPSBT(t *testing.T) {
	pendingID := testPendingID(1)
	fundingScript := testScript(0x51)
	basePSBT := testFundingPSBT(t, &wire.TxOut{
		Value:    50_000,
		PkScript: fundingScript,
	})
	inputs := []FundingInput{
		{
			BackingVTXO: BackingVTXO{
				OutPoint: testOutPoint(0xa1, 0),
				Amount:   51_000,
			},
			PkScript: testScript(0x01),
		},
	}

	result, err := BuildFundedPSBT(basePSBT, 50_000, pendingID, inputs)
	require.NoError(t, err)
	require.Equal(t, pendingID, result.PendingChannelID)
	require.Equal(t, btcutil.Amount(1_000), result.Fee)
	require.Equal(t, result.BackingTx.TxHash(), result.ChannelPoint.Hash)
	require.Equal(t, uint32(0), result.ChannelPoint.Index)

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(result.FundingPsbt), false,
	)
	require.NoError(t, err)
	require.Len(t, packet.UnsignedTx.TxIn, 1)
	require.Len(t, packet.UnsignedTx.TxOut, 1)
	require.Equal(t, fundingScript, packet.UnsignedTx.TxOut[0].PkScript)
	for _, input := range packet.Inputs {
		require.NotNil(t, input.WitnessUtxo)
		require.NotEmpty(t, input.WitnessUtxo.PkScript)
	}
}

func TestBuildFundedPSBTRequiresPositiveFee(t *testing.T) {
	basePSBT := testFundingPSBT(t, &wire.TxOut{
		Value:    50_000,
		PkScript: testScript(0x51),
	})

	_, err := BuildFundedPSBT(
		basePSBT, 50_000, testPendingID(1), []FundingInput{
			{
				BackingVTXO: BackingVTXO{
					OutPoint: testOutPoint(0xa1, 0),
					Amount:   50_000,
				},
				PkScript: testScript(0x01),
			},
		},
	)
	require.ErrorContains(t, err, "positive funding fee")
}

func TestActivateNoPublishFunding(t *testing.T) {
	pendingID := testPendingID(3)
	basePSBT := testFundingPSBT(t, &wire.TxOut{
		Value:    50_000,
		PkScript: testScript(0x51),
	})
	lnd := newTestActivationLND(&lndclient.OpenStatusUpdate{
		PsbtFund: &lnrpc.ReadyForPsbtFunding{
			FundingAmount: 50_000,
			Psbt:          basePSBT,
		},
		PendingChanID: pendingID[:],
	})
	persisted := false

	result, err := ActivateNoPublishFunding(
		t.Context(), lnd, ActivationRequest{
			Peer:             testVertex(9),
			Capacity:         50_000,
			PushAmount:       1_000,
			Private:          true,
			PendingChannelID: pendingID,
			BackingInputs: []FundingInput{
				{
					BackingVTXO: BackingVTXO{
						OutPoint: testOutPoint(0xa1, 0),
						Amount:   51_000,
					},
					PkScript: testScript(0x01),
				},
			},
			UpdateTimeout: time.Second,
			PersistFundingIntent: func(_ context.Context,
				result *ActivationResult) error {

				require.Nil(t, lnd.fundingReq)
				require.Equal(
					t, pendingID, result.PendingChannelID,
				)
				persisted = true

				return nil
			},
		},
	)
	require.NoError(t, err)
	require.True(t, persisted)
	require.Equal(t, pendingID, result.PendingChannelID)
	require.True(t, lnd.openReq.ZeroConf)
	require.True(t, lnd.openReq.ScidAlias)
	require.True(t, lnd.openReq.Private)
	require.Equal(
		t, lnrpc.CommitmentType_ANCHORS, lnd.openReq.CommitmentType,
	)
	require.Equal(t, int64(50_000), lnd.openReq.LocalFundingAmount)
	require.Equal(t, int64(1_000), lnd.openReq.PushSat)

	psbtShim := lnd.openReq.GetFundingShim().GetPsbtShim()
	require.NotNil(t, psbtShim)
	require.True(t, psbtShim.NoPublish)
	require.Equal(t, pendingID[:], psbtShim.PendingChanId)

	verify := lnd.fundingReq.GetPsbtVerify()
	require.NotNil(t, verify)
	require.True(t, verify.SkipFinalize)
	require.Equal(t, pendingID[:], verify.PendingChanId)
	require.Equal(t, result.FundingPsbt, verify.FundedPsbt)
}

func TestActivateNoPublishFundingPropagatesStreamError(t *testing.T) {
	lnd := &testActivationLND{
		err: errors.New("peer disconnected"),
	}
	pendingID := testPendingID(3)

	_, err := ActivateNoPublishFunding(
		t.Context(), lnd, ActivationRequest{
			Peer:             testVertex(9),
			Capacity:         50_000,
			PendingChannelID: pendingID,
			BackingInputs: []FundingInput{
				{
					BackingVTXO: BackingVTXO{
						OutPoint: testOutPoint(0xa1, 0),
						Amount:   51_000,
					},
					PkScript: testScript(0x01),
				},
			},
			UpdateTimeout: time.Second,
		},
	)
	require.ErrorContains(t, err, "peer disconnected")
	cancel := lnd.fundingReq.GetShimCancel()
	require.NotNil(t, cancel)
	require.Equal(t, pendingID[:], cancel.PendingChanId)
}

func TestActivateNoPublishFundingKeepsPersistedShimForRetry(t *testing.T) {
	pendingID := testPendingID(4)
	basePSBT := testFundingPSBT(t, &wire.TxOut{
		Value:    50_000,
		PkScript: testScript(0x51),
	})
	lnd := newTestActivationLND(&lndclient.OpenStatusUpdate{
		PsbtFund: &lnrpc.ReadyForPsbtFunding{
			FundingAmount: 50_000,
			Psbt:          basePSBT,
		},
		PendingChanID: pendingID[:],
	})
	lnd.fundingErr = errors.New("rpc response lost")
	persisted := false

	_, err := ActivateNoPublishFunding(
		t.Context(), lnd, ActivationRequest{
			Peer:             testVertex(9),
			Capacity:         50_000,
			PendingChannelID: pendingID,
			BackingInputs: []FundingInput{{
				BackingVTXO: BackingVTXO{
					OutPoint: testOutPoint(0xa1, 0),
					Amount:   51_000,
				},
				PkScript: testScript(0x01),
			}},
			UpdateTimeout: time.Second,
			PersistFundingIntent: func(_ context.Context,
				_ *ActivationResult) error {

				persisted = true

				return nil
			},
		},
	)
	require.ErrorContains(t, err, "rpc response lost")
	require.True(t, persisted)
	require.NotNil(t, lnd.fundingReq.GetPsbtVerify())
	require.Nil(t, lnd.fundingReq.GetShimCancel())
}

func TestActivateNoPublishFundingRejectsWrongPendingChannelPoint(t *testing.T) {
	pendingID := testPendingID(5)
	basePSBT := testFundingPSBT(t, &wire.TxOut{
		Value:    50_000,
		PkScript: testScript(0x51),
	})
	lnd := newTestActivationLND(&lndclient.OpenStatusUpdate{
		PsbtFund: &lnrpc.ReadyForPsbtFunding{
			FundingAmount: 50_000,
			Psbt:          basePSBT,
		},
		PendingChanID: pendingID[:],
	})
	wrongPoint := testOutPoint(0xff, 1)
	lnd.pendingOverride = &wrongPoint

	_, err := ActivateNoPublishFunding(
		t.Context(), lnd, ActivationRequest{
			Peer:             testVertex(9),
			Capacity:         50_000,
			PendingChannelID: pendingID,
			BackingInputs: []FundingInput{{
				BackingVTXO: BackingVTXO{
					OutPoint: testOutPoint(0xa1, 0),
					Amount:   51_000,
				},
				PkScript: testScript(0x01),
			}},
			UpdateTimeout: time.Second,
		},
	)
	require.ErrorContains(t, err, "unexpected pending channel point")
}

func TestConfirmNoPublishFunding(t *testing.T) {
	pendingID := testPendingID(6)
	result, err := BuildFundedPSBT(
		testFundingPSBT(t, &wire.TxOut{
			Value:    50_000,
			PkScript: testScript(0x51),
		}), 50_000, pendingID, []FundingInput{{
			BackingVTXO: BackingVTXO{
				OutPoint: testOutPoint(0xa1, 0),
				Amount:   51_000,
			},
			PkScript: testScript(0x01),
		}},
	)
	require.NoError(t, err)

	t.Run("replays persisted PSBT", func(t *testing.T) {
		lnd := newTestActivationLND(nil)
		err := ConfirmNoPublishFunding(
			t.Context(), lnd, pendingID, result.FundingPsbt,
			result.ChannelPoint, time.Second,
		)
		require.NoError(t, err)
		require.NotNil(t, lnd.fundingReq.GetPsbtVerify())
	})

	t.Run("recognizes durable channel", func(t *testing.T) {
		point := result.ChannelPoint
		lnd := newTestActivationLND(nil)
		lnd.pending = []lndclient.PendingChannel{{
			ChannelPoint: &point,
		}}
		err := ConfirmNoPublishFunding(
			t.Context(), lnd, pendingID, result.FundingPsbt,
			result.ChannelPoint, time.Second,
		)
		require.NoError(t, err)
		require.Nil(t, lnd.fundingReq)
	})
}

type testActivationLND struct {
	update          *lndclient.OpenStatusUpdate
	updates         chan *lndclient.OpenStatusUpdate
	err             error
	fundingErr      error
	openReq         lnrpc.OpenChannelRequest
	fundingReq      *lnrpc.FundingTransitionMsg
	pendingOverride *wire.OutPoint
	pending         []lndclient.PendingChannel
	channels        []lndclient.ChannelInfo
}

func newTestActivationLND(
	update *lndclient.OpenStatusUpdate) *testActivationLND {

	return &testActivationLND{
		update:  update,
		updates: make(chan *lndclient.OpenStatusUpdate, 2),
	}
}

func (l *testActivationLND) OpenChannelStream(_ context.Context,
	peer route.Vertex, localSat, pushSat btcutil.Amount, private bool,
	opts ...lndclient.OpenChannelOption) (
	<-chan *lndclient.OpenStatusUpdate, <-chan error, error) {

	l.openReq = lnrpc.OpenChannelRequest{
		NodePubkey:         peer[:],
		LocalFundingAmount: int64(localSat),
		PushSat:            int64(pushSat),
		Private:            private,
	}
	for _, opt := range opts {
		opt(&l.openReq)
	}

	errs := make(chan error, 1)
	if l.err != nil {
		errs <- l.err
	} else if l.update != nil {
		l.updates <- l.update
	}

	return l.updates, errs, nil
}

func (l *testActivationLND) FundingStateStep(_ context.Context,
	req *lnrpc.FundingTransitionMsg) (*lnrpc.FundingStateStepResp, error) {

	l.fundingReq = req
	if l.fundingErr != nil {
		return &lnrpc.FundingStateStepResp{}, l.fundingErr
	}

	verify := req.GetPsbtVerify()
	if verify == nil {
		return &lnrpc.FundingStateStepResp{}, nil
	}
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(verify.FundedPsbt), false,
	)
	if err != nil {
		return nil, err
	}
	point := wire.OutPoint{
		Hash:  packet.UnsignedTx.TxHash(),
		Index: 0,
	}
	if l.pendingOverride != nil {
		point = *l.pendingOverride
	}
	l.pending = []lndclient.PendingChannel{{
		ChannelPoint: &point,
	}}
	if l.updates != nil {
		l.updates <- &lndclient.OpenStatusUpdate{
			ChanPending: &lnrpc.PendingUpdate{
				Txid:        point.Hash[:],
				OutputIndex: point.Index,
			},
			PendingChanID: verify.PendingChanId,
		}
	}

	return &lnrpc.FundingStateStepResp{}, nil
}

func (l *testActivationLND) PendingChannels(context.Context) (
	*lndclient.PendingChannels, error) {

	return &lndclient.PendingChannels{
		PendingOpen: l.pending,
	}, nil
}

func (l *testActivationLND) ListChannels(context.Context, bool, bool,
	...lndclient.ListChannelsOption) ([]lndclient.ChannelInfo, error) {

	return l.channels, nil
}

func testFundingPSBT(t *testing.T, output *wire.TxOut) []byte {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxOut(output)

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, packet.Serialize(&buf))

	return buf.Bytes()
}

func testPendingID(seed byte) PendingChannelID {
	var id PendingChannelID
	id[0] = seed

	return id
}

func testVertex(seed byte) route.Vertex {
	var vertex route.Vertex
	vertex[0] = 0x02
	vertex[32] = seed

	return vertex
}

func testOutPoint(seed byte, index uint32) wire.OutPoint {
	hash := chainhash.HashH([]byte{seed})

	return wire.OutPoint{
		Hash:  hash,
		Index: index,
	}
}

func testScript(seed byte) []byte {
	script := make([]byte, 34)
	script[0] = 0x00
	script[1] = 0x20
	script[2] = seed

	return script
}
