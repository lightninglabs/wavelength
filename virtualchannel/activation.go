package virtualchannel

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"
)

// LNDClient is the lnd surface required for virtual channel activation.
type LNDClient interface {
	OpenChannelStream(context.Context, route.Vertex, btcutil.Amount,
		btcutil.Amount, bool, ...lndclient.OpenChannelOption) (
		<-chan *lndclient.OpenStatusUpdate, <-chan error, error)

	FundingStateStep(context.Context,
		*lnrpc.FundingTransitionMsg) (
		*lnrpc.FundingStateStepResp,
		error,
	)

	PendingChannels(context.Context) (*lndclient.PendingChannels, error)

	ListChannels(context.Context, bool, bool,
		...lndclient.ListChannelsOption) (
		[]lndclient.ChannelInfo,
		error,
	)
}

// FundingInput records the VTXO metadata lnd needs to verify the funding PSBT.
type FundingInput struct {
	BackingVTXO

	PkScript []byte
}

// ActivationRequest describes the lnd-side virtual channel funding handshake.
type ActivationRequest struct {
	Peer             route.Vertex
	Capacity         btcutil.Amount
	PushAmount       btcutil.Amount
	Private          bool
	PendingChannelID PendingChannelID
	BackingInputs    []FundingInput
	UpdateTimeout    time.Duration

	// PersistFundingIntent stores the txid-stable funding artifact before
	// lnd accepts the PSBT. A successful lnd transition must never precede
	// the durable channel registration.
	PersistFundingIntent func(context.Context, *ActivationResult) error
}

// ActivationResult is the lnd funding artifact produced by a no-publish
// PSBT verification step.
type ActivationResult struct {
	PendingChannelID PendingChannelID
	ChannelPoint     wire.OutPoint
	BackingTx        *wire.MsgTx
	FundingPsbt      []byte
	Fee              btcutil.Amount
}

// ActivateNoPublishFunding starts lnd's PSBT channel funding flow, verifies a
// VTXO-backed funding PSBT with skip_finalize, and returns the exact backing
// transaction lnd now expects to eventually confirm. The returned backing
// transaction is intentionally unsigned; the caller must obtain and persist the
// collaborative VTXO spend witnesses before marking the virtual channel active.
func ActivateNoPublishFunding(ctx context.Context, lnd LNDClient,
	req ActivationRequest) (*ActivationResult, error) {

	if lnd == nil {
		return nil, fmt.Errorf("lnd client is nil")
	}
	if req.Capacity <= 0 {
		return nil, fmt.Errorf("capacity must be positive")
	}
	if req.PushAmount < 0 {
		return nil, fmt.Errorf("push amount must be non-negative")
	}
	if req.PushAmount > req.Capacity {
		return nil, fmt.Errorf("push amount exceeds capacity")
	}
	if len(req.BackingInputs) == 0 {
		return nil, fmt.Errorf("no backing inputs")
	}

	pendingID := req.PendingChannelID
	if pendingID == (PendingChannelID{}) {
		if _, err := io.ReadFull(
			rand.Reader, pendingID[:],
		); err != nil {
			return nil, fmt.Errorf("generate pending "+
				"channel id: %w", err)
		}
	}

	shim := &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_PsbtShim{
			PsbtShim: &lnrpc.PsbtShim{
				PendingChanId: pendingID[:],
				NoPublish:     true,
			},
		},
	}
	updates, errs, err := lnd.OpenChannelStream(
		ctx, req.Peer, req.Capacity, req.PushAmount, req.Private,
		lndclient.WithFundingShim(shim),
		lndclient.WithCommitmentType(
			lnrpc.CommitmentType_ANCHORS.Enum(),
		),
		lndclient.WithZeroConf(),
		lndclient.WithScid(),
	)
	if err != nil {
		return nil, fmt.Errorf("open channel stream: %w", err)
	}

	ready, err := waitForPsbtFunding(ctx, updates, errs, req.UpdateTimeout)
	if err != nil {
		return nil, cancelNoPublishFunding(
			ctx, lnd, pendingID, err,
		)
	}
	if ready.PendingChanID != nil &&
		!bytes.Equal(ready.PendingChanID, pendingID[:]) {
		return nil, cancelNoPublishFunding(
			ctx, lnd, pendingID,
			fmt.Errorf("unexpected pending channel id"),
		)
	}

	result, err := BuildFundedPSBT(
		ready.PsbtFund.GetPsbt(), ready.PsbtFund.GetFundingAmount(),
		pendingID, req.BackingInputs,
	)
	if err != nil {
		return nil, cancelNoPublishFunding(
			ctx, lnd, pendingID, err,
		)
	}
	if req.PersistFundingIntent != nil {
		err := req.PersistFundingIntent(ctx, result)
		if err != nil {
			return nil, cancelNoPublishFunding(
				ctx, lnd, pendingID,
				fmt.Errorf("persist funding intent: %w", err),
			)
		}
	}

	if err := VerifyNoPublishFunding(
		ctx, lnd, pendingID, result.FundingPsbt,
	); err != nil {
		// The app-side registration is already durable. Keep lnd's shim
		// available so the exact persisted PSBT can be retried after an
		// ambiguous RPC failure or process restart.
		return nil, err
	}
	if err := waitForChannelPending(
		ctx, updates, errs, req.UpdateTimeout, pendingID,
		result.ChannelPoint,
	); err != nil {

		// PsbtVerify succeeded, so lnd may still complete the funding
		// handshake after this RPC returns. Keep the durable app record
		// and let recovery confirm the exact channel point.
		return nil, err
	}

	return result, nil
}

// VerifyNoPublishFunding advances an existing lnd PSBT funding shim using the
// exact app-persisted funding artifact. It is the recovery operation for the
// lnd_negotiating FSM state.
func VerifyNoPublishFunding(ctx context.Context, lnd LNDClient,
	pendingID PendingChannelID, fundedPSBT []byte) error {

	if lnd == nil {
		return fmt.Errorf("lnd client is nil")
	}
	if pendingID == (PendingChannelID{}) {
		return fmt.Errorf("pending channel id is empty")
	}
	if len(fundedPSBT) == 0 {
		return fmt.Errorf("funding PSBT is empty")
	}

	_, err := lnd.FundingStateStep(ctx, &lnrpc.FundingTransitionMsg{
		Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
			PsbtVerify: &lnrpc.FundingPsbtVerify{
				FundedPsbt:    fundedPSBT,
				PendingChanId: pendingID[:],
				SkipFinalize:  true,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("verify funding PSBT: %w", err)
	}

	return nil
}

// ConfirmNoPublishFunding makes the lnd_negotiating -> funding_verified edge
// restart safe. It accepts an already durable pending/open channel, otherwise
// it replays the exact persisted PSBT and waits until lnd exposes the expected
// channel point from its channel database.
func ConfirmNoPublishFunding(ctx context.Context, lnd LNDClient,
	pendingID PendingChannelID, fundedPSBT []byte,
	channelPoint wire.OutPoint, timeout time.Duration) error {

	known, err := lndHasChannelPoint(ctx, lnd, channelPoint)
	if err != nil {
		return fmt.Errorf("inspect lnd channel state: %w", err)
	}
	if known {
		return nil
	}

	verifyErr := VerifyNoPublishFunding(ctx, lnd, pendingID, fundedPSBT)
	if verifyErr != nil {
		// The verify RPC can lose its response after lnd commits the
		// pending channel. Recheck the durable channel point before
		// surfacing the ambiguous error.
		known, lookupErr := lndHasChannelPoint(ctx, lnd, channelPoint)
		if lookupErr == nil && known {
			return nil
		}
		if lookupErr != nil {
			return errors.Join(
				verifyErr, fmt.Errorf("recheck lnd channel "+
					"state: %w", lookupErr),
			)
		}

		return verifyErr
	}

	return waitForLNDChannelPoint(ctx, lnd, channelPoint, timeout)
}

func cancelNoPublishFunding(ctx context.Context, lnd LNDClient,
	pendingID PendingChannelID, cause error) error {

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), 5*time.Second,
	)
	defer cancel()

	_, err := lnd.FundingStateStep(
		cleanupCtx, &lnrpc.FundingTransitionMsg{
			Trigger: &lnrpc.FundingTransitionMsg_ShimCancel{
				ShimCancel: &lnrpc.FundingShimCancel{
					PendingChanId: pendingID[:],
				},
			},
		},
	)
	if err != nil {
		return errors.Join(
			cause, fmt.Errorf("cancel no-publish funding shim: %w",
				err),
		)
	}

	return cause
}

func waitForPsbtFunding(ctx context.Context,
	updates <-chan *lndclient.OpenStatusUpdate, errs <-chan error,
	timeout time.Duration) (*lndclient.OpenStatusUpdate, error) {

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	for {
		select {
		case update, ok := <-updates:
			if !ok {
				return nil, fmt.Errorf("open channel stream " +
					"closed")
			}
			if update.PsbtFund != nil {
				return update, nil
			}

		case err, ok := <-errs:
			if !ok {
				return nil, fmt.Errorf("open channel error " +
					"stream closed")
			}

			return nil, fmt.Errorf("open channel stream: %w", err)

		case <-timer:
			return nil, fmt.Errorf("timed out waiting for PSBT " +
				"funding")

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func waitForChannelPending(ctx context.Context,
	updates <-chan *lndclient.OpenStatusUpdate, errs <-chan error,
	timeout time.Duration, pendingID PendingChannelID,
	expected wire.OutPoint) error {

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	for {
		select {
		case update, ok := <-updates:
			if !ok {
				return fmt.Errorf("open channel stream closed")
			}
			if update.ChanPending == nil {
				continue
			}
			pendingIDMatches := bytes.Equal(
				update.PendingChanID, pendingID[:],
			)
			if update.PendingChanID != nil && !pendingIDMatches {
				return fmt.Errorf("unexpected pending " +
					"channel id")
			}

			txid, err := chainhash.NewHash(update.ChanPending.Txid)
			if err != nil {
				return fmt.Errorf("decode pending channel "+
					"txid: %w", err)
			}
			actual := wire.OutPoint{
				Hash:  *txid,
				Index: update.ChanPending.OutputIndex,
			}
			if actual != expected {
				return fmt.Errorf("unexpected pending channel "+
					"point %s, expected %s", actual,
					expected)
			}

			return nil

		case err, ok := <-errs:
			if !ok {
				return fmt.Errorf("open channel error stream " +
					"closed")
			}

			return fmt.Errorf("open channel stream: %w", err)

		case <-timer:
			return fmt.Errorf("timed out waiting for durable " +
				"pending channel")

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func waitForLNDChannelPoint(ctx context.Context, lnd LNDClient,
	channelPoint wire.OutPoint, timeout time.Duration) error {

	var timeoutC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		known, err := lndHasChannelPoint(ctx, lnd, channelPoint)
		if err != nil {
			return fmt.Errorf("inspect lnd channel state: %w", err)
		}
		if known {
			return nil
		}

		select {
		case <-ticker.C:
		case <-timeoutC:
			return fmt.Errorf("timed out waiting for lnd "+
				"channel %s", channelPoint)

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func lndHasChannelPoint(ctx context.Context, lnd LNDClient,
	channelPoint wire.OutPoint) (bool, error) {

	if lnd == nil {
		return false, fmt.Errorf("lnd client is nil")
	}

	pending, err := lnd.PendingChannels(ctx)
	if err != nil {
		return false, err
	}
	for _, channel := range pending.PendingOpen {
		if channel.ChannelPoint != nil &&
			*channel.ChannelPoint == channelPoint {
			return true, nil
		}
	}

	channels, err := lnd.ListChannels(ctx, false, false)
	if err != nil {
		return false, err
	}
	for _, channel := range channels {
		outpoint, err := lndclient.NewOutpointFromStr(
			channel.ChannelPoint,
		)
		if err != nil {
			return false, fmt.Errorf("decode lnd channel point "+
				"%q: %w", channel.ChannelPoint, err)
		}
		if *outpoint == channelPoint {
			return true, nil
		}
	}

	return false, nil
}

// BuildFundedPSBT adds the selected VTXO inputs to lnd's funding-output PSBT.
func BuildFundedPSBT(basePSBT []byte, fundingAmount int64,
	pendingID PendingChannelID,
	backingInputs []FundingInput) (*ActivationResult, error) {

	if fundingAmount <= 0 {
		return nil, fmt.Errorf("funding amount must be positive")
	}

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(basePSBT), false)
	if err != nil {
		return nil, fmt.Errorf("parse funding PSBT: %w", err)
	}

	fundingOutput, outputIndex, err := fundingOutputFromPSBT(
		packet, fundingAmount,
	)
	if err != nil {
		return nil, err
	}

	backingVTXOs := make([]BackingVTXO, 0, len(backingInputs))
	for _, input := range backingInputs {
		backingVTXOs = append(backingVTXOs, input.BackingVTXO)
	}

	backingTx, fee, err := BuildBackingTx(backingVTXOs, fundingOutput)
	if err != nil {
		return nil, err
	}
	if fee <= 0 {
		return nil, fmt.Errorf("backing VTXOs must include a " +
			"positive funding fee")
	}

	fundedPacket, err := psbt.NewFromUnsignedTx(backingTx)
	if err != nil {
		return nil, fmt.Errorf("create funded PSBT: %w", err)
	}

	inputsByOutpoint := make(
		map[wire.OutPoint]FundingInput, len(backingInputs),
	)
	for _, input := range backingInputs {
		if input.Amount <= 0 {
			return nil, fmt.Errorf("backing VTXO %s amount must "+
				"be positive", input.OutPoint)
		}
		if len(input.PkScript) == 0 {
			return nil, fmt.Errorf("backing VTXO %s script "+
				"is empty", input.OutPoint)
		}

		inputsByOutpoint[input.OutPoint] = input
	}

	for idx, txIn := range backingTx.TxIn {
		input, ok := inputsByOutpoint[txIn.PreviousOutPoint]
		if !ok {
			return nil, fmt.Errorf("missing input metadata for %s",
				txIn.PreviousOutPoint)
		}

		fundedPacket.Inputs[idx].WitnessUtxo = &wire.TxOut{
			Value:    int64(input.Amount),
			PkScript: append([]byte(nil), input.PkScript...),
		}
	}

	var funded bytes.Buffer
	if err := fundedPacket.Serialize(&funded); err != nil {
		return nil, fmt.Errorf("serialize funded PSBT: %w", err)
	}

	channelPoint := wire.OutPoint{
		Hash:  backingTx.TxHash(),
		Index: outputIndex,
	}

	return &ActivationResult{
		PendingChannelID: pendingID,
		ChannelPoint:     channelPoint,
		BackingTx:        backingTx,
		FundingPsbt:      funded.Bytes(),
		Fee:              fee,
	}, nil
}

func fundingOutputFromPSBT(packet *psbt.Packet, fundingAmount int64) (
	*wire.TxOut, uint32, error) {

	if packet == nil || packet.UnsignedTx == nil {
		return nil, 0, fmt.Errorf("funding PSBT has no unsigned tx")
	}

	var (
		fundingOutput *wire.TxOut
		outputIndex   uint32
	)
	for idx, output := range packet.UnsignedTx.TxOut {
		if output.Value != fundingAmount {
			continue
		}
		if fundingOutput != nil {
			return nil, 0, fmt.Errorf("funding PSBT has multiple "+
				"outputs with funding amount %d", fundingAmount)
		}

		fundingOutput = output
		outputIndex = uint32(idx)
	}

	if fundingOutput == nil {
		return nil, 0, fmt.Errorf("funding output for %d sats "+
			"not found", fundingAmount)
	}

	return &wire.TxOut{
		Value:    fundingOutput.Value,
		PkScript: append([]byte(nil), fundingOutput.PkScript...),
	}, outputIndex, nil
}
