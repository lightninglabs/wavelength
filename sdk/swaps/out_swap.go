package swaps

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	loopfsm "github.com/lightninglabs/loop/fsm"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// defaultReceiveExpirySeconds is the default lifetime for the route
	// hint and matching invoice generated for a Lightning-to-Ark receive
	// flow.
	defaultReceiveExpirySeconds = 3600

	// defaultFundingResumeGracePeriod bounds how long a resumed pay
	// session waits for an accepted-but-not-yet-persisted funding
	// attempt to surface in the indexer before retrying the send.
	defaultFundingResumeGracePeriod = 15 * time.Second

	// defaultClaimResumeGracePeriod bounds how long a resumed receive
	// session waits for an accepted-but-not-yet-persisted claim attempt
	// to surface in the indexer before retrying the spend.
	defaultClaimResumeGracePeriod = 15 * time.Second

	// defaultClaimPreimageLookupAttempts bounds the short local retry loop
	// used after a vHTLC spend is indexed before its preimage metadata is
	// visible through the daemon.
	defaultClaimPreimageLookupAttempts = 5

	// defaultClaimPreimageLookupInterval is the delay between indexed-spend
	// preimage lookup attempts.
	defaultClaimPreimageLookupInterval = 200 * time.Millisecond

	// defaultFundingExpiryBuffer refuses to start a pay-side funding
	// attempt when the server-provided funding deadline is already too
	// close to allow meaningful progress.
	defaultFundingExpiryBuffer = 5 * time.Second

	// defaultRefundLocktimeBuffer reserves one block of safety before the
	// negotiated refund locktime so the client does not start or continue a
	// swap when the refund path is already imminent.
	defaultRefundLocktimeBuffer = uint32(1)
)

// ReceiveState identifies the client-side lifecycle state of a
// Lightning-to-Ark receive flow.
type ReceiveState uint8

const (
	// ReceiveStateCreated means the local SDK session exists but has not
	// yet requested a route hint or created the invoice.
	ReceiveStateCreated ReceiveState = iota

	// ReceiveStateInvoiceCreated means the route hint, invoice, preimage,
	// and expected vHTLC script are all available to the caller.
	ReceiveStateInvoiceCreated

	// ReceiveStateVHTLCFunded means the swap server funded the expected
	// vHTLC and the client has recorded the live outpoint and amount.
	ReceiveStateVHTLCFunded

	// ReceiveStateClaimInitiated means the client is attempting to sweep
	// the funded vHTLC with the invoice preimage.
	ReceiveStateClaimInitiated

	// ReceiveStateCompleted means the client submitted or observed the
	// vHTLC claim using the expected preimage.
	ReceiveStateCompleted

	// ReceiveStateExpired means the route/invoice lifetime elapsed before
	// the expected vHTLC could be funded or claimed.
	ReceiveStateExpired

	// ReceiveStateNeedsIntervention means the client observed
	// anomalous swap data that should be preserved for operator
	// inspection instead of being collapsed into a generic failure.
	ReceiveStateNeedsIntervention

	// ReceiveStateFailed means the local client flow hit an unrecoverable
	// error.
	ReceiveStateFailed
)

// String returns a human-readable receive state name.
func (s ReceiveState) String() string {
	switch s {
	case ReceiveStateCreated:
		return "Created"
	case ReceiveStateInvoiceCreated:
		return "InvoiceCreated"
	case ReceiveStateVHTLCFunded:
		return "VHTLCFunded"
	case ReceiveStateClaimInitiated:
		return "ClaimInitiated"
	case ReceiveStateCompleted:
		return "Completed"
	case ReceiveStateExpired:
		return "Expired"
	case ReceiveStateNeedsIntervention:
		return "NeedsIntervention"
	case ReceiveStateFailed:
		return "Failed"
	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// IsTerminal returns true if no more receive lifecycle work should run.
func (s ReceiveState) IsTerminal() bool {
	return s == ReceiveStateCompleted ||
		s == ReceiveStateExpired ||
		s == ReceiveStateNeedsIntervention ||
		s == ReceiveStateFailed
}

// receiveEvent identifies a transition edge in the receive FSM.
type receiveEvent = loopfsm.EventType

const (
	// receiveEventAdvance asks the current Loop FSM state to reconcile the
	// receive session against local daemon and swap-server state.
	receiveEventAdvance = loopfsm.EventType("OnAdvance")

	// receiveEventInvoiceCreated records that the client has prepared the
	// invoice and expected vHTLC script.
	receiveEventInvoiceCreated = loopfsm.EventType("OnInvoiceCreated")

	// receiveEventVHTLCFunded records that the expected vHTLC has been
	// observed on Ark.
	receiveEventVHTLCFunded = loopfsm.EventType("OnVHTLCFunded")

	// receiveEventClaimInitiated records claim intent before the custom
	// input OOR is sent.
	receiveEventClaimInitiated = loopfsm.EventType("OnClaimInitiated")

	// receiveEventCompleted records terminal success.
	receiveEventCompleted = loopfsm.EventType("OnCompleted")

	// receiveEventExpired records terminal expiry.
	receiveEventExpired = loopfsm.EventType("OnExpired")

	// receiveEventNeedsIntervention records a terminal anomalous state that
	// requires operator inspection.
	receiveEventNeedsIntervention = loopfsm.EventType(
		"OnNeedsIntervention",
	)

	// receiveEventFailed records terminal local failure.
	receiveEventFailed = loopfsm.EventType("OnFailed")
)

// receiveTransitions is the complete transition descriptor for client-side
// Lightning-to-Ark receives.
var receiveTransitions = map[ReceiveState]map[receiveEvent]ReceiveState{
	ReceiveStateCreated: {
		receiveEventInvoiceCreated:    ReceiveStateInvoiceCreated,
		receiveEventExpired:           ReceiveStateExpired,
		receiveEventNeedsIntervention: ReceiveStateNeedsIntervention,
		receiveEventFailed:            ReceiveStateFailed,
	},
	ReceiveStateInvoiceCreated: {
		receiveEventVHTLCFunded:       ReceiveStateVHTLCFunded,
		receiveEventExpired:           ReceiveStateExpired,
		receiveEventNeedsIntervention: ReceiveStateNeedsIntervention,
		receiveEventFailed:            ReceiveStateFailed,
	},
	ReceiveStateVHTLCFunded: {
		receiveEventClaimInitiated:    ReceiveStateClaimInitiated,
		receiveEventExpired:           ReceiveStateExpired,
		receiveEventNeedsIntervention: ReceiveStateNeedsIntervention,
		receiveEventFailed:            ReceiveStateFailed,
	},
	ReceiveStateClaimInitiated: {
		receiveEventCompleted:         ReceiveStateCompleted,
		receiveEventExpired:           ReceiveStateExpired,
		receiveEventNeedsIntervention: ReceiveStateNeedsIntervention,
		receiveEventFailed:            ReceiveStateFailed,
	},
}

// ReceiveSession holds one prepared Lightning->Ark swap receive flow. A
// session is not safe for concurrent method calls; callers should not call
// Wait, Claim, State, or other methods from multiple goroutines at the same
// time.
type ReceiveSession struct {
	// Invoice is the BOLT-11 payment request the payer must pay.
	Invoice string

	// Preimage is the fixed preimage committed into both the invoice and
	// the expected vHTLC claim path.
	Preimage lntypes.Preimage

	// PaymentHash is the Lightning payment hash for this receive flow.
	PaymentHash lntypes.Hash

	client              *SwapClient
	amountSat           btcutil.Amount
	state               ReceiveState
	deadline            time.Time
	createdAt           time.Time
	updatedAt           time.Time
	clientPubKey        *btcec.PublicKey
	operatorPubKey      *btcec.PublicKey
	swapServerPubKey    *btcec.PublicKey
	vhtlcConfig         VHTLCConfig
	vhtlcPolicy         *arkscript.VHTLCPolicy
	vhtlcPolicyTemplate []byte
	vhtlcPkScript       []byte
	vhtlcOutpoint       string
	vhtlcAmount         int64
	claimSessionID      string
	// claimIntentRecordedInProcess distinguishes freshly-recorded claim
	// intent from a restored ClaimInitiated row whose accepted spend may
	// still be missing from the indexer.
	claimIntentRecordedInProcess bool
	interventionReason           string
}

// State returns the current client-side receive lifecycle state.
func (s *ReceiveSession) State() ReceiveState {
	if s == nil {
		return ReceiveStateFailed
	}

	return s.state
}

// InterventionReason returns the durable operator-facing reason for a
// NeedsIntervention receive session.
func (s *ReceiveSession) InterventionReason() string {
	if s == nil || s.state != ReceiveStateNeedsIntervention {
		return ""
	}

	return s.interventionReason
}

// TerminalReason returns the durable explanation for a terminal receive
// session.
func (s *ReceiveSession) TerminalReason() string {
	if s == nil {
		return ""
	}

	return s.interventionReason
}

// terminalErr converts one terminal receive state into the public blocking API
// error returned by runUntil.
func (s *ReceiveSession) terminalErr() error {
	if s == nil {
		return fmt.Errorf("receive session must be provided")
	}

	switch s.state {
	case ReceiveStateExpired:
		return errSwapExpired

	case ReceiveStateNeedsIntervention:
		return newInterventionError(s.interventionReason, nil)

	case ReceiveStateFailed:
		return newFailureError(s.interventionReason, nil)

	default:
		return fmt.Errorf(
			"receive session stopped in terminal state %s",
			s.state,
		)
	}
}

// failTerminal persists one safely-classified receive-side terminal failure and
// returns the canonical failure error for the blocking API surface.
func (s *ReceiveSession) failTerminal(ctx context.Context, reason string,
	cause error, mutate func()) error {

	if s == nil {
		return newFailureError(reason, cause)
	}

	s.client.log.WarnS(ctx, "Receive swap failed", cause,
		btclog.Hex("hash", s.PaymentHash[:]),
		slog.String("reason", reason),
		slog.String("outpoint", s.vhtlcOutpoint),
		slog.Int64("amount_sat", s.vhtlcAmount),
	)

	err := s.mutateAndPersist(ctx, func() error {
		if mutate != nil {
			mutate()
		}

		s.interventionReason = reason
		if s.state == ReceiveStateFailed {
			return nil
		}

		return s.transition(receiveEventFailed)
	})
	if err != nil {
		return err
	}

	return newFailureError(reason, cause)
}

// ReceiveViaLightning performs a complete Lightning-to-Ark swap in a single
// blocking call. It generates a preimage, requests a route hint and vHTLC
// config from the swap server, creates a signed invoice, waits for the server
// to fund the vHTLC, and claims the funds into the client's wallet.
func (c *SwapClient) ReceiveViaLightning(ctx context.Context,
	amountSat btcutil.Amount) (*ReceiveResult, error) {

	session, err := c.StartReceiveViaLightning(ctx, amountSat)
	if err != nil {
		return nil, err
	}

	return session.Wait(ctx)
}

// StartReceiveViaLightning prepares a Lightning->Ark receive flow and returns
// the invoice plus claim context needed to complete it after payment begins.
func (c *SwapClient) StartReceiveViaLightning(ctx context.Context,
	amountSat btcutil.Amount) (*ReceiveSession, error) {

	if err := validateSatoshiAmount(
		amountSat, "receive amount",
	); err != nil {
		return nil, err
	}

	session := &ReceiveSession{
		client:    c,
		amountSat: amountSat,
		state:     ReceiveStateCreated,
	}

	if err := session.runUntil(
		ctx, ReceiveStateInvoiceCreated,
	); err != nil {
		return nil, err
	}

	return session, nil
}

// Wait blocks until the swap server funds the expected vHTLC, then claims it
// into the client's wallet.
func (s *ReceiveSession) Wait(ctx context.Context) (*ReceiveResult, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("receive session must be provided")
	}

	if err := s.runUntil(ctx, ReceiveStateCompleted); err != nil {
		return nil, err
	}

	return &ReceiveResult{
		Invoice:      s.Invoice,
		Preimage:     s.Preimage,
		PaymentHash:  s.PaymentHash,
		VTXOOutpoint: s.vhtlcOutpoint,
		AmountSat:    s.vhtlcAmount,
	}, nil
}

// WaitForFunding blocks until the swap server funds the expected vHTLC and
// returns the outpoint plus amount of the live vHTLC.
func (s *ReceiveSession) WaitForFunding(ctx context.Context) (string, int64,
	error) {

	if s == nil || s.client == nil {
		return "", 0, fmt.Errorf("receive session must be provided")
	}

	if err := s.runUntil(ctx, ReceiveStateVHTLCFunded); err != nil {
		return "", 0, err
	}

	return s.vhtlcOutpoint, s.vhtlcAmount, nil
}

// Claim submits the vHTLC claim for a funded out-swap into a fresh
// wallet-owned receive script.
func (s *ReceiveSession) Claim(ctx context.Context, outpoint string,
	amount int64) (*ReceiveResult, error) {

	if s == nil || s.client == nil {
		return nil, fmt.Errorf("receive session must be provided")
	}

	if s.state == ReceiveStateInvoiceCreated {
		err := s.validateReceiveFunding(ctx, outpoint, amount)
		if err != nil {
			return nil, err
		}

		if err := s.mutateAndPersist(ctx, func() error {
			s.vhtlcOutpoint = outpoint
			s.vhtlcAmount = amount

			return s.transition(receiveEventVHTLCFunded)
		}); err != nil {
			return nil, err
		}
	}

	if s.state != ReceiveStateVHTLCFunded &&
		s.state != ReceiveStateClaimInitiated &&
		s.state != ReceiveStateCompleted {

		return nil, fmt.Errorf(
			"cannot claim receive session in state %s", s.state,
		)
	}

	rememberedOutpoint := s.vhtlcOutpoint
	if rememberedOutpoint == "" {
		rememberedOutpoint = outpoint
	}

	rememberedAmount := s.vhtlcAmount
	if rememberedAmount == 0 {
		rememberedAmount = amount
	}

	if s.vhtlcOutpoint == "" || s.vhtlcAmount == 0 {
		if err := s.rememberReceiveFunding(
			ctx, rememberedOutpoint, rememberedAmount,
		); err != nil {
			return nil, err
		}
	}

	if err := s.runUntil(ctx, ReceiveStateCompleted); err != nil {
		return nil, err
	}

	return &ReceiveResult{
		Invoice:      s.Invoice,
		Preimage:     s.Preimage,
		PaymentHash:  s.PaymentHash,
		VTXOOutpoint: s.vhtlcOutpoint,
		AmountSat:    s.vhtlcAmount,
	}, nil
}

// VHTLCInfo returns the scripts for the expected incoming vHTLC and its
// preimage sweep path.
func (s *ReceiveSession) VHTLCInfo() (*ReceiveVHTLCInfo, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("receive session must be provided")
	}

	claimPath, err := s.vhtlcPolicy.ClaimPath(s.Preimage)
	if err != nil {
		return nil, fmt.Errorf("build claim path: %w", err)
	}

	return &ReceiveVHTLCInfo{
		PkScript: append([]byte(nil), s.vhtlcPkScript...),
		ClaimScript: append(
			[]byte(nil), claimPath.WitnessScript...,
		),
	}, nil
}

// runUntil advances the receive FSM until the target or a terminal state is
// reached.
func (s *ReceiveSession) runUntil(ctx context.Context,
	target ReceiveState) error {

	machine := newReceiveLoopFSM(s, target)

	for s.state != target {
		if s.state.IsTerminal() {
			return s.terminalErr()
		}

		if err := machine.advance(ctx); err != nil {
			return err
		}
	}

	return nil
}

// transition applies one receive FSM event to the current state.
func (s *ReceiveSession) transition(event receiveEvent) error {
	next, ok := receiveTransitions[s.state][event]
	if !ok {
		return fmt.Errorf("invalid receive transition %s -> %s",
			s.state, event)
	}

	s.state = next

	return nil
}

// prepareInvoice requests route metadata from the swap server and constructs
// the invoice plus expected vHTLC policy.
func (s *ReceiveSession) prepareInvoice(ctx context.Context) error {
	if s.client.invoiceGen == nil {
		return fmt.Errorf(
			"invoice generator required for ReceiveViaLightning",
		)
	}

	clientKey, err := s.client.daemon.IdentityPubKey(ctx)
	if err != nil {
		return fmt.Errorf("get client pubkey: %w", err)
	}

	operatorKey, err := s.client.daemon.OperatorPubKey(ctx)
	if err != nil {
		return fmt.Errorf("get operator pubkey: %w", err)
	}

	preimage, err := NewPreimage()
	if err != nil {
		return fmt.Errorf("generate preimage: %w", err)
	}

	// Receive setup is the one session edge that prepares external
	// metadata before the first durable row exists. The server's route hint
	// allocation is lease-like, and the SDK invoice generator only encodes
	// a signed payment request instead of registering it with an LN invoice
	// database. The session is persisted before the invoice is returned to
	// the caller.
	expiry := time.Duration(defaultReceiveExpirySeconds) * time.Second
	hint, vhtlcCfg, err := s.client.server.RequestChannelID(
		ctx, clientKey, defaultReceiveExpirySeconds,
	)
	if err != nil {
		return fmt.Errorf("request channel ID: %w", err)
	}

	s.client.log.InfoS(ctx, "Received route hint from swap server",
		slog.Uint64("channel_id", hint.ChannelID),
	)

	inv, hash, err := s.client.invoiceGen.CreateInvoice(
		ctx, s.amountSat, "swap", hint, expiry, &preimage,
	)
	if err != nil {
		return fmt.Errorf("create invoice: %w", err)
	}

	serverKey, err := btcec.ParsePubKey(vhtlcCfg.SwapServerPubkey)
	if err != nil {
		return fmt.Errorf("parse server pubkey: %w", err)
	}

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:       serverKey,
		Receiver:     clientKey,
		Server:       operatorKey,
		PreimageHash: hash,
		RefundLocktime: vhtlcCfg.
			RefundLocktime,
		UnilateralClaimDelay: vhtlcCfg.
			UnilateralClaimDelay,
		UnilateralRefundDelay: vhtlcCfg.
			UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: vhtlcCfg.
			UnilateralRefundWithoutReceiverDelay,
	})
	if err != nil {
		return fmt.Errorf("build vHTLC policy: %w", err)
	}

	pkScript, err := policy.PkScript()
	if err != nil {
		return fmt.Errorf("get vHTLC pkScript: %w", err)
	}

	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	if err != nil {
		return fmt.Errorf("encode vHTLC policy: %w", err)
	}

	s.client.log.InfoS(ctx, "Invoice created for out-swap",
		btclog.Hex("hash", hash[:]),
		slog.Int64("amount_sat", int64(s.amountSat)),
		slog.Time(
			"deadline",
			s.client.currentTime().Add(expiry),
		),
	)

	return s.mutateAndPersist(ctx, func() error {
		s.Invoice = string(inv.PaymentRequest)
		s.Preimage = preimage
		s.PaymentHash = hash
		s.deadline = s.client.currentTime().Add(expiry)
		if s.createdAt.IsZero() {
			s.createdAt = s.client.currentTime()
		}
		s.clientPubKey = clientKey
		s.operatorPubKey = operatorKey
		s.swapServerPubKey = serverKey
		s.vhtlcConfig = *vhtlcCfg
		s.vhtlcPolicy = policy
		s.vhtlcPolicyTemplate = policyTemplate
		s.vhtlcPkScript = pkScript

		return s.transition(receiveEventInvoiceCreated)
	})
}

// waitForFunding waits until the expected vHTLC is indexed as live.
func (s *ReceiveSession) waitForFunding(ctx context.Context) error {
	outpoint, amount, err := s.client.waitForVHTLC(
		ctx, s.vhtlcPkScript, s.deadline,
		s.ensureReceiveFundingStillPossible,
	)
	if err != nil {
		if errors.Is(err, errSwapExpired) {
			return err
		}

		return fmt.Errorf("wait for vHTLC: %w", err)
	}

	if err := s.validateReceiveFunding(ctx, outpoint, amount); err != nil {
		return err
	}

	return s.mutateAndPersist(ctx, func() error {
		s.vhtlcOutpoint = outpoint
		s.vhtlcAmount = amount

		return s.transition(receiveEventVHTLCFunded)
	})
}

// ensureReceiveFundingStillPossible stops an invoice-created receive session
// once the vHTLC refund path has matured and no live claimable vHTLC was found.
func (s *ReceiveSession) ensureReceiveFundingStillPossible(
	ctx context.Context) error {

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		s.client.log.DebugS(ctx,
			"Unable to query receive refund locktime height", err,
			btclog.Hex("hash", s.PaymentHash[:]),
		)

		return nil
	}

	if height+s.client.refundLocktimeBuffer < s.vhtlcConfig.RefundLocktime {
		return nil
	}

	return fmt.Errorf(
		"refund locktime %d is imminent or reached before receive "+
			"funding was observed at height %d: %w",
		s.vhtlcConfig.RefundLocktime, height, errSwapExpired,
	)
}

// validateReceiveFunding checks manually or automatically observed funding
// details before the claim path can reveal the invoice preimage.
func (s *ReceiveSession) validateReceiveFunding(ctx context.Context,
	outpoint string, amount int64) error {

	if amount != int64(s.amountSat) {
		return s.failTerminal(ctx, fmt.Sprintf(
			"funded vHTLC amount %d does not match "+
				"invoice amount %d",
			amount, s.amountSat,
		), nil, func() {
			s.vhtlcOutpoint = outpoint
			s.vhtlcAmount = amount
		})
	}

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return fmt.Errorf("get block height: %w", err)
	}
	if height+s.client.refundLocktimeBuffer >=
		s.vhtlcConfig.RefundLocktime {

		return s.failTerminal(ctx, fmt.Sprintf(
			"funded vHTLC observed too close to refund "+
				"locktime %d at height %d",
			s.vhtlcConfig.RefundLocktime, height,
		), nil, func() {
			s.vhtlcOutpoint = outpoint
			s.vhtlcAmount = amount
		})
	}

	return nil
}

// claimFundedVHTLC reconciles an already-spent vHTLC before sending the claim
// transaction, then submits the preimage claim if no spend is indexed yet.
func (s *ReceiveSession) claimFundedVHTLC(ctx context.Context) error {
	claimed, err := s.client.receiveClaimAlreadyIndexed(
		ctx, s.PaymentHash, s.vhtlcPkScript,
	)
	if err != nil {
		return err
	}
	if claimed {
		return s.mutateAndPersist(ctx, func() error {
			return s.transition(receiveEventCompleted)
		})
	}

	if s.claimSessionID != "" {
		return s.mutateAndPersist(ctx, func() error {
			return s.transition(receiveEventCompleted)
		})
	}

	if err := s.waitForClaimResumeGrace(ctx); err != nil {
		return err
	}

	claimSessionID, err := s.client.claimReceiveVHTLC(
		ctx, s.PaymentHash, s.Preimage, s.vhtlcPolicy,
		s.vhtlcPolicyTemplate,
		s.vhtlcPkScript, s.vhtlcOutpoint, s.vhtlcAmount,
	)
	if errors.Is(err, errReceiveClaimAlreadyIndexed) {
		return s.mutateAndPersist(ctx, func() error {
			return s.transition(receiveEventCompleted)
		})
	}
	if err != nil {
		return err
	}
	if claimSessionID == "" {
		return fmt.Errorf("claim vHTLC returned empty session id")
	}

	if err := s.persistClaimSessionID(ctx, claimSessionID); err != nil {
		return newRetryableActionError(err)
	}
	s.claimIntentRecordedInProcess = false

	if err := s.mutateAndPersist(ctx, func() error {
		return s.transition(receiveEventCompleted)
	}); err != nil {
		return newRetryableActionError(err)
	}

	return nil
}

// waitForClaimResumeGrace gives an accepted-but-not-yet-indexed receive claim a
// bounded chance to surface after restart before submitting another spend.
func (s *ReceiveSession) waitForClaimResumeGrace(ctx context.Context) error {
	if s.state != ReceiveStateClaimInitiated || s.updatedAt.IsZero() ||
		s.claimIntentRecordedInProcess {

		return nil
	}

	wait := s.updatedAt.Add(s.client.claimResumeGracePeriod).
		Sub(s.client.currentTime())
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-timer.C:
		return nil
	}
}

// receiveClaimAlreadyIndexed reports whether the expected vHTLC has already
// been spent with the receive preimage.
func (c *SwapClient) receiveClaimAlreadyIndexed(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte) (bool, error) {

	vtxo, err := c.daemon.FindSpentVTXOByPkScript(ctx, pkScript)
	if err != nil || vtxo == nil {
		return false, err
	}

	preimage, err := findMatchingPreimageInVTXO(vtxo, paymentHash)
	if err != nil {
		return false, err
	}
	if preimageMatchesHash(preimage, paymentHash) {
		return true, nil
	}

	if vtxo.SpentByTxID == "" {
		return false, nil
	}

	pkg, err := c.daemon.GetIndexedOORSession(
		ctx, pkScript, vtxo.SpentByTxID,
	)
	if err != nil {
		return false, err
	}

	preimage, err = findMatchingPreimageInCheckpoints(pkg, paymentHash)
	if err != nil {
		return false, err
	}

	return preimageMatchesHash(preimage, paymentHash), nil
}

// claimReceiveVHTLC claims one funded vHTLC with the session preimage into a
// fresh wallet-owned receive script.
func (c *SwapClient) claimReceiveVHTLC(ctx context.Context,
	paymentHash lntypes.Hash, preimage lntypes.Preimage,
	policy *arkscript.VHTLCPolicy, policyTemplate []byte, pkScript []byte,
	outpoint string, amount int64) (string, error) {

	c.log.InfoS(ctx, "vHTLC found, claiming",
		btclog.Hex("hash", paymentHash[:]),
		slog.String("outpoint", outpoint),
		slog.Int64("amount", amount),
	)

	claimPath, err := policy.ClaimPath(preimage)
	if err != nil {
		return "", fmt.Errorf("build claim path: %w", err)
	}

	spendPath, err := claimPath.Encode()
	if err != nil {
		return "", fmt.Errorf("encode claim path: %w", err)
	}

	receiveInfo, err := c.daemon.AllocateReceiveScript(ctx, "")
	if err != nil {
		return "", fmt.Errorf("get receive script: %w", err)
	}
	if receiveInfo == nil {
		return "", fmt.Errorf("receive script is required")
	}

	var lastSendErr error
	for attempt := 1; attempt <= c.claimMaxAttempts; attempt++ {
		claimSessionID, err := c.daemon.SendOORWithCustomInputs(
			ctx, receiveInfo.PubKeyXOnly, amount, []CustomInput{{
				Outpoint:           outpoint,
				VTXOPolicyTemplate: policyTemplate,
				SpendPath:          spendPath,
				AmountSat:          amount,
				PkScript:           pkScript,
			}},
		)
		if err == nil {
			c.log.InfoS(ctx, "vHTLC claimed successfully",
				btclog.Hex("hash", paymentHash[:]),
			)

			return claimSessionID, nil
		}
		lastSendErr = err

		claimed, reconcileErr := c.receiveClaimAlreadyIndexed(
			ctx, paymentHash, pkScript,
		)
		if reconcileErr != nil {
			return "", reconcileErr
		}
		if claimed {
			return "", errReceiveClaimAlreadyIndexed
		}

		if attempt == c.claimMaxAttempts {
			break
		}

		c.log.DebugS(ctx, "vHTLC claim retry scheduled",
			btclog.Hex("hash", paymentHash[:]),
			slog.Int("attempt", attempt),
			slog.String("reason", err.Error()))

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(c.claimRetryDelay):
		}
	}

	if lastSendErr == nil {
		return "", fmt.Errorf("claim vHTLC: no send attempt made")
	}

	return "", fmt.Errorf("claim vHTLC: %w", lastSendErr)
}

// encodeVHTLCPolicyTemplate serializes the semantic vHTLC policy template in
// canonical leaf order.
func encodeVHTLCPolicyTemplate(policy *arkscript.VHTLCPolicy) ([]byte, error) {
	if policy == nil {
		return nil, fmt.Errorf("vHTLC policy is required")
	}

	if policy.Template == nil {
		return nil, fmt.Errorf("vHTLC policy template is required")
	}

	return policy.Template.Encode()
}

// waitForVHTLC polls the authoritative indexer until the expected vHTLC is
// live, then returns its outpoint and amount.
func (c *SwapClient) waitForVHTLC(ctx context.Context,
	pkScript []byte, deadline time.Time,
	keepWaiting func(context.Context) error) (string, int64, error) {

	pkScriptHex := hex.EncodeToString(pkScript)
	if deadline.IsZero() {
		deadline = c.currentTime().Add(c.waitVHTLCTimeout)
	}

	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}

		vtxo, err := c.daemon.FindLiveVTXOByPkScript(ctx, pkScript)
		if err != nil {
			c.log.DebugS(ctx, "Unable to query vHTLC state", err,
				slog.String("pk_script", pkScriptHex))
		}
		if vtxo != nil {
			c.log.InfoS(ctx, "Found funded vHTLC",
				slog.String("pk_script", pkScriptHex),
				slog.String("outpoint", vtxo.Outpoint),
				slog.Int64("amount_sat", vtxo.AmountSat),
			)

			return vtxo.Outpoint, vtxo.AmountSat, nil
		}

		if keepWaiting != nil {
			if err := keepWaiting(ctx); err != nil {
				return "", 0, err
			}
		}

		if !deadline.IsZero() && !c.currentTime().Before(deadline) {
			return "", 0, errSwapExpired
		}

		wait := c.waitPollInterval
		if until := deadline.Sub(c.currentTime()); !deadline.IsZero() &&
			until < wait {

			wait = until
		}
		if wait <= 0 {
			return "", 0, errSwapExpired
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", 0, ctx.Err()

		case <-timer.C:
		}
	}
}
