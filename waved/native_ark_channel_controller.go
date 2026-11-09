package waved

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrInsufficientArkChannelLiquidity means no active private channel
	// has enough balance on the requested sending side.
	ErrInsufficientArkChannelLiquidity = fmt.Errorf("insufficient active " +
		"Ark channel liquidity")

	// ErrReceiveChannelFallback means channel creation was abandoned before
	// the hub's prepared OOR crossed its signing point of no return.
	ErrReceiveChannelFallback = fmt.Errorf("receive channel can safely " +
		"fall back to vHTLC")
)

// NativeArkChannelController owns one Ark FSM and one modular lnd endpoint.
type NativeArkChannelController struct {
	party       arkchannel.Party
	cfg         ArkChannelControllerConfig
	coordinator *arkchannel.Coordinator
	remote      lnruntime.ProcessFundingPeer
	peerInfo    lnruntime.FundingPeerInfo
	keys        nativeArkChannelKeys
	service     *arkchannel.Service
}

var _ contractcourt.AuxChannelLifecycle = (*NativeArkChannelController)(nil)

// nativeArkChannelKeys are fixed wallet roles restored by locator on restart.
type nativeArkChannelKeys struct {
	ark     keychain.KeyDescriptor
	backing keychain.KeyDescriptor
	funder  keychain.KeyDescriptor
}

// arkChannelBackingRestorer registers virtual funding before lnd restores its
// pending funding manager state.
type arkChannelBackingRestorer interface {
	RestoreBacking(arkchannel.Terms, arkchannel.Backing) error
}

// restoreNativeArkChannelBackingRecords registers every signed backing in one
// pre-start snapshot of the durable channel store.
func restoreNativeArkChannelBackingRecords(restorer arkChannelBackingRestorer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		backing := record.Snapshot.Backing
		if backing == nil ||
			record.Snapshot.Phase == arkchannel.PhaseClosed {

			continue
		}
		if err := restorer.RestoreBacking(
			record.Snapshot.Terms, *backing,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"restore backing: %w", err,
				),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// arkChannelForceCloseResumer reconciles one materialized channel with lnd's
// durable commitment-broadcast state.
type arkChannelForceCloseResumer interface {
	ResumeForceCloseChannel(wire.OutPoint) error
}

// resumeOnchainArkChannelRecords attempts every materialized channel and
// returns only isolated failures after the complete recovery pass.
func resumeOnchainArkChannelRecords(node arkChannelForceCloseResumer,
	records []arkchannel.Record) error {

	failures := make([]arkchannel.ResumeFailure, 0)
	for _, record := range records {
		if !shouldResumeOnchainArkChannel(record.Snapshot) {
			continue
		}
		if record.Snapshot.Backing == nil {
			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf(
					"materialized channel has no " +
						"backing",
				),
			})

			continue
		}
		if err := node.ResumeForceCloseChannel(
			record.Snapshot.Backing.ChannelPoint,
		); err != nil {

			failures = append(failures, arkchannel.ResumeFailure{
				ChannelID: record.Snapshot.Terms.ID,
				Err: fmt.Errorf("resume materialized channel: "+
					"%w",
					err),
			})
		}
	}
	if len(failures) > 0 {
		return &arkchannel.ResumeFailures{Failures: failures}
	}

	return nil
}

// shouldResumeOnchainArkChannel reports whether backing publication crossed
// the durable handoff but lnd may still need its commitment broadcast marker.
func shouldResumeOnchainArkChannel(snapshot arkchannel.Snapshot) bool {
	return snapshot.Phase == arkchannel.PhaseOnChain
}

// ChainWatchOwner reports whether lnd or the Ark lifecycle currently owns
// chain observation for a channel.
func (c *NativeArkChannelController) ChainWatchOwner(ctx context.Context,
	channel *chanstate.OpenChannel) (contractcourt.ChainWatchOwner, error) {

	record, err := c.coordinator.FindByChannelPoint(
		ctx, channel.FundingOutpoint,
	)
	if err != nil {
		return 0, fmt.Errorf("load Ark channel lifecycle: %w", err)
	}

	if shouldWatchArkChannel(record.Snapshot.Phase) {
		return contractcourt.ChainWatchOwnerLnd, nil
	}

	return contractcourt.ChainWatchOwnerAux, nil
}

// PrepareCommitmentPublish materializes the Ark backing before lnd publishes
// a commitment transaction.
func (c *NativeArkChannelController) PrepareCommitmentPublish(
	ctx context.Context, channelPoint wire.OutPoint) error {

	return c.materializeBeforeCommitment(ctx, channelPoint)
}

const arkChannelFinalizationRetry = time.Second

// WaitForChannelFinalization durably records lnd's terminal channel state.
// The Ark owner retains retry policy so lnd only provides the cancellation and
// ordering boundary.
func (c *NativeArkChannelController) WaitForChannelFinalization(
	ctx context.Context, channelPoint wire.OutPoint) error {

	for {
		err := c.recordFullyResolvedChannel(ctx, channelPoint)
		if err == nil {
			return nil
		}

		if c.cfg.Log != nil {
			c.cfg.Log.WarnS(ctx, "Unable to finalize Ark channel",
				err,
				btclog.Fmt("channel_point", "%v", channelPoint),
			)
		}

		timer := time.NewTimer(arkChannelFinalizationRetry)
		select {
		case <-timer.C:
			continue

		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		}
	}
}

// materializeBeforeCommitment blocks lnd's local commitment publication until
// either recovery-ready endpoint has durably published the exact backing.
func (c *NativeArkChannelController) materializeBeforeCommitment(
	ctx context.Context, channelPoint wire.OutPoint) error {

	record, err := c.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return fmt.Errorf("load channel publication lifecycle: %w", err)
	}
	if record.Snapshot.Phase == arkchannel.PhaseOnChain {
		return nil
	}
	if c.service == nil {
		return fmt.Errorf("Ark channel service is not ready")
	}
	record, err = c.service.Materialize(
		ctx, record.Snapshot.Terms.ID,
	)
	if err != nil {
		return fmt.Errorf("materialize channel backing: %w", err)
	}
	if record.Snapshot.Phase != arkchannel.PhaseOnChain {
		return fmt.Errorf("channel backing stopped in phase %s",
			record.Snapshot.Phase)
	}

	return nil
}

// shouldWatchArkChannel reports whether lnd must own the channel's chain and
// contract lifecycle. An unpublished outpoint remains dormant, while an
// observed publication can be handled without first contacting the peer.
func shouldWatchArkChannel(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseActivating, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing, arkchannel.PhaseOnChain,
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// shouldRestoreArkChannelAddsDisabled reports whether a durable cooperative
// close artifact requires a restored lnd link to remain quiesced.
func shouldRestoreArkChannelAddsDisabled(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// recordFullyResolvedChannel advances the Ark FSM only after lnd has resolved
// and swept every output. Chain evidence can recover a peer that never
// received the initiating endpoint's materialization transition.
func (c *NativeArkChannelController) recordFullyResolvedChannel(
	ctx context.Context, channelPoint wire.OutPoint) error {

	record, err := c.coordinator.FindByChannelPoint(ctx, channelPoint)
	if err != nil {
		return err
	}
	switch record.Snapshot.Phase {
	case arkchannel.PhaseActivating, arkchannel.PhaseActive,
		arkchannel.PhaseMaterializing,
		arkchannel.PhaseCoopClosing,
		arkchannel.PhaseCoopCloseSigned:

		record, _, err = c.coordinator.Apply(
			ctx, record.Snapshot.Terms.ID,
			&arkchannel.BackingObserved{
				TxID: channelPoint.Hash,
			},
		)
		if err != nil {
			return err
		}

	case arkchannel.PhaseOnChain:
	case arkchannel.PhaseClosed:
		return nil

	default:
		return fmt.Errorf("cannot resolve on-chain channel from %s",
			record.Snapshot.Phase)
	}

	_, _, err = c.coordinator.Apply(
		ctx, record.Snapshot.Terms.ID, &arkchannel.ChannelClosed{},
	)

	return err
}

// newPromotionTerms creates stable protocol identifiers and binds every key
// role to the two endpoint wallets.
func (c *NativeArkChannelController) newPromotionTerms(amount btcutil.Amount,
	idempotencyKey string) (arkchannel.Terms, error) {

	if idempotencyKey == "" {
		return arkchannel.Terms{}, fmt.Errorf("idempotency key is " +
			"required")
	}
	id, pending, scid := c.promotionIdentifiers(idempotencyKey)

	return c.newClientFundedTerms(
		id, pending, scid, amount, arkchannel.KindPromotion,
		lntypes.Hash{},
	)
}

// promotionIdentifiers derives every retry-sensitive protocol identifier from
// the daemon identity and caller-provided idempotency key.
func (c *NativeArkChannelController) promotionIdentifiers(
	idempotencyKey string) (arkchannel.ID, [32]byte, uint64) {

	derive := func(label string) [32]byte {
		hash := sha256.New()
		_, _ = hash.Write(
			[]byte("wavelength/ark-channel/promotion/v1/"),
		)
		_, _ = hash.Write([]byte(label))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(
			c.cfg.IdentityKey.PubKey.SerializeCompressed(),
		)
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(idempotencyKey))
		var result [32]byte
		copy(result[:], hash.Sum(nil))

		return result
	}
	id := arkchannel.ID(derive("channel-id"))
	pending := derive("pending-channel-id")
	scidSeed := derive("reserved-scid")
	blockHeight := uint32(scidSeed[0])<<16 |
		uint32(scidSeed[1])<<8 | uint32(scidSeed[2])
	txIndex := uint32(scidSeed[3])<<16 |
		uint32(scidSeed[4])<<8 | uint32(scidSeed[5])
	if blockHeight == 0 {
		blockHeight = 1
	}
	if txIndex == 0 {
		txIndex = 1
	}
	scid := lnwire.ShortChannelID{
		BlockHeight: blockHeight, TxIndex: txIndex,
		TxPosition: 0,
	}.ToUint64()

	return id, pending, scid
}

// newClientFundedTerms binds deterministic or random protocol identifiers to
// this endpoint's fixed channel and Ark keys.
func (c *NativeArkChannelController) newClientFundedTerms(id arkchannel.ID,
	pending [32]byte, scid uint64, amount btcutil.Amount,
	kind arkchannel.Kind, paymentHash lntypes.Hash) (arkchannel.Terms,
	error) {

	terms := arkchannel.Terms{
		ID: id, Kind: kind, Funder: arkchannel.PartyClient,
		PendingChannelID: pending, ReservedSCID: scid,
		Capacity: amount, PaymentHash: paymentHash,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay: c.peerInfo.ChannelDelay,
			FunderDelay:  c.peerInfo.FunderDelay,
			MinExitDelay: c.peerInfo.MinimumExitDelay,
		},
	}
	copy(
		terms.ClientNodeKey[:],
		c.cfg.IdentityKey.PubKey.SerializeCompressed(),
	)
	terms.HubNodeKey = c.peerInfo.HubNodeKey
	copy(
		terms.VTXO.ClientArkKey[:],
		c.keys.ark.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubArkKey = c.peerInfo.HubArkKey
	terms.VTXO.ArkOperatorKey = c.peerInfo.ArkOperatorKey
	copy(
		terms.VTXO.ClientChannelKey[:],
		c.keys.backing.PubKey.SerializeCompressed(),
	)
	terms.VTXO.HubChannelKey = c.peerInfo.HubChannelKey
	copy(
		terms.VTXO.FunderKey[:],
		c.keys.funder.PubKey.SerializeCompressed(),
	)

	return terms, terms.Validate()
}

// newReceiveIntentTerms binds one deterministic invoice reservation to the
// hub's Ark funding key and the client's channel endpoint keys.
func (c *NativeArkChannelController) newReceiveIntentTerms(
	paymentHash lntypes.Hash, reservedSCID uint64,
	capacity btcutil.Amount) (arkchannel.Terms, error) {

	pendingChannelID := arkchannel.ReceiveIntentPendingID(paymentHash)
	terms := arkchannel.Terms{
		ID:               arkchannel.ReceiveIntentID(paymentHash),
		Kind:             arkchannel.KindReceiveIntent,
		Funder:           arkchannel.PartyHub,
		PendingChannelID: pendingChannelID,
		ReservedSCID:     reservedSCID, Capacity: capacity,
		PaymentHash: paymentHash,
		VTXO: arkchannel.VTXOTerms{
			ChannelDelay:   c.peerInfo.ChannelDelay,
			FunderDelay:    c.peerInfo.FunderDelay,
			MinExitDelay:   c.peerInfo.MinimumExitDelay,
			HubArkKey:      c.peerInfo.HubArkKey,
			HubChannelKey:  c.peerInfo.HubChannelKey,
			ArkOperatorKey: c.peerInfo.ArkOperatorKey,
			FunderKey:      c.peerInfo.HubFunderKey,
		},
	}
	copy(
		terms.ClientNodeKey[:],
		c.cfg.IdentityKey.PubKey.SerializeCompressed(),
	)
	terms.HubNodeKey = c.peerInfo.HubNodeKey
	copy(
		terms.VTXO.ClientArkKey[:],
		c.keys.ark.PubKey.SerializeCompressed(),
	)
	copy(
		terms.VTXO.ClientChannelKey[:],
		c.keys.backing.PubKey.SerializeCompressed(),
	)

	return terms, terms.Validate()
}

// waitIncomingPaymentReady joins the two independent durable readiness
// barriers. Returning after only one would either expose an unusable channel or
// release the preimage before its channel exists.
func waitIncomingPaymentReady(ctx context.Context,
	waitInvoice func(context.Context) (lnwire.ShortChannelID, error),
	syncIntent func(context.Context) error) (lnwire.ShortChannelID, error) {

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type invoiceWaitResult struct {
		channelID lnwire.ShortChannelID
		err       error
	}
	invoiceResult := make(chan invoiceWaitResult, 1)
	go func() {
		channelID, err := waitInvoice(waitCtx)
		invoiceResult <- invoiceWaitResult{
			channelID: channelID,
			err:       err,
		}
	}()
	syncResult := make(chan error, 1)
	go func() {
		syncResult <- syncIntent(waitCtx)
	}()

	var incomingSCID lnwire.ShortChannelID
	for invoiceResult != nil || syncResult != nil {
		select {
		case result := <-invoiceResult:
			invoiceResult = nil
			if result.err != nil {
				return lnwire.ShortChannelID{}, fmt.Errorf(
					"wait for incoming hold invoice: %w",
					result.err)
			}
			incomingSCID = result.channelID

		case err := <-syncResult:
			syncResult = nil
			if err != nil && !errors.Is(
				err, ErrReceiveChannelFallback,
			) {
				return lnwire.ShortChannelID{}, fmt.Errorf(
					"synchronize receive channel: %w", err)
			}

		case <-waitCtx.Done():
			return lnwire.ShortChannelID{}, waitCtx.Err()
		}
	}

	if incomingSCID == (lnwire.ShortChannelID{}) {
		return lnwire.ShortChannelID{}, fmt.Errorf("incoming payment " +
			"channel SCID is empty")
	}

	return incomingSCID, nil
}

// receiveIntentIsRetained identifies reusable or later channel lifecycle
// states.
func receiveIntentIsRetained(phase arkchannel.Phase) bool {
	switch phase {
	case arkchannel.PhaseActive, arkchannel.PhaseMaterializing,
		arkchannel.PhaseOnChain, arkchannel.PhaseClosed,
		arkchannel.PhaseCoopClosing, arkchannel.PhaseCoopCloseSigned,
		arkchannel.PhaseCoopClosePublished:
		return true

	default:
		return false
	}
}

// receiveIntentEndpointsDisagree detects a retained/pre-PONR split.
func receiveIntentEndpointsDisagree(remoteMissing bool, remote arkchannel.Phase,
	localMissing bool, local arkchannel.Phase) bool {

	remoteRetained := !remoteMissing && receiveIntentIsRetained(remote)
	localRetained := !localMissing && receiveIntentIsRetained(local)

	return remoteRetained != localRetained
}
