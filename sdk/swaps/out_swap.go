package swaps

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	loopfsm "github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch/hop"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

	// defaultOverdueReceiveMailboxPollWindow is the grace window used when
	// a daemon resumes an unpaid receive after its invoice deadline. It
	// gives an already-delivered mailbox event time to arrive locally
	// before the receive is expired.
	defaultOverdueReceiveMailboxPollWindow = 10 * time.Second

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

type outSwapOnionDecoder func(ReceiveAuthKey, lntypes.Hash,
	[]byte) (*decodedOutSwapOnion, error)

type decodedOutSwapOnion struct {
	amountToForward lnwire.MilliSatoshi
	totalAmount     lnwire.MilliSatoshi
	paymentAddr     [32]byte
	hasMPP          bool
}

// ReceiveState identifies the client-side lifecycle state of a
// Lightning-to-Ark receive flow.
type ReceiveState uint8

const (
	// ReceiveStateCreated means the local SDK session exists but has not
	// yet requested a route hint or created the invoice.
	ReceiveStateCreated ReceiveState = iota

	// ReceiveStateInvoiceCreated means the route hint, invoice, preimage,
	// and mailbox metadata are all available to the caller.
	ReceiveStateInvoiceCreated

	// ReceiveStateHTLCEventAccepted means the server's mailbox event has
	// been validated and durably accepted, so the client can resume funding
	// detection without requiring mailbox redelivery.
	ReceiveStateHTLCEventAccepted

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

	case ReceiveStateHTLCEventAccepted:
		return "HTLCEventAccepted"

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
	// invoice and mailbox metadata.
	receiveEventInvoiceCreated = loopfsm.EventType("OnInvoiceCreated")

	// receiveEventHTLCEventAccepted records that the server's mailbox event
	// has been validated and durably accepted.
	receiveEventHTLCEventAccepted = loopfsm.EventType(
		"OnHTLCEventAccepted",
	)

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
		receiveEventHTLCEventAccepted: ReceiveStateHTLCEventAccepted,
		receiveEventExpired:           ReceiveStateExpired,
		receiveEventNeedsIntervention: ReceiveStateNeedsIntervention,
		receiveEventFailed:            ReceiveStateFailed,
	},
	ReceiveStateHTLCEventAccepted: {
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

	client             *SwapClient
	amountSat          btcutil.Amount
	memo               string
	payerFeeMsat       uint64
	requestedAmountSat uint64
	availableCreditSat uint64
	attachedCreditSat  uint64
	expectedVHTLCSat   uint64
	dustLimitSat       uint64
	state              ReceiveState
	deadline           time.Time
	createdAt          time.Time
	updatedAt          time.Time
	clientPubKey       *btcec.PublicKey
	operatorPubKey     *btcec.PublicKey
	// swapServerPubKey is the remote sender in the accepted vHTLC policy.
	// For Lightning-backed receives this is the swap server key; for
	// direct same-Ark receives this is the paying client's sender key.
	swapServerPubKey       *btcec.PublicKey
	settlementType         SettlementType
	vhtlcConfig            VHTLCConfig
	vhtlcPolicy            *arkscript.VHTLCPolicy
	vhtlcPolicyTemplate    []byte
	vhtlcPkScript          []byte
	vhtlcOutpoint          string
	vhtlcAmount            int64
	paymentAddr            [32]byte
	pendingHTLCAckCursor   uint64
	claimReceivePubKey     []byte
	claimReceiveScript     []byte
	claimSessionID         string
	claimRecoveryID        string
	claimRecoveryFailureAt time.Time
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
		return fmt.Errorf("receive session stopped in terminal "+
			"state %s", s.state)
	}
}

// failTerminal persists one safely-classified receive-side terminal failure and
// returns the canonical failure error for the blocking API surface.
func (s *ReceiveSession) failTerminal(ctx context.Context, reason string,
	cause error, mutate func()) error {

	if s == nil {
		return newFailureError(reason, cause)
	}

	s.client.log.WarnS(ctx, "Receive swap failed",
		cause,
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
	amountSat btcutil.Amount, memo ...string) (*ReceiveSession, error) {

	if err := validateSatoshiAmount(
		amountSat, "receive amount",
	); err != nil {
		return nil, err
	}

	session := &ReceiveSession{
		client:    c,
		amountSat: amountSat,
		memo:      receiveInvoiceMemo(memo),
		state:     ReceiveStateCreated,
	}

	if err := session.runUntil(
		ctx, ReceiveStateInvoiceCreated,
	); err != nil {
		return nil, err
	}

	return session, nil
}

// receiveInvoiceMemo returns the optional invoice memo passed by the caller.
func receiveInvoiceMemo(memo []string) string {
	if len(memo) == 0 {
		return ""
	}

	return memo[0]
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
		return nil, fmt.Errorf("cannot claim receive session before " +
			"out-swap HTLC event is accepted")
	}

	if s.state == ReceiveStateHTLCEventAccepted {
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
		if err := s.ensureReceiveClaimRecoveryArmed(ctx); err != nil {
			return nil, err
		}
	}

	if s.state != ReceiveStateVHTLCFunded &&
		s.state != ReceiveStateClaimInitiated &&
		s.state != ReceiveStateCompleted {
		return nil, fmt.Errorf("cannot claim receive session in "+
			"state %s", s.state)
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
	if s.vhtlcPolicy == nil || len(s.vhtlcPkScript) == 0 {
		return nil, fmt.Errorf("out-swap HTLC event has not been " +
			"accepted yet")
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

	responderCtx, stopResponder := context.WithCancel(ctx)
	defer stopResponder()

	if s.client != nil && receiveTargetNeedsForfeitResponder(target) {
		receiver, _ :=
			s.client.outEvents.(OutSwapForfeitSignatureReceiver)
		paymentHash := s.PaymentHash
		clientPubKey := s.clientPubKey
		if receiver != nil && clientPubKey != nil {
			go s.respondToOutSwapForfeitSignatureRequests(
				responderCtx, receiver, paymentHash,
				clientPubKey,
			)
		}
	}

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

func receiveTargetNeedsForfeitResponder(target ReceiveState) bool {
	return target == ReceiveStateVHTLCFunded ||
		target == ReceiveStateClaimInitiated ||
		target == ReceiveStateCompleted
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
		return fmt.Errorf("invoice generator required for " +
			"ReceiveViaLightning")
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
	paymentHash := preimage.Hash()

	authKey, err := s.client.receiveAuthKey(ctx, paymentHash)
	if err != nil {
		return fmt.Errorf("get receive auth key: %w", err)
	}

	claimReceiveInfo, err := s.client.daemon.AllocateReceiveScript(ctx, "")
	if err != nil {
		return fmt.Errorf("allocate claim receive script: %w", err)
	}
	if claimReceiveInfo == nil {
		return fmt.Errorf("claim receive script is required")
	}
	if len(claimReceiveInfo.PubKeyXOnly) == 0 {
		return fmt.Errorf("claim receive pubkey is required")
	}
	if len(claimReceiveInfo.PkScript) == 0 {
		return fmt.Errorf("claim receive script is required")
	}

	// Receive setup is the one session edge that prepares external
	// metadata before the first durable row exists. The server's route hint
	// allocation is lease-like, and the SDK invoice generator only encodes
	// a signed payment request instead of registering it with an LN invoice
	// database. The session is persisted before the invoice is returned to
	// the caller.
	expiry := time.Duration(defaultReceiveExpirySeconds) * time.Second

	// Only advertise the in-ark credit capability when the wired event
	// receiver can actually pull in-ark events: this session validates
	// credit-shaped events, but a receiver limited to the out-swap
	// interface would never surface one, and an advertised-but-unservable
	// capability would leave a same-Ark credit receive waiting out the
	// invoice deadline.
	_, supportsInArkCredit :=
		s.client.outEvents.(IncomingVHTLCEventReceiver)
	quote, err := s.client.server.RequestChannelID(
		ctx, clientKey, paymentHash, s.amountSat,
		defaultReceiveExpirySeconds, supportsInArkCredit,
	)
	if err != nil {
		return fmt.Errorf("request channel ID: %w", err)
	}
	if quote == nil || len(quote.RouteHintPaths) == 0 {
		return fmt.Errorf("route quote must be provided")
	}
	hintPaths := quote.RouteHintPaths

	// Every alternative path must terminate at the same virtual channel:
	// the sender can enter through any backend, but all of them intercept
	// into the one vSCID this receive is registered under. A divergent
	// final hop would let a malformed quote split the receive across two
	// different registrations.
	var finalHop *RouteHint
	for i, hintPath := range hintPaths {
		if len(hintPath) == 0 {
			return fmt.Errorf("route hint path %d is empty", i)
		}
		lastHop := hintPath[len(hintPath)-1]
		if finalHop == nil {
			finalHop = lastHop
			continue
		}
		if lastHop.ChannelID != finalHop.ChannelID {
			return fmt.Errorf("route hint path %d terminates at "+
				"channel %d, want %d", i, lastHop.ChannelID,
				finalHop.ChannelID)
		}
	}

	requestedAmountSat := quote.RequestedAmountSat
	if requestedAmountSat == 0 {
		requestedAmountSat = uint64(s.amountSat)
	}
	if requestedAmountSat != uint64(s.amountSat) {
		return fmt.Errorf("route quote amount %d does not match "+
			"requested receive amount %d", requestedAmountSat,
			s.amountSat)
	}
	expectedVHTLCSat := quote.VHTLCAmountSat
	if expectedVHTLCSat == 0 {
		expectedVHTLCSat = requestedAmountSat
	}
	if quote.AttachedCreditSat > ^uint64(0)-requestedAmountSat {
		return fmt.Errorf("route quote attached credit overflows " +
			"vHTLC amount")
	}
	if quote.AttachedCreditSat > 0 &&
		expectedVHTLCSat != requestedAmountSat+quote.AttachedCreditSat {
		return fmt.Errorf("route quote vHTLC amount %d does not equal "+
			"requested amount %d plus attached credit %d",
			expectedVHTLCSat, requestedAmountSat,
			quote.AttachedCreditSat)
	}

	s.client.log.InfoS(ctx, "Received route hint from swap server",
		slog.Uint64("channel_id", finalHop.ChannelID),
		slog.Int("path_count", len(hintPaths)),
		slog.Int("path_hops", len(hintPaths[0])),
		slog.Uint64("payer_fee_msat", quote.PayerFeeMsat),
		slog.Uint64("attached_credit_sat", quote.AttachedCreditSat),
		slog.Uint64("vhtlc_amount_sat", expectedVHTLCSat),
	)

	inv, hash, err := s.client.invoiceGen.
		CreateInvoiceWithKeyRouteHintPaths(
			ctx, s.amountSat, s.memo, hintPaths, expiry, authKey,
			&preimage,
		)
	if err != nil {
		return fmt.Errorf("create invoice: %w", err)
	}
	if hash != paymentHash {
		return fmt.Errorf("invoice hash does not match route hash")
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
		s.payerFeeMsat = quote.PayerFeeMsat
		s.requestedAmountSat = requestedAmountSat
		s.availableCreditSat = quote.AvailableCreditSat
		s.attachedCreditSat = quote.AttachedCreditSat
		s.expectedVHTLCSat = expectedVHTLCSat
		s.dustLimitSat = quote.DustLimitSat
		s.settlementType = quote.SettlementType
		s.deadline = s.client.currentTime().Add(expiry)
		if s.createdAt.IsZero() {
			s.createdAt = s.client.currentTime()
		}
		s.clientPubKey = clientKey
		s.operatorPubKey = operatorKey
		s.paymentAddr = inv.Terms.PaymentAddr
		s.claimReceivePubKey = append(
			[]byte(nil), claimReceiveInfo.PubKeyXOnly...,
		)
		s.claimReceiveScript = append(
			[]byte(nil), claimReceiveInfo.PkScript...,
		)

		return s.transition(receiveEventInvoiceCreated)
	})
}

// waitForHTLCEvent waits until the swap server delivers the HTLC event,
// validates it, then persists the accepted event before acking the mailbox.
func (s *ReceiveSession) waitForHTLCEvent(ctx context.Context) error {
	if s.client.outEvents == nil {
		return fmt.Errorf("out-swap event receiver is not configured")
	}

	authKey, err := s.client.receiveAuthKey(ctx, s.PaymentHash)
	if err != nil {
		return fmt.Errorf("get receive auth key: %w", err)
	}

	waitCtx, cancel := s.invoiceDeadlineContext(ctx)
	defer cancel()

	notification, err := s.waitIncomingVHTLCNotification(
		waitCtx, authKey,
	)
	if err != nil {
		if invoiceDeadlineExceeded(ctx, waitCtx, err) {
			return fmt.Errorf("receive invoice deadline "+
				"elapsed: %w", errSwapExpired)
		}

		return err
	}
	if notification == nil {
		return fmt.Errorf("incoming vHTLC notification must be " +
			"provided")
	}
	if notification.Ack != nil && notification.AckCursor == 0 {
		return fmt.Errorf("incoming vHTLC ack cursor must be provided")
	}

	return s.ackAcceptedHTLCEvent(ctx, notification.Ack)
}

// invoiceDeadlineContext bounds only the unpaid-invoice mailbox wait. When a
// daemon resumes after the invoice deadline, it still gives mailbox delivery a
// short grace window to surface an already-delivered HTLC event before the
// receive is expired.
func (s *ReceiveSession) invoiceDeadlineContext(ctx context.Context) (
	context.Context, context.CancelFunc) {

	if s.client == nil || s.deadline.IsZero() {
		return context.WithCancel(ctx)
	}

	now := s.client.currentTime()
	if now.Before(s.deadline) {
		return context.WithDeadline(ctx, s.deadline)
	}

	return context.WithTimeout(ctx, s.client.overdueReceivePollWindow)
}

// invoiceDeadlineExceeded reports whether the receive invoice's own deadline
// context expired, rather than the caller/root context interrupting the wait.
func invoiceDeadlineExceeded(parent, waitCtx context.Context, err error) bool {
	if err == nil || waitCtx.Err() == nil || parent.Err() != nil {
		return false
	}

	return isDeadlineExceededErr(err) || errors.Is(
		waitCtx.Err(), context.DeadlineExceeded,
	)
}

// waitForFunding waits until the expected accepted vHTLC is indexed as live.
func (s *ReceiveSession) waitForFunding(ctx context.Context) error {
	if err := s.ackAcceptedHTLCEvent(ctx, nil); err != nil {
		return err
	}

	if s.vhtlcPolicy == nil || len(s.vhtlcPkScript) == 0 {
		return fmt.Errorf("out-swap HTLC event has not been accepted " +
			"yet")
	}

	// Once the HTLC event is accepted, the server may already have funded
	// the vHTLC. Do not add a wall-clock deadline here: the refund-locktime
	// guard below is the durable boundary, and backend/indexer outages
	// should keep retrying until the caller shuts the worker down.
	outpoint, amount, err := s.client.waitForVHTLC(
		ctx, s.vhtlcPkScript, time.Time{},
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

	err = s.mutateAndPersist(ctx, func() error {
		s.vhtlcOutpoint = outpoint
		s.vhtlcAmount = amount

		return s.transition(receiveEventVHTLCFunded)
	})
	if err != nil {
		return err
	}

	return s.ensureReceiveClaimRecoveryArmed(ctx)
}

// waitIncomingVHTLCNotification waits for and validates the server notification
// that tells this receiver which vHTLC script should be funded.
func (s *ReceiveSession) waitIncomingVHTLCNotification(ctx context.Context,
	authKey ReceiveAuthKey) (*IncomingVHTLCNotification, error) {

	if receiver, ok := s.client.outEvents.(IncomingVHTLCEventReceiver); ok {
		notification, err := receiver.WaitIncomingVHTLC(
			ctx, s.PaymentHash, s.clientPubKey,
		)
		if err != nil {
			return nil, fmt.Errorf("wait for incoming vHTLC "+
				"event: %w", err)
		}
		if err := validateIncomingVHTLCAck(notification); err != nil {
			return nil, err
		}

		return s.acceptIncomingVHTLCNotification(
			ctx, notification, authKey,
		)
	}

	notification, err := s.client.outEvents.WaitOutSwapHtlc(
		ctx, s.PaymentHash, s.clientPubKey,
	)
	if err != nil {
		return nil, fmt.Errorf("wait for out-swap HTLC event: %w", err)
	}
	if notification == nil {
		return nil, fmt.Errorf("out-swap HTLC notification must be " +
			"provided")
	}

	incoming := &IncomingVHTLCNotification{
		OutSwap:   notification.Event,
		AckCursor: notification.AckCursor,
		Ack:       notification.Ack,
	}
	if err := validateIncomingVHTLCAck(incoming); err != nil {
		return nil, err
	}

	return s.acceptIncomingVHTLCNotification(ctx, incoming, authKey)
}

// validateIncomingVHTLCAck checks mailbox ack metadata before the notification
// is durably accepted.
func validateIncomingVHTLCAck(notification *IncomingVHTLCNotification) error {
	if notification == nil {
		return fmt.Errorf("incoming vHTLC notification must be " +
			"provided")
	}
	if notification.Ack != nil && notification.AckCursor == 0 {
		return fmt.Errorf("incoming vHTLC ack cursor must be provided")
	}

	return nil
}

// acceptIncomingVHTLCNotification validates and persists either incoming vHTLC
// event shape.
func (s *ReceiveSession) acceptIncomingVHTLCNotification(ctx context.Context,
	notification *IncomingVHTLCNotification, authKey ReceiveAuthKey) (
	*IncomingVHTLCNotification, error) {

	if notification == nil {
		return nil, fmt.Errorf("incoming vHTLC notification must be " +
			"provided")
	}

	switch {
	case notification.OutSwap != nil:
		if err := s.acceptOutSwapHtlcEvent(
			ctx, notification.OutSwap, authKey,
			notification.AckCursor,
		); err != nil {
			return nil, err
		}

	case notification.InArk != nil:
		if err := s.acceptInArkHtlcEvent(
			ctx, notification.InArk, notification.AckCursor,
		); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("incoming vHTLC event missing payload")
	}

	return notification, nil
}

// acceptOutSwapHtlcEvent validates the server's funded HTLC notification and
// builds the vHTLC policy that the client will later claim.
func (s *ReceiveSession) acceptOutSwapHtlcEvent(ctx context.Context,
	event *OutSwapHtlcEvent, authKey ReceiveAuthKey,
	ackCursor uint64) error {

	if event == nil {
		return fmt.Errorf("out-swap HTLC event must be provided")
	}
	if authKey == nil {
		return fmt.Errorf("receive auth key must be provided")
	}
	if event.PaymentHash != s.PaymentHash {
		return s.failTerminal(
			ctx, "out-swap HTLC event payment hash mismatch", nil,
			nil,
		)
	}
	requestedAmountSat := event.RequestedAmountSat
	if requestedAmountSat == 0 {
		requestedAmountSat = uint64(s.amountSat)
	}
	if requestedAmountSat != uint64(s.amountSat) {
		return s.failTerminal(
			ctx, fmt.Sprintf("out-swap HTLC requested amount %d "+
				"does not match invoice amount %d",
				requestedAmountSat, s.amountSat),
			nil,
			nil,
		)
	}
	if event.AttachedCreditSat != s.attachedCreditSat {
		return s.failTerminal(
			ctx, fmt.Sprintf("out-swap HTLC attached credit %d "+
				"does not match expected credit %d",
				event.AttachedCreditSat, s.attachedCreditSat),
			nil,
			nil,
		)
	}
	expectedVHTLCSat := s.expectedVHTLCSat
	if expectedVHTLCSat == 0 {
		expectedVHTLCSat = uint64(s.amountSat)
	}
	if event.AmountSat != int64(expectedVHTLCSat) {
		return s.failTerminal(
			ctx, fmt.Sprintf("out-swap HTLC amount %d does not "+
				"match expected vHTLC amount %d",
				event.AmountSat, expectedVHTLCSat),
			nil,
			nil,
		)
	}
	if err := s.validateOnionPayload(event, authKey); err != nil {
		return s.failTerminal(
			ctx, "out-swap HTLC onion validation failed", err, nil,
		)
	}

	serverKey, err := btcec.ParsePubKey(
		event.VHTLCConfig.SwapServerPubkey,
	)
	if err != nil {
		return fmt.Errorf("parse server pubkey: %w", err)
	}

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:       serverKey,
		Receiver:     s.clientPubKey,
		Server:       s.operatorPubKey,
		PreimageHash: s.PaymentHash,
		RefundLocktime: event.VHTLCConfig.
			RefundLocktime,
		UnilateralClaimDelay: event.VHTLCConfig.
			UnilateralClaimDelay,
		UnilateralRefundDelay: event.VHTLCConfig.
			UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: event.VHTLCConfig.
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

	return s.mutateAndPersist(ctx, func() error {
		s.swapServerPubKey = serverKey
		if s.settlementType == "" {
			s.settlementType = SettlementTypeLightning
		}
		s.vhtlcConfig = event.VHTLCConfig
		s.vhtlcPolicy = policy
		s.vhtlcPolicyTemplate = policyTemplate
		s.vhtlcPkScript = pkScript
		s.pendingHTLCAckCursor = ackCursor

		return s.transition(receiveEventHTLCEventAccepted)
	})
}

// ackAcceptedHTLCEvent retries any mailbox ack that is still pending after the
// HTLC event was durably accepted.
func (s *ReceiveSession) ackAcceptedHTLCEvent(ctx context.Context,
	ack func(context.Context) error) error {

	if s.pendingHTLCAckCursor == 0 {
		return nil
	}

	if ack == nil {
		if s.client.outEvents == nil {
			return fmt.Errorf("out-swap event receiver is not " +
				"configured")
		}

		ack = func(ctx context.Context) error {
			return s.client.outEvents.AckOutSwapHtlc(
				ctx, s.PaymentHash, s.clientPubKey,
				s.pendingHTLCAckCursor,
			)
		}
	}

	if err := ack(ctx); err != nil {
		return newRetryableActionError(
			fmt.Errorf("ack out-swap HTLC event: %w", err),
		)
	}

	if err := s.acknowledgeOutSwapHTLC(ctx); err != nil {
		return err
	}

	if err := s.clearPendingHTLCAck(ctx); err != nil {
		return newRetryableActionError(err)
	}

	return nil
}

// acknowledgeOutSwapHTLC records the receiver's durable event acceptance with
// the swap server before clearing the local pending ACK marker.
func (s *ReceiveSession) acknowledgeOutSwapHTLC(ctx context.Context) error {
	// The legacy direct p2p rail has no server-side funding gate: the
	// sender already funded the vHTLC before the event was published, so
	// there is nothing to acknowledge. The credit-shaped in-ark leg is
	// different: the swap server funds the padded vHTLC only after this
	// ACK proves the receiver durably accepted the event, exactly like
	// the Lightning out-swap rail.
	if s.settlementType == SettlementTypeInArk &&
		s.attachedCreditSat == 0 {
		return nil
	}
	if s.client == nil || s.client.server == nil {
		return fmt.Errorf("swap server connection is not configured")
	}
	if s.clientPubKey == nil {
		return fmt.Errorf("client vHTLC pubkey is not configured")
	}

	if err := s.client.server.AcknowledgeOutSwapHTLC(
		ctx, s.PaymentHash, s.clientPubKey,
	); err != nil {

		err = fmt.Errorf("acknowledge out-swap HTLC: %w", err)
		if isTerminalOutSwapHTLCAckError(err) {
			return newFailureError(
				"swap server rejected out-swap HTLC ack", err,
			)
		}

		return newRetryableActionError(err)
	}

	return nil
}

// isTerminalOutSwapHTLCAckError reports authoritative server ACK rejections
// that cannot be fixed by retrying the durable pending cursor. The server uses
// FailedPrecondition for the transient "not published yet" state, so that code
// deliberately remains retryable.
func isTerminalOutSwapHTLCAckError(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.NotFound, codes.PermissionDenied:
		return true

	default:
		return false
	}
}

// clearPendingHTLCAck records that the accepted HTLC event's mailbox cursor
// was durably acknowledged.
func (s *ReceiveSession) clearPendingHTLCAck(ctx context.Context) error {
	return s.mutateAndPersist(ctx, func() error {
		s.pendingHTLCAckCursor = 0

		return nil
	})
}

// validateCreditShapedInArkEvent checks a swap-server-funded in-ark event
// against the session's credit-attach route quote. Every field of the
// (requested amount, attached credit, padded amount) triplet must match: the
// event commits this session to a claim of the padded vHTLC, and the server
// finalizes the receiver's attach reservation against that claim.
func (s *ReceiveSession) validateCreditShapedInArkEvent(
	event *InArkHtlcEvent) error {

	if s.attachedCreditSat == 0 {
		return fmt.Errorf("credit-shaped in-ark HTLC event with "+
			"attached credit %d sat for a session without a "+
			"credit-attach plan", event.AttachedCreditSat)
	}
	if event.RequestedAmountSat != uint64(s.amountSat) {
		return fmt.Errorf("in-ark HTLC requested amount %d does not "+
			"match invoice amount %d", event.RequestedAmountSat,
			s.amountSat)
	}
	if event.AttachedCreditSat != s.attachedCreditSat {
		return fmt.Errorf("in-ark HTLC attached credit %d does not "+
			"match expected credit %d", event.AttachedCreditSat,
			s.attachedCreditSat)
	}
	if event.AmountSat != int64(s.expectedVHTLCSat) {
		return fmt.Errorf("in-ark HTLC amount %d does not match "+
			"expected vHTLC amount %d", event.AmountSat,
			s.expectedVHTLCSat)
	}

	return nil
}

// acceptInArkHtlcEvent validates a direct same-Ark vHTLC notification and
// builds the policy that the receiver will later claim.
func (s *ReceiveSession) acceptInArkHtlcEvent(ctx context.Context,
	event *InArkHtlcEvent, ackCursor uint64) error {

	if event == nil {
		return fmt.Errorf("in-ark HTLC event must be provided")
	}
	if event.PaymentHash != s.PaymentHash {
		return s.failTerminal(
			ctx, "in-ark HTLC event payment hash mismatch", nil,
			nil,
		)
	}

	if event.RequestedAmountSat > 0 || event.AttachedCreditSat > 0 {
		// A credit-shaped event is the swap-server-funded in-ark leg
		// of a credit-attach receive: amount_sat carries the padded
		// vHTLC and the event must match the session's route quote
		// triplet exactly.
		if err := s.validateCreditShapedInArkEvent(event); err != nil {
			return s.failTerminal(ctx, err.Error(), nil, nil)
		}
	} else {
		// A legacy direct p2p event cannot settle a session whose
		// route quote attached credit (or padded the vHTLC above the
		// invoice amount): the sender funds the receiver's script
		// directly, so no output exists that could carry the server's
		// custodial credit top-up. Accepting such an event would
		// commit this session to the in-ark rail and leave it polling
		// a vHTLC that can never be funded, so fail fast instead.
		if s.attachedCreditSat > 0 {
			return s.failTerminal(
				ctx, fmt.Sprintf("in-ark HTLC event "+
					"conflicts with credit-attach "+
					"receive plan: attached credit %d "+
					"sat requires a credit-shaped event",
					s.attachedCreditSat),
				nil,
				nil,
			)
		}
		// Note that expectedVHTLCSat is not persisted directly: a
		// restored session reconstructs it as requested plus
		// attachedCredit, so a padded quote with zero attached credit
		// would lose its padding across a restart. No server produces
		// that shape today; if one ever does, the expected amount
		// needs its own column.
		if s.expectedVHTLCSat != 0 &&
			s.expectedVHTLCSat != uint64(s.amountSat) {
			return s.failTerminal(
				ctx, fmt.Sprintf("in-ark HTLC event "+
					"conflicts with padded vHTLC receive "+
					"plan: expected vHTLC %d sat does "+
					"not match invoice amount %d sat",
					s.expectedVHTLCSat, s.amountSat),
				nil,
				nil,
			)
		}
		if event.AmountSat != int64(s.amountSat) {
			return s.failTerminal(
				ctx, fmt.Sprintf("in-ark HTLC amount %d does "+
					"not match invoice amount %d",
					event.AmountSat, s.amountSat),
				nil,
				nil,
			)
		}
	}
	if event.SenderPubkey == nil {
		return fmt.Errorf("in-ark HTLC sender pubkey is required")
	}
	cfg := event.VHTLCConfig
	cfgSenderKey, err := btcec.ParsePubKey(cfg.SwapServerPubkey)
	if err != nil {
		return fmt.Errorf("parse in-ark vHTLC sender pubkey: %w", err)
	}
	if !cfgSenderKey.IsEqual(event.SenderPubkey) {
		return s.failTerminal(
			ctx, "in-ark HTLC sender pubkey mismatch", nil, nil,
		)
	}
	refundNoReceiverDelay := cfg.UnilateralRefundWithoutReceiverDelay

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               event.SenderPubkey,
		Receiver:                             s.clientPubKey,
		Server:                               s.operatorPubKey,
		PreimageHash:                         s.PaymentHash,
		RefundLocktime:                       cfg.RefundLocktime,
		UnilateralClaimDelay:                 cfg.UnilateralClaimDelay,
		UnilateralRefundDelay:                cfg.UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: refundNoReceiverDelay,
	})
	if err != nil {
		return fmt.Errorf("build in-ark vHTLC policy: %w", err)
	}

	pkScript, err := policy.PkScript()
	if err != nil {
		return fmt.Errorf("get in-ark vHTLC pkScript: %w", err)
	}

	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	if err != nil {
		return fmt.Errorf("encode in-ark vHTLC policy: %w", err)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.swapServerPubKey = event.SenderPubkey
		s.settlementType = SettlementTypeInArk
		s.vhtlcConfig = event.VHTLCConfig
		s.vhtlcPolicy = policy
		s.vhtlcPolicyTemplate = policyTemplate
		s.vhtlcPkScript = pkScript
		s.pendingHTLCAckCursor = ackCursor

		return s.transition(receiveEventHTLCEventAccepted)
	})
}

// validateOnionPayload decodes the final-hop onion of every payment part with
// the invoice auth key and checks that each matches the prepared invoice
// fields. Events without parts carry one legacy single-part onion that must
// forward the full invoice amount on its own.
func (s *ReceiveSession) validateOnionPayload(event *OutSwapHtlcEvent,
	authKey ReceiveAuthKey) error {

	if authKey == nil {
		return fmt.Errorf("receive auth key must be provided")
	}

	expectedMsat := lnwire.NewMSatFromSatoshis(s.amountSat)

	// Legacy single-part events carry the lone onion in the event body and
	// the shard must forward the full invoice amount.
	parts := event.Parts
	if len(parts) == 0 {
		parts = []OutSwapHtlcPart{{
			AmountMsat: expectedMsat,
			OnionBlob:  event.OnionBlob,
		}}
	}

	// Every shard must commit to the invoice payment address and total,
	// while individual forwarded amounts only need to sum to the total.
	var sumMsat lnwire.MilliSatoshi
	for idx, part := range parts {
		payload, err := s.client.decodeOutSwapOnion(
			authKey, s.PaymentHash, part.OnionBlob,
		)
		if err != nil {
			return fmt.Errorf("part %d: %w", idx, err)
		}

		if payload.amountToForward == 0 {
			return fmt.Errorf("part %d: onion forwards zero amount",
				idx)
		}
		if payload.amountToForward > expectedMsat {
			return fmt.Errorf("part %d: onion amount %d msat "+
				"exceeds invoice amount %d msat", idx,
				payload.amountToForward, expectedMsat)
		}
		if payload.amountToForward != part.AmountMsat {
			return fmt.Errorf("part %d: onion amount %d msat does "+
				"not match part amount %d msat", idx,
				payload.amountToForward, part.AmountMsat)
		}
		if !payload.hasMPP {
			return fmt.Errorf("part %d: onion missing MPP "+
				"payment address", idx)
		}
		if payload.paymentAddr != s.paymentAddr {
			return fmt.Errorf("part %d: onion payment address "+
				"mismatch", idx)
		}
		if payload.totalAmount != expectedMsat {
			return fmt.Errorf("part %d: onion total amount %d "+
				"msat does not match invoice amount %d msat",
				idx, payload.totalAmount, expectedMsat)
		}

		sumMsat += payload.amountToForward
	}

	if sumMsat != expectedMsat {
		return fmt.Errorf("onion amounts sum to %d msat, invoice "+
			"amount is %d msat", sumMsat, expectedMsat)
	}

	return nil
}

// decodeOutSwapOnion decodes the final-hop onion with the invoice auth key.
func decodeOutSwapOnion(receiveAuthKey ReceiveAuthKey, paymentHash lntypes.Hash,
	onionBlob []byte) (*decodedOutSwapOnion, error) {

	router := sphinx.NewRouter(
		receiveAuthKey, sphinx.NewMemoryReplayLog(),
	)
	processor := hop.NewOnionProcessor(router)
	if err := processor.Start(); err != nil {
		return nil, fmt.Errorf("start onion processor: %w", err)
	}
	defer func() {
		_ = processor.Stop()
	}()

	iterator, err := processor.ReconstructHopIterator(
		bytes.NewReader(onionBlob), paymentHash[:],
		hop.ReconstructBlindingInfo{},
	)
	if err != nil {
		return nil, fmt.Errorf("decode onion: %w", err)
	}

	payload, _, err := iterator.HopPayload()
	if err != nil {
		return nil, fmt.Errorf("decode hop payload: %w", err)
	}

	result := &decodedOutSwapOnion{
		amountToForward: payload.ForwardingInfo().AmountToForward,
	}
	if payload.MPP != nil {
		result.hasMPP = true
		result.paymentAddr = payload.MPP.PaymentAddr()
		result.totalAmount = payload.MPP.TotalMsat()
	}

	return result, nil
}

// ensureReceiveFundingStillPossible stops an invoice-created receive session
// once the vHTLC refund path has matured and no live claimable vHTLC was found.
func (s *ReceiveSession) ensureReceiveFundingStillPossible(
	ctx context.Context) error {

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		s.client.log.DebugS(
			ctx,
			"Unable to query receive refund locktime height",
			slog.String("err", err.Error()),
			btclog.Hex("hash", s.PaymentHash[:]),
		)

		return nil
	}

	if height+s.client.refundLocktimeBuffer < s.vhtlcConfig.RefundLocktime {
		return nil
	}

	return fmt.Errorf("refund locktime %d is imminent or reached before "+
		"receive funding was observed at height %d: %w",
		s.vhtlcConfig.RefundLocktime, height, errSwapExpired)
}

// validateReceiveFunding checks manually or automatically observed funding
// details before the claim path can reveal the invoice preimage.
func (s *ReceiveSession) validateReceiveFunding(ctx context.Context,
	outpoint string, amount int64) error {

	expectedAmountSat := s.expectedVHTLCSat
	if expectedAmountSat == 0 {
		expectedAmountSat = uint64(s.amountSat)
	}

	if amount != int64(expectedAmountSat) {
		return s.failTerminal(ctx, fmt.Sprintf("funded "+
			"vHTLC amount %d does not match "+
			"expected vHTLC amount %d", amount,
			expectedAmountSat), nil, func() {
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
		return s.failTerminal(ctx, fmt.Sprintf("funded "+
			"vHTLC observed too close to refund "+
			"locktime %d at height %d",
			s.vhtlcConfig.RefundLocktime, height), nil, func() {
			s.vhtlcOutpoint = outpoint
			s.vhtlcAmount = amount
		})
	}

	return nil
}

// claimFundedVHTLC reconciles an already-spent vHTLC before sending the claim
// transaction, then submits the preimage claim if no spend is indexed yet.
func (s *ReceiveSession) claimFundedVHTLC(ctx context.Context) error {
	if s.claimSessionID != "" {
		if err := cancelVHTLCRecovery(
			ctx, s.client.daemon, s.claimRecoveryID,
			recoveryReasonClaimAccepted, "",
		); err != nil {
			return newRetryableActionError(err)
		}

		// A persisted claim session ID means the daemon accepted the
		// custom-input claim spend before this attempt. Do not submit
		// another claim for the same vHTLC.
		return s.mutateAndPersist(ctx, func() error {
			return s.transition(receiveEventCompleted)
		})
	}

	if s.state == ReceiveStateClaimInitiated &&
		!s.claimIntentRecordedInProcess {

		// A restored ClaimInitiated row without in-process intent
		// may have submitted before restart but failed to persist the
		// returned session ID, so reconcile fully before deciding to
		// retry.
		claimed, err := s.client.receiveClaimAlreadyIndexed(
			ctx, s.PaymentHash, s.vhtlcPkScript,
		)
		if err != nil {
			return err
		}
		if claimed {
			if err := cancelVHTLCRecovery(
				ctx, s.client.daemon, s.claimRecoveryID,
				recoveryReasonClaimIndexed, "",
			); err != nil {
				return newRetryableActionError(err)
			}

			return s.mutateAndPersist(ctx, func() error {
				return s.transition(receiveEventCompleted)
			})
		}
	}

	if s.claimIntentRecordedInProcess {
		// A fresh in-process claim only gets a bounded spend check. A
		// slow indexer must not consume the caller's context before the
		// claim submission below.
		claimed, err := s.client.receiveClaimAlreadyIndexedBounded(
			ctx, s.PaymentHash, s.vhtlcPkScript,
		)
		if err != nil {
			return err
		}
		if claimed {
			if err := cancelVHTLCRecovery(
				ctx, s.client.daemon, s.claimRecoveryID,
				recoveryReasonClaimIndexed, "",
			); err != nil {
				return newRetryableActionError(err)
			}

			return s.mutateAndPersist(ctx, func() error {
				return s.transition(receiveEventCompleted)
			})
		}
	}

	if err := s.waitForClaimResumeGrace(ctx); err != nil {
		return err
	}

	if err := s.ensureClaimReceiveInfo(ctx); err != nil {
		return err
	}
	if err := s.ensureReceiveClaimRecoveryArmed(ctx); err != nil {
		return err
	}

	recoveryHandled, err := s.reconcileReceiveClaimRecovery(ctx)
	if err != nil {
		return newRetryableActionError(err)
	}
	if recoveryHandled {
		return nil
	}

	if err := s.ensureReceiveClaimStillPossible(ctx); err != nil {
		return err
	}
	if err := s.reconcileLiveReceiveFunding(ctx); err != nil {
		return err
	}

	claimSessionID, err := s.client.claimReceiveVHTLC(
		ctx, s.PaymentHash, s.Preimage, s.vhtlcPolicy,
		s.vhtlcPolicyTemplate, s.vhtlcPkScript, s.vhtlcOutpoint,
		s.vhtlcAmount, s.claimReceivePubKey,
	)
	if errors.Is(err, errReceiveClaimAlreadyIndexed) {
		if cancelErr := cancelVHTLCRecovery(
			ctx, s.client.daemon, s.claimRecoveryID,
			recoveryReasonClaimIndexed, "",
		); cancelErr != nil {
			return newRetryableActionError(cancelErr)
		}

		// Spent-without-preimage is terminal for retry purposes inside
		// claimReceiveVHTLC; this branch only handles a matching
		// preimage-backed spend observed during retry reconciliation.
		return s.mutateAndPersist(ctx, func() error {
			return s.transition(receiveEventCompleted)
		})
	}
	if err != nil {
		if escalateErr := s.maybeEscalateReceiveClaimRecovery(
			ctx, err,
		); escalateErr != nil {
			return newRetryableActionError(escalateErr)
		}

		return waitForFixedPoll(ctx, s.client.waitPollInterval)
	}
	if claimSessionID == "" {
		return fmt.Errorf("claim vHTLC returned empty session id")
	}

	if err := s.persistClaimSessionID(ctx, claimSessionID); err != nil {
		return newRetryableActionError(err)
	}
	s.claimIntentRecordedInProcess = false
	if err := cancelVHTLCRecovery(
		ctx, s.client.daemon, s.claimRecoveryID,
		recoveryReasonClaimAccepted, "",
	); err != nil {
		return newRetryableActionError(err)
	}

	if err := s.mutateAndPersist(ctx, func() error {
		return s.transition(receiveEventCompleted)
	}); err != nil {
		return newRetryableActionError(err)
	}

	return nil
}

// ensureClaimReceiveInfo recovers a missing claim destination for legacy or
// manually constructed sessions before submitting the claim spend.
func (s *ReceiveSession) ensureClaimReceiveInfo(ctx context.Context) error {
	if len(s.claimReceivePubKey) != 0 && len(s.claimReceiveScript) != 0 {
		return nil
	}

	receiveInfo, err := s.client.daemon.AllocateReceiveScript(ctx, "")
	if err != nil {
		return fmt.Errorf("allocate claim receive script: %w", err)
	}
	if receiveInfo == nil {
		return fmt.Errorf("claim receive script is required")
	}
	if len(receiveInfo.PubKeyXOnly) == 0 {
		return fmt.Errorf("claim receive pubkey is required")
	}
	if len(receiveInfo.PkScript) == 0 {
		return fmt.Errorf("claim receive script is required")
	}

	return s.mutateAndPersist(ctx, func() error {
		s.claimReceivePubKey = append(
			[]byte(nil), receiveInfo.PubKeyXOnly...,
		)
		s.claimReceiveScript = append(
			[]byte(nil), receiveInfo.PkScript...,
		)

		return nil
	})
}

// ensureReceiveClaimStillPossible stops a new claim attempt once the refund
// path is mature enough for the swap server to recover the vHTLC.
func (s *ReceiveSession) ensureReceiveClaimStillPossible(
	ctx context.Context) error {

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return fmt.Errorf("get block height: %w", err)
	}

	if height+s.client.refundLocktimeBuffer < s.vhtlcConfig.RefundLocktime {
		return nil
	}

	// Once the refund locktime is mature, a new receive claim becomes a
	// late race with the server refund path and should stop durably.
	reason := fmt.Sprintf("refund locktime %d is imminent or reached "+
		"before receive claim at height %d",
		s.vhtlcConfig.RefundLocktime, height)
	if err := s.mutateAndPersist(ctx, func() error {
		s.interventionReason = reason

		return s.transition(receiveEventExpired)
	}); err != nil {
		return err
	}

	return fmt.Errorf("%s: %w", reason, errSwapExpired)
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
	if !preimageMatchesHash(preimage, paymentHash) {
		return false, errReceiveVHTLCSpentWithoutPreimage
	}

	return true, nil
}

// receiveClaimAlreadyIndexedBounded performs best-effort reconciliation before
// a freshly initiated claim without letting an absent spent record consume the
// caller's whole claim context.
func (c *SwapClient) receiveClaimAlreadyIndexedBounded(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte) (bool, error) {

	reconcileCtx, cancel := context.WithTimeout(ctx, c.waitPollInterval)
	defer cancel()

	claimed, err := c.receiveClaimAlreadyIndexed(
		reconcileCtx, paymentHash, pkScript,
	)
	if err != nil {
		// Swallow the bounded reconcile timeout however the
		// transport encoded it. gRPC wraps a tripped client
		// deadline as a status error that does not unwrap to
		// context.DeadlineExceeded, so check the inner ctx and the
		// gRPC status code in addition to errors.Is.
		boundedTimedOut := reconcileCtx.Err() != nil &&
			ctx.Err() == nil
		if boundedTimedOut || isDeadlineExceededErr(err) {
			c.log.DebugS(
				ctx,
				"Timed out checking indexed receive claim",
				slog.String("err", err.Error()),
				btclog.Hex("hash", paymentHash[:]),
			)

			return false, nil
		}

		return false, err
	}

	return claimed, nil
}

// claimReceiveVHTLC claims one funded vHTLC with the session preimage into the
// wallet-owned receive pubkey prepared when the receive session was created.
func (c *SwapClient) claimReceiveVHTLC(ctx context.Context,
	paymentHash lntypes.Hash, preimage lntypes.Preimage,
	policy *arkscript.VHTLCPolicy, policyTemplate []byte, pkScript []byte,
	outpoint string, amount int64, claimReceivePubKey []byte) (string,
	error) {

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

	if len(claimReceivePubKey) == 0 {
		return "", fmt.Errorf("claim receive pubkey is required")
	}

	var lastSendErr error
	for attempt := 1; attempt <= c.claimMaxAttempts; attempt++ {
		claimSessionID, err := c.daemon.SendOORWithCustomInputs(
			ctx, claimReceivePubKey, amount, []CustomInput{{
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
			slog.String("reason", err.Error()),
		)

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

// reconcileLiveReceiveFunding refreshes the remembered vHTLC funding row before
// a claim. A cooperative vHTLC refresh preserves the policy script but moves
// the claimable output to a new outpoint, and the replacement amount may be
// reduced by refresh fees. A delayed or resumed receive session must follow the
// authoritative live indexer row before spending.
func (s *ReceiveSession) reconcileLiveReceiveFunding(
	ctx context.Context) error {

	if s == nil || s.client == nil || len(s.vhtlcPkScript) == 0 {
		return nil
	}

	vtxo, err := s.client.daemon.FindLiveVTXOByPkScript(
		ctx, s.vhtlcPkScript,
	)
	if err != nil {
		s.client.log.DebugS(
			ctx,
			"Unable to refresh receive vHTLC outpoint",
			slog.String("err", err.Error()),
			btclog.Hex("hash", s.PaymentHash[:]),
			slog.String("outpoint", s.vhtlcOutpoint),
		)

		return nil
	}
	if vtxo == nil || vtxo.Outpoint == "" {
		return nil
	}
	if s.vhtlcAmount != 0 && vtxo.AmountSat != s.vhtlcAmount {
		s.client.log.WarnS(
			ctx,
			"Live receive vHTLC amount changed before claim",
			nil,
			btclog.Hex("hash", s.PaymentHash[:]),
			slog.String("remembered_outpoint", s.vhtlcOutpoint),
			slog.String("live_outpoint", vtxo.Outpoint),
			slog.Int64("remembered_amount_sat", s.vhtlcAmount),
			slog.Int64("live_amount_sat", vtxo.AmountSat),
		)
	}

	return s.rememberReceiveFunding(ctx, vtxo.Outpoint, vtxo.AmountSat)
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
//
// The polled pkScript is derived locally from the vHTLC policy parameters
// supplied by the swap server. Some receive paths, such as same-Ark p2p OOR,
// can materialize the vHTLC in the local daemon before the proof-gated indexer
// lookup is registered for this principal. In that case the local live VTXO set
// is authoritative enough to proceed, while complete absence remains retryable
// until the refund-locktime guard expires.
func (c *SwapClient) waitForVHTLC(ctx context.Context, pkScript []byte,
	deadline time.Time, keepWaiting func(context.Context) error) (string,
	int64, error) {

	pkScriptHex := hex.EncodeToString(pkScript)
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}

		vtxo, err := c.daemon.FindLiveVTXOByPkScript(ctx, pkScript)
		if err != nil {
			c.log.DebugS(ctx, "Unable to query vHTLC state",
				slog.String("err", err.Error()),
				slog.String("pk_script", pkScriptHex),
			)
			if isUnregisteredScriptErr(err) {
				localVTXO, localErr :=
					c.localLiveVTXOByPkScript(
						ctx, pkScript,
					)
				if localErr != nil {
					c.log.DebugS(
						ctx,
						"Unable to query local vHTLC "+
							"state",
						slog.String(
							"err", localErr.Error(),
						),
						slog.String(
							"pk_script",
							pkScriptHex,
						),
					)
				}
				if localVTXO != nil {
					c.log.InfoS(ctx,
						"Found local funded vHTLC",
						slog.String(
							"pk_script",
							pkScriptHex,
						),
						slog.String(
							"outpoint",
							localVTXO.Outpoint,
						),
						slog.Int64(
							"amount_sat",
							localVTXO.AmountSat,
						),
					)

					return localVTXO.Outpoint,
						localVTXO.AmountSat, nil
				}
			}
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

// localLiveVTXOByPkScript returns a daemon-local live VTXO matching pkScript.
// This is a fallback for OOR-delivered vHTLCs that are locally known before a
// proof-gated indexed lookup can be made under the receiver's principal.
func (c *SwapClient) localLiveVTXOByPkScript(ctx context.Context,
	pkScript []byte) (*VTXOInfo, error) {

	vtxos, err := c.daemon.ListLiveVTXOs(ctx)
	if err != nil {
		return nil, err
	}

	for i := range vtxos {
		if bytes.Equal(vtxos[i].PkScript, pkScript) {
			return &vtxos[i], nil
		}
	}

	return nil, nil
}
