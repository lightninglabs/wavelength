package swaps

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btclog/v2"
	loopfsm "github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	refreshInputFundingKeyPrefix = "refresh-input-funding:"
	refreshInputRecoveryLabel    = "refresh-input-refund"
	refreshOutputRecoveryLabel   = "refresh-output-claim"
)

// RefreshState identifies one durable boundary in the two-vHTLC refresh
// protocol.
type RefreshState uint8

const (
	// RefreshStateCreated means the client intent, preimage, exact source,
	// age cap, and recovery destinations are durable locally.
	RefreshStateCreated RefreshState = iota

	// RefreshStateSwapCreated means the server returned and the client
	// validated the first-leg vHTLC terms.
	RefreshStateSwapCreated

	// RefreshStateFundingInitiated means first-leg funding intent is
	// durable before the daemon OOR call is submitted or reconciled.
	RefreshStateFundingInitiated

	// RefreshStateInputVHTLCFunded means the exact source funded the
	// server-claimable first leg and its refund recovery is armed.
	RefreshStateInputVHTLCFunded

	// RefreshStateOutputHTLCEventAccepted means the REFRESH event
	// describing the second leg is durable before acknowledgement.
	RefreshStateOutputHTLCEventAccepted

	// RefreshStateOutputVHTLCFunded means the server-funded second leg is
	// live, has the expected amount and age, and has claim recovery armed.
	RefreshStateOutputVHTLCFunded

	// RefreshStateClaimInitiated means output claim intent is durable
	// before the client reveals the preimage in a custom-input OOR.
	RefreshStateClaimInitiated

	// RefreshStateOutputClaimed means the client claim was accepted or
	// indexed and the server can now claim the first leg with the preimage.
	RefreshStateOutputClaimed

	// RefreshStateCompleted means the client observed the server's matching
	// preimage claim of the first leg.
	RefreshStateCompleted

	// RefreshStateExpired means no first-leg funding was accepted
	// before the negotiated funding window closed.
	RefreshStateExpired

	// RefreshStateRefundInitiated means the client cannot safely reveal the
	// preimage and is recovering its funded first leg.
	RefreshStateRefundInitiated

	// RefreshStateRefunded means the first leg was recovered without
	// revealing the preimage. This is a safe terminal outcome.
	RefreshStateRefunded

	// RefreshStateNeedsIntervention preserves anomalous on-chain or
	// protocol evidence that cannot be repaired automatically.
	RefreshStateNeedsIntervention

	// RefreshStateFailed means the refresh failed before funds were
	// exposed.
	RefreshStateFailed
)

// String returns the durable name for one refresh state.
func (s RefreshState) String() string {
	switch s {
	case RefreshStateCreated:
		return "Created"

	case RefreshStateSwapCreated:
		return "SwapCreated"

	case RefreshStateFundingInitiated:
		return "FundingInitiated"

	case RefreshStateInputVHTLCFunded:
		return "InputVHTLCFunded"

	case RefreshStateOutputHTLCEventAccepted:
		return "OutputHTLCEventAccepted"

	case RefreshStateOutputVHTLCFunded:
		return "OutputVHTLCFunded"

	case RefreshStateClaimInitiated:
		return "ClaimInitiated"

	case RefreshStateOutputClaimed:
		return "OutputClaimed"

	case RefreshStateCompleted:
		return "Completed"

	case RefreshStateExpired:
		return "Expired"

	case RefreshStateRefundInitiated:
		return "RefundInitiated"

	case RefreshStateRefunded:
		return "Refunded"

	case RefreshStateNeedsIntervention:
		return "NeedsIntervention"

	case RefreshStateFailed:
		return "Failed"

	default:
		return fmt.Sprintf("Unknown(%d)", s)
	}
}

// IsTerminal reports whether no more automatic refresh work should run.
func (s RefreshState) IsTerminal() bool {
	return s == RefreshStateCompleted ||
		s == RefreshStateExpired ||
		s == RefreshStateRefunded ||
		s == RefreshStateNeedsIntervention ||
		s == RefreshStateFailed
}

type refreshEvent = loopfsm.EventType

const refreshOutputAcceptedState = RefreshStateOutputHTLCEventAccepted

const (
	refreshEventAdvance          = loopfsm.EventType("OnAdvance")
	refreshEventSwapCreated      = loopfsm.EventType("OnSwapCreated")
	refreshEventFundingInitiated = loopfsm.EventType(
		"OnFundingInitiated",
	)
	refreshEventInputVHTLCFunded = loopfsm.EventType(
		"OnInputVHTLCFunded",
	)
	refreshEventOutput = loopfsm.EventType(
		"OnOutputHTLCEventAccepted",
	)
	refreshEventOutputVHTLCFunded = loopfsm.EventType(
		"OnOutputVHTLCFunded",
	)
	refreshEventClaimInitiated    = loopfsm.EventType("OnClaimInitiated")
	refreshEventOutputClaimed     = loopfsm.EventType("OnOutputClaimed")
	refreshEventCompleted         = loopfsm.EventType("OnCompleted")
	refreshEventExpired           = loopfsm.EventType("OnExpired")
	refreshEventRefundInitiated   = loopfsm.EventType("OnRefundInitiated")
	refreshEventRefunded          = loopfsm.EventType("OnRefunded")
	refreshEventNeedsIntervention = loopfsm.EventType(
		"OnNeedsIntervention",
	)
	refreshEventFailed = loopfsm.EventType("OnFailed")
)

var refreshTransitions = map[RefreshState]map[refreshEvent]RefreshState{
	RefreshStateCreated: {
		refreshEventSwapCreated:       RefreshStateSwapCreated,
		refreshEventExpired:           RefreshStateExpired,
		refreshEventFailed:            RefreshStateFailed,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateSwapCreated: {
		refreshEventFundingInitiated:  RefreshStateFundingInitiated,
		refreshEventExpired:           RefreshStateExpired,
		refreshEventFailed:            RefreshStateFailed,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateFundingInitiated: {
		refreshEventInputVHTLCFunded:  RefreshStateInputVHTLCFunded,
		refreshEventRefundInitiated:   RefreshStateRefundInitiated,
		refreshEventExpired:           RefreshStateExpired,
		refreshEventFailed:            RefreshStateFailed,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateInputVHTLCFunded: {
		refreshEventOutput:            refreshOutputAcceptedState,
		refreshEventRefundInitiated:   RefreshStateRefundInitiated,
		refreshEventFailed:            RefreshStateFailed,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateOutputHTLCEventAccepted: {
		refreshEventOutputVHTLCFunded: RefreshStateOutputVHTLCFunded,
		refreshEventRefundInitiated:   RefreshStateRefundInitiated,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateOutputVHTLCFunded: {
		refreshEventClaimInitiated:    RefreshStateClaimInitiated,
		refreshEventRefundInitiated:   RefreshStateRefundInitiated,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateClaimInitiated: {
		refreshEventOutputClaimed:     RefreshStateOutputClaimed,
		refreshEventRefundInitiated:   RefreshStateRefundInitiated,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateOutputClaimed: {
		refreshEventCompleted:         RefreshStateCompleted,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
	RefreshStateRefundInitiated: {
		refreshEventRefunded:          RefreshStateRefunded,
		refreshEventOutputClaimed:     RefreshStateOutputClaimed,
		refreshEventNeedsIntervention: RefreshStateNeedsIntervention,
	},
}

// refreshSwapServerConn is deliberately narrower than SwapServerConn so
// existing embedders and fakes are not forced to implement the new protocol.
type refreshSwapServerConn interface {
	CreateRefreshSwap(context.Context, lntypes.Hash, btcutil.Amount,
		*btcec.PublicKey, uint32) (*RefreshSwapConfig, error)
}

// RefreshSession owns one durable two-leg VTXO refresh. It is not safe for
// concurrent method calls.
type RefreshSession struct {
	client *SwapClient

	preimage         lntypes.Preimage
	paymentHash      lntypes.Hash
	amountSat        btcutil.Amount
	sourceOutpoint   string
	maxVTXOAgeBlocks uint32
	state            RefreshState
	stateVersion     uint64
	createdAt        time.Time
	updatedAt        time.Time

	cfg            *RefreshSwapConfig
	clientPubKey   *btcec.PublicKey
	operatorPubKey *btcec.PublicKey
	serverPubKey   *btcec.PublicKey

	inputPolicy         *arkscript.VHTLCPolicy
	inputPolicyTemplate []byte
	inputPkScript       []byte
	inputOutpoint       string
	inputAmount         int64
	fundingSessionID    string
	refundReceivePubKey []byte
	refundReceiveScript []byte
	refundSessionID     string
	refundRecoveryID    string

	outputSenderPubKey   *btcec.PublicKey
	outputConfig         VHTLCConfig
	outputPolicy         *arkscript.VHTLCPolicy
	outputPolicyTemplate []byte
	outputPkScript       []byte
	outputOutpoint       string
	outputAmount         int64
	outputObservedHeight uint32
	outputCreatedHeight  int32
	outputBatchExpiry    int32
	pendingAckCursor     uint64
	claimReceivePubKey   []byte
	claimReceiveScript   []byte
	claimSessionID       string
	claimRecoveryID      string
	claimIntentInProcess bool

	inputClaimTxID          string
	interventionReason      string
	refundRecoveryFailureAt time.Time
	claimRecoveryFailureAt  time.Time
}

// State returns the current refresh state.
func (s *RefreshSession) State() RefreshState {
	if s == nil {
		return RefreshStateFailed
	}

	return s.state
}

// PaymentHash returns the shared hashlock for both refresh legs.
func (s *RefreshSession) PaymentHash() lntypes.Hash {
	if s == nil {
		return lntypes.Hash{}
	}

	return s.paymentHash
}

// TerminalReason returns the durable failure or intervention explanation.
func (s *RefreshSession) TerminalReason() string {
	if s == nil {
		return ""
	}

	return s.interventionReason
}

// RefreshSwap performs a complete two-vHTLC refresh in one blocking call.
func (c *SwapClient) RefreshSwap(ctx context.Context, req RefreshSwapRequest) (
	*RefreshResult, error) {

	session, err := c.StartRefreshSwap(ctx, req)
	if err != nil {
		return nil, err
	}

	return session.Wait(ctx)
}

// StartRefreshSwap durably records an explicit refresh intent and advances it
// until the server terms have been validated and persisted.
func (c *SwapClient) StartRefreshSwap(ctx context.Context,
	req RefreshSwapRequest) (*RefreshSession, error) {

	if c == nil || c.daemon == nil || c.server == nil {
		return nil, fmt.Errorf("refresh swap connections are not " +
			"configured")
	}
	if c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("refresh swap store is not configured")
	}
	if c.outEvents == nil {
		return nil, fmt.Errorf("refresh event receiver is not " +
			"configured")
	}
	if _, ok := c.outEvents.(IncomingVHTLCEventReceiver); !ok {
		return nil, fmt.Errorf("refresh event receiver does not " +
			"support in-ark vHTLC events")
	}
	if _, ok := c.server.(refreshSwapServerConn); !ok {
		return nil, fmt.Errorf("swap server connection does not " +
			"support refresh swaps")
	}
	if err := validateRefreshRequest(req); err != nil {
		return nil, err
	}
	unlock := c.lockRefreshSession(req.PaymentHash)
	defer unlock()

	existing, err := c.loadRefreshSwap(ctx, req.PaymentHash)
	if err == nil {
		if err := existing.validateRequest(req); err != nil {
			return nil, err
		}
		if existing.state == RefreshStateCreated {
			if err := existing.runUntil(
				ctx, RefreshStateSwapCreated,
			); err != nil {
				return nil, err
			}
		}

		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := c.ensurePaymentHashOwnerAvailable(
		ctx, req.PaymentHash, SwapDirectionRefresh,
	); err != nil {
		return nil, err
	}

	if err := c.validateRefreshSource(ctx, req); err != nil {
		return nil, err
	}
	clientKey, err := c.daemon.IdentityPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get client pubkey: %w", err)
	}
	operatorKey, err := c.daemon.OperatorPubKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("get operator pubkey: %w", err)
	}
	refundInfo, err := c.daemon.AllocateReceiveScript(
		ctx, "refresh input refund",
	)
	if err != nil {
		return nil, fmt.Errorf("allocate refresh refund "+
			"destination: %w", err)
	}
	if err := validateReceiveInfo(
		refundInfo, "refresh refund",
	); err != nil {
		return nil, err
	}
	claimInfo, err := c.daemon.AllocateReceiveScript(
		ctx, "refresh output claim",
	)
	if err != nil {
		return nil, fmt.Errorf("allocate refresh claim destination: %w",
			err)
	}
	if err := validateReceiveInfo(claimInfo, "refresh claim"); err != nil {
		return nil, err
	}

	now := c.currentTime()
	session := &RefreshSession{
		client:           c,
		preimage:         req.Preimage,
		paymentHash:      req.PaymentHash,
		amountSat:        req.AmountSat,
		sourceOutpoint:   req.SourceOutpoint,
		maxVTXOAgeBlocks: req.MaxVTXOAgeBlocks,
		state:            RefreshStateCreated,
		createdAt:        now,
		clientPubKey:     clientKey,
		operatorPubKey:   operatorKey,
		refundReceivePubKey: append(
			[]byte(nil), refundInfo.PubKeyXOnly...,
		),
		refundReceiveScript: append(
			[]byte(nil), refundInfo.PkScript...,
		),
		claimReceivePubKey: append(
			[]byte(nil), claimInfo.PubKeyXOnly...,
		),
		claimReceiveScript: append(
			[]byte(nil), claimInfo.PkScript...,
		),
	}
	if err := session.persist(ctx); err != nil {
		return nil, err
	}
	if err := session.runUntil(ctx, RefreshStateSwapCreated); err != nil {
		return nil, err
	}

	return session, nil
}

// Wait runs the refresh until the server's first-leg claim is observed or a
// safe terminal recovery outcome is reached.
func (s *RefreshSession) Wait(ctx context.Context) (*RefreshResult, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("refresh session must be provided")
	}
	unlock := s.client.lockRefreshSession(s.paymentHash)
	defer unlock()
	if err := s.ensureCurrentSnapshot(ctx); err != nil {
		return nil, err
	}

	if err := s.runUntil(ctx, RefreshStateCompleted); err != nil {
		return nil, err
	}

	return &RefreshResult{
		PaymentHash:         s.paymentHash,
		SourceOutpoint:      s.sourceOutpoint,
		InputVHTLCOutpoint:  s.inputOutpoint,
		OutputVHTLCOutpoint: s.outputOutpoint,
		AmountSat:           int64(s.amountSat),
		FundingSessionID:    s.fundingSessionID,
		ClaimSessionID:      s.claimSessionID,
		ServerClaimTxID:     s.inputClaimTxID,
	}, nil
}

// validateRefreshRequest rejects malformed or self-inconsistent caller terms
// before local or remote durable state is created.
func validateRefreshRequest(req RefreshSwapRequest) error {
	if err := validateSatoshiAmount(
		req.AmountSat, "refresh amount",
	); err != nil {
		return err
	}
	if req.SourceOutpoint == "" {
		return fmt.Errorf("refresh source outpoint must be provided")
	}
	if req.MaxVTXOAgeBlocks == 0 {
		return fmt.Errorf("refresh maximum VTXO age must be positive")
	}
	if req.Preimage.Hash() != req.PaymentHash {
		return fmt.Errorf("refresh preimage does not match payment " +
			"hash")
	}

	return nil
}

// validateReceiveInfo checks the wallet destination allocated before either
// refresh leg can expose funds.
func validateReceiveInfo(info *ReceiveInfo, label string) error {
	if info == nil || len(info.PubKeyXOnly) == 0 ||
		len(info.PkScript) == 0 {
		return fmt.Errorf("%s receive destination is incomplete", label)
	}

	return nil
}

// validateRefreshSource confirms the exact requested source is currently
// managed and can cover the refresh output. Final double-spend admission stays
// authoritative in the daemon call that carries ExactInputOutpoints.
func (c *SwapClient) validateRefreshSource(ctx context.Context,
	req RefreshSwapRequest) error {

	vtxos, err := c.daemon.ListLiveVTXOs(ctx)
	if err != nil {
		return fmt.Errorf("list live VTXOs for refresh source: %w", err)
	}
	for i := range vtxos {
		if vtxos[i].Outpoint != req.SourceOutpoint {
			continue
		}
		if vtxos[i].AmountSat < int64(req.AmountSat) {
			return fmt.Errorf("refresh source amount %d is below "+
				"requested amount %d", vtxos[i].AmountSat,
				req.AmountSat)
		}

		return nil
	}

	return fmt.Errorf("refresh source outpoint %s is not live",
		req.SourceOutpoint)
}

// validateRequest prevents a repeated StartRefreshSwap call from changing any
// caller-owned term already bound to the payment hash.
func (s *RefreshSession) validateRequest(req RefreshSwapRequest) error {
	if s == nil {
		return fmt.Errorf("refresh session must be provided")
	}
	if s.paymentHash != req.PaymentHash || s.preimage != req.Preimage ||
		s.amountSat != req.AmountSat ||
		s.sourceOutpoint != req.SourceOutpoint ||
		s.maxVTXOAgeBlocks != req.MaxVTXOAgeBlocks {
		return fmt.Errorf("refresh request conflicts with durable "+
			"terms for payment hash %s", req.PaymentHash)
	}

	return nil
}

// runUntil advances one reconciliation action at a time until the requested
// state or a terminal state is reached.
func (s *RefreshSession) runUntil(ctx context.Context,
	target RefreshState) error {

	machine := newRefreshLoopFSM(s, target)
	for s.state != target {
		if s.state.IsTerminal() {
			if s.state == RefreshStateCompleted {
				return nil
			}

			return s.terminalErr()
		}
		if err := machine.advance(ctx); err != nil {
			return err
		}
	}

	return nil
}

// terminalErr maps a durable refresh terminal state onto the blocking API.
func (s *RefreshSession) terminalErr() error {
	switch s.state {
	case RefreshStateExpired:
		return ErrSwapExpired

	case RefreshStateRefunded:
		return ErrSwapRefunded

	case RefreshStateNeedsIntervention:
		return newInterventionError(s.interventionReason, nil)

	case RefreshStateFailed:
		return newFailureError(s.interventionReason, nil)

	default:
		return fmt.Errorf("refresh session stopped in terminal "+
			"state %s", s.state)
	}
}

// transition applies one declared refresh edge.
func (s *RefreshSession) transition(event refreshEvent) error {
	next, ok := refreshTransitions[s.state][event]
	if !ok {
		return fmt.Errorf("invalid refresh transition %s -> %s",
			s.state, event)
	}
	s.state = next

	return nil
}

// createSwap obtains and validates the first-leg terms, then derives the exact
// client-funded vHTLC policy from the already-durable local intent.
func (s *RefreshSession) createSwap(ctx context.Context) error {
	server, ok := s.client.server.(refreshSwapServerConn)
	if !ok {
		return newFailureError(
			"swap server connection does not support refresh swaps",
			nil,
		)
	}
	cfg, err := server.CreateRefreshSwap(
		ctx, s.paymentHash, s.amountSat, s.clientPubKey,
		s.maxVTXOAgeBlocks,
	)
	if err != nil {
		wrappedErr := fmt.Errorf("create refresh swap: %w", err)
		if isTerminalRefreshCreateError(err) {
			return newFailureError(
				"swap server rejected refresh request",
				wrappedErr,
			)
		}

		return newRetryableActionError(
			wrappedErr,
		)
	}
	if err := s.validateServerConfig(cfg); err != nil {
		return newFailureError("invalid refresh swap terms", err)
	}

	policy, pkScript, policyTemplate, err := buildVHTLCPolicy(
		s.clientPubKey, cfg.ServerPubkey, s.operatorPubKey,
		s.paymentHash, cfg.VHTLCConfig,
	)
	if err != nil {
		return newFailureError("build refresh input vHTLC", err)
	}

	s.client.log.InfoS(ctx, "Refresh swap created",
		btclog.Hex("hash", s.paymentHash[:]),
		slog.Int64("amount_sat", int64(s.amountSat)),
		slog.String("source_outpoint", s.sourceOutpoint),
		slog.Uint64("max_vtxo_age_blocks", uint64(s.maxVTXOAgeBlocks)),
		slog.Time("deadline", cfg.Expiry),
	)

	return s.mutateAndPersist(ctx, func() error {
		s.cfg = cfg
		s.serverPubKey = cfg.ServerPubkey
		s.inputPolicy = policy
		s.inputPkScript = pkScript
		s.inputPolicyTemplate = policyTemplate

		return s.transition(refreshEventSwapCreated)
	})
}

// isTerminalRefreshCreateError identifies authoritative request and identity
// rejections that cannot change when the durable request is replayed. Server
// availability, capacity, and internal failures remain retryable.
func isTerminalRefreshCreateError(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.AlreadyExists,
		codes.PermissionDenied, codes.Unauthenticated:
		return true

	default:
		return false
	}
}

// validateServerConfig checks that a replayed server response still echoes
// every client-owned term and names the refresh rail.
func (s *RefreshSession) validateServerConfig(cfg *RefreshSwapConfig) error {
	if cfg == nil {
		return fmt.Errorf("refresh config is required")
	}
	if cfg.PaymentHash != s.paymentHash {
		return fmt.Errorf("payment hash mismatch")
	}
	if cfg.AmountSat != int64(s.amountSat) {
		return fmt.Errorf("amount %d does not match requested "+
			"amount %d", cfg.AmountSat, s.amountSat)
	}
	if cfg.ServerPubkey == nil {
		return fmt.Errorf("server pubkey is required")
	}
	if cfg.SettlementType != SettlementTypeRefresh {
		return fmt.Errorf("settlement type %q is not refresh",
			cfg.SettlementType)
	}
	if cfg.Expiry.IsZero() {
		return fmt.Errorf("funding expiry is required")
	}
	if err := vhtlcTiming(cfg.VHTLCConfig).ValidateOrder(); err != nil {
		return fmt.Errorf("invalid input vHTLC timing: %w", err)
	}

	cfgServer, err := btcec.ParsePubKey(
		cfg.VHTLCConfig.SwapServerPubkey,
	)
	if err != nil {
		return fmt.Errorf("parse input vHTLC server pubkey: %w", err)
	}
	if !cfgServer.IsEqual(cfg.ServerPubkey) {
		return fmt.Errorf("input vHTLC server pubkey mismatch")
	}

	return nil
}

// buildVHTLCPolicy derives the semantic policy, pkScript, and daemon template
// shared by the input and output legs.
func buildVHTLCPolicy(sender, receiver, operator *btcec.PublicKey,
	paymentHash lntypes.Hash, cfg VHTLCConfig) (*arkscript.VHTLCPolicy,
	[]byte, []byte, error) {

	if sender == nil || receiver == nil || operator == nil {
		return nil, nil, nil, fmt.Errorf("vHTLC participant keys are " +
			"required")
	}
	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:       sender,
		Receiver:     receiver,
		Server:       operator,
		PreimageHash: paymentHash,
		RefundLocktime: cfg.
			RefundLocktime,
		UnilateralClaimDelay: cfg.
			UnilateralClaimDelay,
		UnilateralRefundDelay: cfg.
			UnilateralRefundDelay,
		UnilateralRefundWithoutReceiverDelay: cfg.
			UnilateralRefundWithoutReceiverDelay,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	pkScript, err := policy.PkScript()
	if err != nil {
		return nil, nil, nil, err
	}
	policyTemplate, err := encodeVHTLCPolicyTemplate(policy)
	if err != nil {
		return nil, nil, nil, err
	}

	return policy, pkScript, policyTemplate, nil
}

// inputFundingKey is stable across process restarts and daemon RPC retries.
func (s *RefreshSession) inputFundingKey() string {
	return refreshInputFundingKeyPrefix + s.paymentHash.String()
}

// fundingDeadline returns the daemon admission deadline with the same safety
// buffer used by ordinary pay sessions.
func (s *RefreshSession) fundingDeadline() time.Time {
	if s.cfg == nil || s.cfg.Expiry.IsZero() {
		return time.Time{}
	}

	return s.cfg.Expiry.Add(-s.client.fundingExpiryBuffer)
}

// fundingAdmissionClosed reports whether only read-only keyed reconciliation
// remains safe.
func (s *RefreshSession) fundingAdmissionClosed() bool {
	deadline := s.fundingDeadline()

	return !deadline.IsZero() && !s.client.currentTime().Before(deadline)
}

// initiateFunding persists the external-effect intent before asking the
// daemon to consume the exact source VTXO.
func (s *RefreshSession) initiateFunding(ctx context.Context) error {
	if s.fundingAdmissionClosed() {
		return s.markExpired(ctx, "refresh funding deadline elapsed")
	}
	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	if height+s.client.refundLocktimeBuffer >=
		s.cfg.VHTLCConfig.RefundLocktime {
		return newFailureError(
			"refresh input refund locktime is too close", nil,
		)
	}

	return s.mutateAndPersist(ctx, func() error {
		return s.transition(refreshEventFundingInitiated)
	})
}

// fundInputVHTLC submits or reconciles the first-leg OOR using only the exact
// caller-selected managed input.
func (s *RefreshSession) fundInputVHTLC(ctx context.Context) error {
	if s.fundingSessionID != "" && s.inputOutpoint != "" {
		return s.markInputFunded(ctx)
	}

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	blockAdmissionClosed := height+s.client.refundLocktimeBuffer >=
		s.cfg.VHTLCConfig.RefundLocktime
	existingOnly := s.fundingAdmissionClosed() || blockAdmissionClosed
	result, err := s.client.daemon.SendOORWithPolicyOptionsDetails(
		ctx, int64(s.amountSat), s.inputPolicyTemplate, OORSendOptions{
			IdempotencyKey:      s.inputFundingKey(),
			AdmissionDeadline:   s.fundingDeadline(),
			ExistingOnly:        existingOnly,
			ExactInputOutpoints: []string{s.sourceOutpoint},
		},
	)
	if err != nil {
		if existingOnly && status.Code(err) == codes.NotFound {
			reason := "refresh funding safety window closed " +
				"without an accepted transfer"
			if !blockAdmissionClosed {
				reason = "refresh funding admission closed " +
					"without an accepted transfer"
			}

			return s.markExpired(
				ctx, reason,
			)
		}

		return newRetryableActionError(
			fmt.Errorf("fund refresh input vHTLC: %w", err),
		)
	}
	if result == nil || result.SessionID == "" ||
		result.RecipientOutpoint == "" {
		return newRetryableActionError(
			fmt.Errorf("fund refresh input vHTLC returned " +
				"incomplete OOR metadata"),
		)
	}

	// Record accepted daemon metadata without rollback. The keyed daemon
	// intent makes the next attempt read-only if this store write fails.
	s.fundingSessionID = result.SessionID
	s.inputOutpoint = result.RecipientOutpoint
	s.inputAmount = int64(s.amountSat)
	if err := s.persist(ctx); err != nil {
		return newRetryableActionError(
			fmt.Errorf("persist refresh funding result: %w", err),
		)
	}

	return s.markInputFunded(ctx)
}

// markInputFunded arms sender-side recovery before exposing the session to the
// server-funded second leg.
func (s *RefreshSession) markInputFunded(ctx context.Context) error {
	if err := s.ensureInputRefundRecoveryArmed(ctx); err != nil {
		return newRetryableActionError(err)
	}
	if s.state == RefreshStateInputVHTLCFunded {
		return nil
	}

	s.client.log.InfoS(ctx, "Refresh input vHTLC funded",
		btclog.Hex("hash", s.paymentHash[:]),
		slog.String("source_outpoint", s.sourceOutpoint),
		slog.String("vhtlc_outpoint", s.inputOutpoint),
		slog.String("funding_session_id", s.fundingSessionID),
	)

	return s.mutateAndPersist(ctx, func() error {
		return s.transition(refreshEventInputVHTLCFunded)
	})
}

// markExpired persists terminal expiry only while no first-leg transfer was
// accepted, so a funding ambiguity never strands value without recovery.
func (s *RefreshSession) markExpired(ctx context.Context, reason string) error {
	if s.fundingSessionID != "" || s.inputOutpoint != "" {
		return s.initiateRefund(ctx, reason)
	}
	if s.state != RefreshStateExpired {
		if err := s.mutateAndPersist(ctx, func() error {
			s.interventionReason = reason

			return s.transition(refreshEventExpired)
		}); err != nil {
			return err
		}
	}

	return ErrSwapExpired
}

// waitForOutputEvent polls the durable incoming mailbox while also watching
// the input refund boundary. This prevents a blocked mailbox receive from
// suppressing first-leg recovery indefinitely.
func (s *RefreshSession) waitForOutputEvent(ctx context.Context) error {
	receiver, ok := s.client.outEvents.(IncomingVHTLCEventReceiver)
	if !ok {
		return s.initiateRefund(
			ctx,
			"refresh event receiver does not support in-ark events",
		)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.checkInputFundingRejected(ctx); err != nil {
			return err
		}
		if s.state.IsTerminal() {
			return nil
		}
		if err := s.ensureWaitingForOutputSafe(ctx); err != nil {
			return err
		}

		wait := s.client.waitPollInterval
		if wait <= 0 {
			wait = time.Second
		}
		waitCtx, cancel := context.WithTimeout(ctx, wait)
		notification, err := receiver.WaitIncomingVHTLC(
			waitCtx, s.paymentHash, s.clientPubKey,
		)
		cancel()
		if err != nil {
			if isDeadlineExceededErr(err) && ctx.Err() == nil {
				continue
			}

			return newRetryableActionError(
				fmt.Errorf("wait refresh output event: %w",
					err),
			)
		}
		if notification == nil || notification.InArk == nil {
			return s.initiateRefund(
				ctx,
				"refresh mailbox delivered a non-in-ark event",
			)
		}
		if notification.AckCursor == 0 {
			return s.initiateRefund(
				ctx,
				"refresh output event is missing an ACK cursor",
			)
		}
		if err := s.acceptOutputEvent(
			ctx, notification.InArk, notification.AckCursor,
		); err != nil {
			return err
		}

		// ACK belongs to the next durable state's reconciliation
		// action. Returning now keeps the Loop FSM aligned if ACK fails
		// and lets a restart use the persisted cursor.
		return nil
	}
}

// ensureWaitingForOutputSafe moves a funded input into recovery once either
// leg can no longer leave enough time for an atomic claim sequence.
func (s *RefreshSession) ensureWaitingForOutputSafe(ctx context.Context) error {
	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	if height+s.client.refundLocktimeBuffer <
		s.cfg.VHTLCConfig.RefundLocktime {
		return nil
	}

	return s.initiateRefund(
		ctx,
		"refresh output was unavailable before input refund locktime",
	)
}

// checkInputFundingRejected treats a failed daemon session as ambiguous. The
// operator may already have finalized the transfer before local bookkeeping
// failed, so the armed recovery remains authoritative until index evidence
// proves which side spent the first leg.
func (s *RefreshSession) checkInputFundingRejected(ctx context.Context) error {
	if s.fundingSessionID == "" {
		return nil
	}
	session, err := s.client.daemon.GetOORSession(
		ctx, s.fundingSessionID,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get refresh input funding session: %w",
				err),
		)
	}
	if session == nil || session.GetStatus() !=
		waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED {
		return nil
	}

	return s.reconcileLiveInput(ctx)
}

// acceptOutputEvent validates the second-leg server event and persists every
// byte needed to claim it before either mailbox or server acknowledgement.
func (s *RefreshSession) acceptOutputEvent(ctx context.Context,
	event *InArkHtlcEvent, ackCursor uint64) error {

	if event.SettlementType != SettlementTypeRefresh {
		return s.initiateRefund(
			ctx, fmt.Sprintf("refresh output event has "+
				"settlement type %q", event.SettlementType),
		)
	}
	if event.PaymentHash != s.paymentHash {
		return s.initiateRefund(
			ctx, "refresh output event payment hash mismatch",
		)
	}
	if event.AmountSat != int64(s.amountSat) ||
		event.VHTLCAmountSat != 0 &&
			event.VHTLCAmountSat != int64(s.amountSat) {
		return s.initiateRefund(
			ctx, fmt.Sprintf("refresh output event amount %d/%d "+
				"does not match %d", event.AmountSat,
				event.VHTLCAmountSat, s.amountSat),
		)
	}
	if event.RequestedAmountSat != uint64(s.amountSat) ||
		event.AttachedCreditSat != 0 {
		return s.initiateRefund(
			ctx, fmt.Sprintf("refresh output settlement amounts "+
				"%d/%d do not match %d/0",
				event.RequestedAmountSat,
				event.AttachedCreditSat, s.amountSat),
		)
	}
	if event.SenderPubkey == nil || s.serverPubKey == nil ||
		!event.SenderPubkey.IsEqual(s.serverPubKey) {
		return s.initiateRefund(
			ctx, "refresh output sender does not match "+
				"negotiated server",
		)
	}
	if err := vhtlcTiming(event.VHTLCConfig).ValidateOrder(); err != nil {
		return s.initiateRefund(
			ctx, fmt.Sprintf("refresh output timing is "+
				"invalid: %v", err),
		)
	}
	eventServer, err := btcec.ParsePubKey(
		event.VHTLCConfig.SwapServerPubkey,
	)
	if err != nil || !eventServer.IsEqual(event.SenderPubkey) {
		return s.initiateRefund(
			ctx, "refresh output vHTLC server pubkey mismatch",
		)
	}
	if err := s.validateAtomicTiming(ctx, event.VHTLCConfig); err != nil {
		var retryable *retryableActionError
		if errors.As(err, &retryable) {
			return err
		}

		return s.initiateRefund(ctx, err.Error())
	}

	policy, pkScript, policyTemplate, err := buildVHTLCPolicy(
		event.SenderPubkey, s.clientPubKey, s.operatorPubKey,
		s.paymentHash, event.VHTLCConfig,
	)
	if err != nil {
		return s.initiateRefund(
			ctx, fmt.Sprintf("build refresh output vHTLC: %v", err),
		)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.outputSenderPubKey = event.SenderPubkey
		s.outputConfig = event.VHTLCConfig
		s.outputPolicy = policy
		s.outputPkScript = pkScript
		s.outputPolicyTemplate = policyTemplate
		if event.VHTLCOutpoint != "" {
			s.outputOutpoint = event.VHTLCOutpoint
		}
		if event.VHTLCAmountSat != 0 {
			s.outputAmount = event.VHTLCAmountSat
		}
		s.pendingAckCursor = ackCursor

		return s.transition(refreshEventOutput)
	})
}

// validateAtomicTiming requires the output refund to mature far enough before
// the input refund that the client claim can reveal the preimage and the server
// can still claim its leg with a recovery margin.
func (s *RefreshSession) validateAtomicTiming(ctx context.Context,
	output VHTLCConfig) error {

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	policy := s.client.recoveryPolicy.WithDefaults()
	if err := vhtlcTiming(output).ValidateClaimWindow(
		arkscript.VHTLCClaimWindow{
			CurrentHeight:     height,
			ExitAncestryDelay: policy.ExitAncestryDelayBlocks,
			RecoveryMargin:    policy.MinRecoveryMarginBlocks,
		},
	); err != nil {
		return fmt.Errorf("refresh output claim window is invalid: %w",
			err)
	}

	inputRefund := uint64(s.cfg.VHTLCConfig.RefundLocktime)
	outputRefund := uint64(output.RefundLocktime)
	margin := uint64(policy.MinRecoveryMarginBlocks)
	if inputRefund <= outputRefund+margin {
		return fmt.Errorf("refresh input refund locktime %d must "+
			"exceed output refund locktime %d by more than "+
			"%d blocks", inputRefund, outputRefund, margin)
	}

	return nil
}

// ackOutputEvent completes the mailbox ACK and signed server acceptance gate,
// then clears the durable pending cursor.
func (s *RefreshSession) ackOutputEvent(ctx context.Context,
	ack func(context.Context) error) error {

	if s.pendingAckCursor == 0 {
		return nil
	}
	if ack == nil {
		ack = func(ctx context.Context) error {
			return s.client.outEvents.AckOutSwapHtlc(
				ctx, s.paymentHash, s.clientPubKey,
				s.pendingAckCursor,
			)
		}
	}
	if err := ack(ctx); err != nil {
		return newRetryableActionError(
			fmt.Errorf("ack refresh mailbox event: %w", err),
		)
	}
	ackSignature, err := s.client.daemon.SignOutSwapHTLCAck(
		ctx, s.paymentHash, uint64(s.amountSat), s.outputPkScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("sign refresh output acknowledgement: %w",
				err),
		)
	}
	if err := s.client.server.AcknowledgeOutSwapHTLC(
		ctx, s.paymentHash, s.clientPubKey, ackSignature,
	); err != nil {

		if isTerminalOutSwapHTLCAckError(err) {
			return s.initiateRefund(
				ctx, fmt.Sprintf("server rejected "+
					"refresh ACK: %v", err),
			)
		}

		return newRetryableActionError(
			fmt.Errorf("acknowledge refresh output: %w", err),
		)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.pendingAckCursor = 0

		return nil
	})
}

// waitForOutputFunding reconciles the ACK after restart, waits for the exact
// vHTLC, then enforces the caller's backing-batch age cap before claiming.
func (s *RefreshSession) waitForOutputFunding(ctx context.Context) error {
	if err := s.ackOutputEvent(ctx, nil); err != nil {
		return err
	}

	announcedOutpoint := s.outputOutpoint
	funding, err := s.client.waitForVHTLC(
		ctx, s.outputPkScript, time.Time{},
		func(ctx context.Context) error {
			return s.ensureOutputFundingStillSafe(ctx)
		},
	)
	if err != nil {
		if s.state == RefreshStateRefundInitiated {
			return nil
		}

		return newRetryableActionError(
			fmt.Errorf("wait for refresh output vHTLC: %w", err),
		)
	}
	if funding == nil || funding.Outpoint == "" {
		return newRetryableActionError(
			fmt.Errorf("refresh output funding metadata is " +
				"incomplete"),
		)
	}
	if announcedOutpoint != "" && funding.Outpoint != announcedOutpoint {
		return s.initiateRefund(
			ctx, fmt.Sprintf("refresh output outpoint %s does "+
				"not match announced outpoint %s",
				funding.Outpoint, announcedOutpoint),
		)
	}
	if funding.AmountSat != int64(s.amountSat) {
		return s.initiateRefund(
			ctx, fmt.Sprintf("refresh output amount %d does not "+
				"match %d", funding.AmountSat, s.amountSat),
		)
	}

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	if err := validateRefreshOutputAge(
		funding, height, s.maxVTXOAgeBlocks,
	); err != nil {
		return s.initiateRefund(ctx, err.Error())
	}
	recoveryID, err := s.armRefreshRecovery(
		ctx, refreshRecoveryLabel(
			refreshOutputRecoveryLabel, funding.Outpoint,
		),
		recoveryDirectionReceive,
		recoveryActionClaim,
		funding.Outpoint,
		funding.AmountSat,
		s.outputSenderPubKey,
		s.clientPubKey,
		s.outputConfig,
		s.claimReceiveScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("arm refresh output claim recovery: %w",
				err),
		)
	}

	err = s.mutateAndPersist(ctx, func() error {
		s.outputOutpoint = funding.Outpoint
		s.outputAmount = funding.AmountSat
		s.outputObservedHeight = height
		s.outputCreatedHeight = funding.CreatedHeight
		s.outputBatchExpiry = funding.BatchExpiry
		s.claimRecoveryID = recoveryID

		return s.transition(refreshEventOutputVHTLCFunded)
	})
	if err != nil {
		return err
	}

	return nil
}

// ensureOutputFundingStillSafe stops waiting once either output claiming or
// input recovery would enter its refund race window.
func (s *RefreshSession) ensureOutputFundingStillSafe(
	ctx context.Context) error {

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	buffer := s.client.refundLocktimeBuffer
	if height+buffer < s.outputConfig.RefundLocktime &&
		height+buffer < s.cfg.VHTLCConfig.RefundLocktime {
		return nil
	}
	if err := s.initiateRefund(
		ctx,
		"refresh output was not funded inside the safe claim window",
	); err != nil {
		return err
	}

	return ErrSwapExpired
}

// validateRefreshOutputAge checks the backing-batch evidence attached to the
// actual server-funded vHTLC. Equality with the requested cap is accepted.
func validateRefreshOutputAge(vtxo *VTXOInfo, currentHeight uint32,
	maxAge uint32) error {

	if maxAge == 0 {
		return fmt.Errorf("refresh maximum VTXO age must be positive")
	}
	if vtxo == nil || vtxo.CreatedHeight <= 0 {
		return fmt.Errorf("refresh output created height is " +
			"unavailable")
	}
	if vtxo.BatchExpiry <= 0 || vtxo.BatchExpiry < vtxo.CreatedHeight {
		return fmt.Errorf("refresh output batch expiry is invalid")
	}
	createdHeight := uint32(vtxo.CreatedHeight)
	batchExpiry := uint32(vtxo.BatchExpiry)
	if createdHeight > currentHeight {
		return fmt.Errorf("refresh output created height %d is in "+
			"the future", createdHeight)
	}
	if batchExpiry <= currentHeight {
		return fmt.Errorf("refresh output backing batch is expired")
	}
	age := currentHeight - createdHeight
	if age > maxAge {
		return fmt.Errorf("refresh output backing-batch age %d "+
			"exceeds maximum %d", age, maxAge)
	}

	return nil
}

// initiateOutputClaim persists the preimage-revealing side-effect intent.
func (s *RefreshSession) initiateOutputClaim(ctx context.Context) error {
	if err := s.ensureOutputClaimRecoveryArmed(ctx); err != nil {
		return newRetryableActionError(err)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.claimIntentInProcess = true

		return s.transition(refreshEventClaimInitiated)
	})
}

// claimOutputVHTLC reconciles a possible prior claim before submitting the
// custom-input spend that reveals the shared preimage.
func (s *RefreshSession) claimOutputVHTLC(ctx context.Context) error {
	if s.claimSessionID != "" {
		return s.markOutputClaimed(ctx)
	}

	// A cooperative rollover spends the old outpoint without the preimage
	// while preserving this policy script. Follow the authoritative live
	// replacement before interpreting any same-script spent row.
	if err := s.reconcileLiveOutput(ctx); err != nil {
		return err
	}
	if s.state == RefreshStateOutputClaimed {
		return nil
	}
	claimed, err := s.client.refreshOutputClaimAlreadyIndexedBounded(
		ctx, s.paymentHash, s.outputPkScript, s.outputOutpoint,
	)
	if err != nil {
		if errors.Is(err, errReceiveVHTLCSpentWithoutPreimage) {
			return s.initiateRefund(
				ctx,
				"refresh output was spent without the preimage",
			)
		}

		return newRetryableActionError(err)
	}
	if claimed {
		return s.markOutputClaimed(ctx)
	}

	if !s.claimIntentInProcess && !s.updatedAt.IsZero() {
		graceUntil := s.updatedAt.Add(s.client.claimResumeGracePeriod)
		if s.client.currentTime().Before(graceUntil) {
			return waitForFixedPoll(ctx, s.client.waitPollInterval)
		}
	}
	recoveryHandled, err := s.reconcileOutputClaimRecovery(ctx)
	if err != nil {
		return err
	}
	if recoveryHandled {
		return nil
	}
	if err := s.ensureOutputClaimStillSafe(ctx); err != nil {
		return err
	}

	claimSessionID, err := s.claimRefreshOutputVHTLC(ctx)
	if errors.Is(err, errReceiveClaimAlreadyIndexed) {
		return s.markOutputClaimed(ctx)
	}
	if errors.Is(err, errReceiveVHTLCSpentWithoutPreimage) {
		return s.initiateRefund(
			ctx, "refresh output was spent without the preimage",
		)
	}
	if err != nil {
		if escalateErr := s.maybeEscalateRefreshRecovery(
			ctx, s.claimRecoveryID, s.outputConfig.RefundLocktime,
			&s.claimRecoveryFailureAt, "output claim", err,
		); escalateErr != nil {
			return newRetryableActionError(escalateErr)
		}

		return newRetryableActionError(err)
	}
	if claimSessionID == "" {
		return newRetryableActionError(
			fmt.Errorf("refresh output claim returned empty " +
				"session id"),
		)
	}

	// The daemon side effect is already accepted, so retain the session ID
	// in memory if the following store write fails and reconcile by spend.
	s.claimSessionID = claimSessionID
	if err := s.persist(ctx); err != nil {
		return newRetryableActionError(
			fmt.Errorf("persist refresh output claim: %w", err),
		)
	}
	s.claimIntentInProcess = false

	return s.markOutputClaimed(ctx)
}

// claimRefreshOutputVHTLC spends only the currently persisted output
// outpoint. Its reconciliation path is role-scoped because both refresh legs
// and any cooperative rollover share the same script hash.
func (s *RefreshSession) claimRefreshOutputVHTLC(ctx context.Context) (string,
	error) {

	claimPath, err := s.outputPolicy.ClaimPath(s.preimage)
	if err != nil {
		return "", fmt.Errorf("build refresh output claim path: %w",
			err)
	}
	spendPath, err := claimPath.Encode()
	if err != nil {
		return "", fmt.Errorf("encode refresh output claim path: %w",
			err)
	}
	if len(s.claimReceivePubKey) == 0 {
		return "", errors.New("refresh claim receive pubkey is " +
			"required")
	}

	var lastSendErr error
	for attempt := 1; attempt <= s.client.claimMaxAttempts; attempt++ {
		attemptedOutpoint := s.outputOutpoint
		claimSessionID, err := s.client.daemon.SendOORWithCustomInputs(
			ctx, s.claimReceivePubKey, s.outputAmount,
			[]CustomInput{{
				Outpoint:           attemptedOutpoint,
				VTXOPolicyTemplate: s.outputPolicyTemplate,
				SpendPath:          spendPath,
				AmountSat:          s.outputAmount,
				PkScript:           s.outputPkScript,
			}},
		)
		if err == nil {
			return claimSessionID, nil
		}
		lastSendErr = err

		// If this exact outpoint rolled over during submission, bind
		// the replacement recovery first and retry it. The old
		// same-script spend says nothing about the replacement.
		if err := s.reconcileLiveOutput(ctx); err != nil {
			return "", err
		}
		if s.outputOutpoint != attemptedOutpoint {
			continue
		}

		claimed, reconcileErr :=
			s.client.refreshOutputClaimAlreadyIndexedBounded(
				ctx, s.paymentHash, s.outputPkScript,
				s.outputOutpoint,
			)
		if reconcileErr != nil {
			return "", reconcileErr
		}
		if claimed {
			return "", errReceiveClaimAlreadyIndexed
		}
		if attempt < s.client.claimMaxAttempts {
			if err := waitForFixedPoll(
				ctx, s.client.claimRetryDelay,
			); err != nil {
				return "", err
			}
		}
	}

	if lastSendErr == nil {
		return "", errors.New("claim refresh output: no send attempt " +
			"made")
	}

	return "", fmt.Errorf("claim refresh output: %w", lastSendErr)
}

// refreshOutputClaimAlreadyIndexedBounded checks only the persisted output
// outpoint, so an earlier same-script rollover spend cannot be mistaken for
// the claim or refund of the current output.
func (c *SwapClient) refreshOutputClaimAlreadyIndexedBounded(
	ctx context.Context, paymentHash lntypes.Hash, pkScript []byte,
	outpoint string) (bool, error) {

	reconcileCtx, cancel := context.WithTimeout(ctx, c.waitPollInterval)
	defer cancel()

	spentVTXOs, err := c.daemon.ListSpentVTXOs(reconcileCtx)
	if err != nil {
		if reconcileCtx.Err() != nil && ctx.Err() == nil ||
			isDeadlineExceededErr(err) {
			return false, nil
		}

		return false, err
	}
	spent, err := exactRefreshVHTLC(
		spentVTXOs, outpoint, pkScript, "output",
	)
	if err != nil || spent == nil {
		return false, err
	}
	preimage, err := findMatchingPreimageInVTXO(spent, paymentHash)
	if err != nil {
		return false, err
	}
	if preimageMatchesHash(preimage, paymentHash) {
		return true, nil
	}
	if spent.SpentByTxID == "" {
		return false, nil
	}

	pkg, err := c.daemon.GetIndexedOORSession(
		reconcileCtx, pkScript, spent.SpentByTxID,
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

// ensureOutputClaimStillSafe avoids revealing the preimage once the server's
// output refund can race a new claim.
func (s *RefreshSession) ensureOutputClaimStillSafe(ctx context.Context) error {
	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	if height+s.client.refundLocktimeBuffer <
		s.outputConfig.RefundLocktime {
		return nil
	}

	return s.initiateRefund(
		ctx, "refresh output refund locktime reached before claim",
	)
}

// reconcileLiveOutput follows a cooperative refresh of the output vHTLC while
// preserving the exact amount and age constraints before spending it.
func (s *RefreshSession) reconcileLiveOutput(ctx context.Context) error {
	vtxo, err := s.client.daemon.FindLiveVTXOByPkScript(
		ctx, s.outputPkScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("reconcile refresh output vHTLC: %w", err),
		)
	}
	if vtxo == nil {
		return nil
	}
	if vtxo.AmountSat != int64(s.amountSat) {
		return s.initiateRefund(
			ctx, fmt.Sprintf("refreshed output amount %d does "+
				"not match %d", vtxo.AmountSat, s.amountSat),
		)
	}
	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	if err := validateRefreshOutputAge(
		vtxo, height, s.maxVTXOAgeBlocks,
	); err != nil {
		return s.initiateRefund(ctx, err.Error())
	}
	if vtxo.Outpoint == s.outputOutpoint {
		return nil
	}
	recoveryID, err := s.armRefreshRecovery(
		ctx, refreshRecoveryLabel(
			refreshOutputRecoveryLabel, vtxo.Outpoint,
		),
		recoveryDirectionReceive,
		recoveryActionClaim,
		vtxo.Outpoint,
		vtxo.AmountSat,
		s.outputSenderPubKey,
		s.clientPubKey,
		s.outputConfig,
		s.claimReceiveScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("rebind refresh output recovery: %w", err),
		)
	}
	cancelState, lastErr, err := cancelRefreshVHTLCRecovery(
		ctx, s.client.daemon, s.claimRecoveryID, "refresh output "+
			"vHTLC rolled over", "",
	)
	if err != nil {
		return newRetryableActionError(err)
	}
	switch {
	case cancelState == recoveryStateCompleted:
		return s.markOutputClaimedWithRecovery(ctx, false)

	case cancelState == recoveryStateFailed ||
		recoveryIsActive(cancelState):
		return newInterventionError(
			refreshRecoveryCancelReason(
				"output claim", cancelState, lastErr,
			),
			nil,
		)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.outputOutpoint = vtxo.Outpoint
		s.outputAmount = vtxo.AmountSat
		s.outputObservedHeight = height
		s.outputCreatedHeight = vtxo.CreatedHeight
		s.outputBatchExpiry = vtxo.BatchExpiry
		s.claimRecoveryID = recoveryID

		return nil
	})
}

// markOutputClaimed closes both dormant recovery branches before recording
// preimage exposure. Once this boundary is durable the input must only be
// completed by the server's matching claim, never by a client refund.
func (s *RefreshSession) markOutputClaimed(ctx context.Context) error {
	return s.markOutputClaimedWithRecovery(ctx, true)
}

// markOutputClaimedWithRecovery records preimage exposure while avoiding an
// invalid cancellation request when the daemon recovery itself won the claim.
func (s *RefreshSession) markOutputClaimedWithRecovery(ctx context.Context,
	cancelClaimRecovery bool) error {

	if cancelClaimRecovery {
		_, _, err := cancelRefreshVHTLCRecovery(
			ctx, s.client.daemon, s.claimRecoveryID,
			recoveryReasonClaimAccepted, s.claimSessionID,
		)
		if err != nil {
			return newRetryableActionError(err)
		}
	}
	refundState, lastErr, err := cancelRefreshVHTLCRecovery(
		ctx, s.client.daemon, s.refundRecoveryID, "refresh output "+
			"claim exposed preimage", s.claimSessionID,
	)
	if err != nil {
		return newRetryableActionError(err)
	}
	if refundState == recoveryStateCompleted ||
		refundState == recoveryStateFailed ||
		recoveryIsActive(refundState) {
		return newInterventionError(
			refreshRecoveryCancelReason(
				"input refund", refundState, lastErr,
			),
			nil,
		)
	}
	if s.state == RefreshStateOutputClaimed {
		return nil
	}

	s.client.log.InfoS(ctx, "Refresh output vHTLC claimed",
		btclog.Hex("hash", s.paymentHash[:]),
		slog.String("output_outpoint", s.outputOutpoint),
		slog.String("claim_session_id", s.claimSessionID),
	)

	return s.mutateAndPersist(ctx, func() error {
		return s.transition(refreshEventOutputClaimed)
	})
}

// reconcileOutputClaimRecovery keeps cooperative claims quiescent after the
// daemon has started unilateral recovery for the same second-leg outpoint.
func (s *RefreshSession) reconcileOutputClaimRecovery(ctx context.Context) (
	bool, error) {

	state, lastErr, err := getVHTLCRecoveryState(
		ctx, s.client.daemon, s.claimRecoveryID,
	)
	if err != nil {
		return false, newRetryableActionError(err)
	}

	switch {
	case state == recoveryStateCompleted:
		s.client.log.InfoS(
			ctx,
			"Refresh output claim recovery completed",
			btclog.Hex("hash", s.paymentHash[:]),
			slog.String("recovery_id", s.claimRecoveryID),
		)

		return true, s.markOutputClaimedWithRecovery(ctx, false)

	case state == recoveryStateFailed:
		reason := "refresh output claim recovery failed"
		if lastErr != "" {
			reason += ": " + lastErr
		}

		return true, newInterventionError(reason, nil)

	case recoveryIsActive(state):
		s.client.log.DebugS(ctx, "Refresh output claim recovery active",
			btclog.Hex("hash", s.paymentHash[:]),
			slog.String("recovery_id", s.claimRecoveryID),
			slog.String("recovery_state", state.String()),
		)

		return true, waitForFixedPoll(
			ctx, s.client.waitPollInterval,
		)

	default:
		return false, nil
	}
}

// waitForInputClaim completes the atomic exchange only after the server's
// first-leg spend exposes the same preimage.
func (s *RefreshSession) waitForInputClaim(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.reconcileLiveInput(ctx); err != nil {
			return err
		}
		if s.state.IsTerminal() {
			return nil
		}
		preimage, spent, err :=
			s.client.waitForRefreshInputClaimObservation(
				ctx, s.paymentHash, s.inputPkScript,
				s.inputOutpoint,
			)
		if err != nil {
			if interventionReason(err) != "" {
				return err
			}

			return newRetryableActionError(
				fmt.Errorf("observe refresh input claim: %w",
					err),
			)
		}
		if preimage != nil {
			return s.mutateAndPersist(ctx, func() error {
				if spent != nil {
					s.inputClaimTxID = spent.SpentByTxID
				}

				return s.transition(refreshEventCompleted)
			})
		}
		if spent != nil {
			reason := "refresh input was spent without the " +
				"shared preimage"
			if spent.SpentByTxID != "" {
				reason += " by " + spent.SpentByTxID
			}

			return newInterventionError(reason, nil)
		}

		if err := waitForFixedPoll(
			ctx, s.client.waitPollInterval,
		); err != nil {
			return err
		}
	}
}

// reconcileLiveInput follows a cooperative rollover of the first-leg policy
// and arms recovery for the replacement before persisting its new outpoint.
func (s *RefreshSession) reconcileLiveInput(ctx context.Context) error {
	if len(s.inputPkScript) == 0 {
		return nil
	}

	vtxo, err := s.client.daemon.FindLiveVTXOByPkScript(
		ctx, s.inputPkScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("reconcile refresh input vHTLC: %w", err),
		)
	}
	if vtxo == nil || vtxo.Outpoint == "" ||
		vtxo.Outpoint == s.inputOutpoint {
		return nil
	}
	if vtxo.AmountSat != int64(s.amountSat) {
		return newInterventionError(
			fmt.Sprintf("refreshed input amount %d does not "+
				"match %d", vtxo.AmountSat, s.amountSat),
			nil,
		)
	}
	if s.state == RefreshStateOutputClaimed {
		return s.mutateAndPersist(ctx, func() error {
			s.inputOutpoint = vtxo.Outpoint
			s.inputAmount = vtxo.AmountSat
			s.refundRecoveryID = ""

			return nil
		})
	}

	recoveryID, err := s.armRefreshRecovery(
		ctx, refreshRecoveryLabel(
			refreshInputRecoveryLabel, vtxo.Outpoint,
		),
		recoveryDirectionPay,
		recoveryActionRefundWithoutReceiver,
		vtxo.Outpoint,
		vtxo.AmountSat,
		s.clientPubKey,
		s.serverPubKey,
		s.cfg.VHTLCConfig,
		s.refundReceiveScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("rebind refresh input recovery: %w", err),
		)
	}
	cancelState, lastErr, err := cancelRefreshVHTLCRecovery(
		ctx, s.client.daemon, s.refundRecoveryID, "refresh input "+
			"vHTLC rolled over", "",
	)
	if err != nil {
		return newRetryableActionError(err)
	}
	switch {
	case cancelState == recoveryStateCompleted:
		return s.markRefundRecoveryWon(ctx)

	case cancelState == recoveryStateFailed ||
		recoveryIsActive(cancelState):
		return newInterventionError(
			refreshRecoveryCancelReason(
				"input refund", cancelState, lastErr,
			),
			nil,
		)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.inputOutpoint = vtxo.Outpoint
		s.inputAmount = vtxo.AmountSat
		s.refundRecoveryID = recoveryID

		return nil
	})
}

// waitForRefreshInputClaimObservation gives the exact first-leg spent row a
// short chance to gain its finalized checkpoint metadata. Unlike the generic
// in-swap observer, it deliberately has no wallet-wide live-VTXO fallback:
// Alice's second-leg claim exposes the same preimage and must not be mistaken
// for the server spending the first leg.
func (c *SwapClient) waitForRefreshInputClaimObservation(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte, outpoint string) (
	*lntypes.Preimage, *VTXOInfo, error) {

	var spentInput *VTXOInfo
	maxAttempts := defaultClaimPreimageLookupAttempts
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		preimage, spent, err := c.findRefreshInputClaimObservation(
			ctx, paymentHash, pkScript, outpoint,
		)
		if err != nil || preimage != nil || spent == nil {
			return preimage, spent, err
		}

		spentInput = spent
		if attempt == defaultClaimPreimageLookupAttempts {
			break
		}

		timer := time.NewTimer(defaultClaimPreimageLookupInterval)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, nil, ctx.Err()

		case <-timer.C:
		}
	}

	return nil, spentInput, nil
}

// findRefreshInputClaimObservation searches only the persisted first-leg
// outpoint and policy script for the server's matching preimage spend.
func (c *SwapClient) findRefreshInputClaimObservation(ctx context.Context,
	paymentHash lntypes.Hash, pkScript []byte, outpoint string) (
	*lntypes.Preimage, *VTXOInfo, error) {

	spentVTXOs, err := c.daemon.ListSpentVTXOs(ctx)
	if err != nil {
		return nil, nil, err
	}
	spent, err := exactRefreshVHTLC(
		spentVTXOs, outpoint, pkScript, "input",
	)
	if err != nil {
		return nil, nil, err
	}
	if spent == nil {
		spent, err = c.daemon.FindSpentVTXOByPkScript(ctx, pkScript)
		if err != nil {
			return nil, nil, err
		}
		if spent != nil && spent.Outpoint != outpoint {
			return nil, nil, nil
		}
	}
	if spent == nil {
		return nil, nil, nil
	}

	preimage, err := findMatchingPreimageInVTXO(spent, paymentHash)
	if err != nil || preimage != nil {
		return preimage, spent, err
	}
	if spent.SpentByTxID == "" {
		return nil, nil, nil
	}

	pkg, err := c.daemon.GetIndexedOORSession(
		ctx, pkScript, spent.SpentByTxID,
	)
	if err != nil {
		return nil, nil, err
	}
	preimage, err = findMatchingPreimageInCheckpoints(pkg, paymentHash)
	if err != nil || preimage != nil {
		return preimage, spent, err
	}

	return nil, spent, nil
}

// exactRefreshVHTLC selects one role-specific spent row and rejects an
// outpoint whose stored script disagrees with the negotiated policy.
func exactRefreshVHTLC(vtxos []VTXOInfo, outpoint string, pkScript []byte,
	role string) (*VTXOInfo, error) {

	for i := range vtxos {
		if vtxos[i].Outpoint != outpoint {
			continue
		}
		if len(vtxos[i].PkScript) > 0 &&
			!bytes.Equal(vtxos[i].PkScript, pkScript) {

			reason := fmt.Sprintf("refresh %s outpoint has "+
				"unexpected policy script", role)

			return nil, newInterventionError(
				reason, nil,
			)
		}

		return &vtxos[i], nil
	}

	return nil, nil
}

// initiateRefund cancels any dormant output-claim recovery before allowing a
// first-leg refund. This prevents a late unilateral claim from revealing the
// preimage after the client has begun taking its input back.
func (s *RefreshSession) initiateRefund(ctx context.Context,
	reason string) error {

	if s.state == RefreshStateClaimInitiated ||
		s.state == RefreshStateOutputClaimed ||
		s.state == RefreshStateCompleted {
		return newInterventionError(
			"cannot refund refresh input after claim intent "+
				"became durable", nil,
		)
	}
	if s.state == RefreshStateRefundInitiated {
		return nil
	}
	cancelState, lastErr, err := cancelRefreshVHTLCRecovery(
		ctx, s.client.daemon, s.claimRecoveryID, "refresh aborted "+
			"before preimage exposure", "",
	)
	if err != nil {
		return newRetryableActionError(err)
	}
	switch {
	case cancelState == recoveryStateCompleted:
		return s.markOutputClaimedWithRecovery(ctx, false)

	case cancelState == recoveryStateFailed ||
		recoveryIsActive(cancelState):
		return newInterventionError(
			refreshRecoveryCancelReason(
				"output claim", cancelState, lastErr,
			),
			nil,
		)
	}

	s.client.log.InfoS(ctx, "Refresh input refund initiated",
		btclog.Hex("hash", s.paymentHash[:]),
		slog.String("input_outpoint", s.inputOutpoint),
		slog.String("reason", reason),
	)

	return s.mutateAndPersist(ctx, func() error {
		s.interventionReason = reason

		return s.transition(refreshEventRefundInitiated)
	})
}

// completeRefund reconciles a claim that won the race, a prior refund, or the
// dormant recovery row before submitting the sender-only timeout spend.
func (s *RefreshSession) completeRefund(ctx context.Context) error {
	if len(s.outputPkScript) > 0 {
		claimed, err := s.client.receiveClaimAlreadyIndexedBounded(
			ctx, s.paymentHash, s.outputPkScript,
		)
		if err != nil && !errors.Is(
			err, errReceiveVHTLCSpentWithoutPreimage,
		) {
			return newRetryableActionError(err)
		}
		if claimed {
			return s.markOutputClaimed(ctx)
		}
	}
	if err := s.reconcileLiveInput(ctx); err != nil {
		return err
	}
	if s.state == RefreshStateRefunded {
		return nil
	}

	preimage, spent, err := s.client.waitForRefreshInputClaimObservation(
		ctx, s.paymentHash, s.inputPkScript, s.inputOutpoint,
	)
	if err != nil {
		if interventionReason(err) != "" {
			return err
		}

		return newRetryableActionError(
			fmt.Errorf("observe refresh input before refund: %w",
				err),
		)
	}
	if preimage != nil {
		return newInterventionError(
			"server claimed refresh input before the output "+
				"claim was observed", nil,
		)
	}
	refundOutput, err := s.client.daemon.FindLiveVTXOByPkScript(
		ctx, s.refundReceiveScript,
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("observe refresh refund output: %w", err),
		)
	}
	if refundOutput != nil && refundOutput.Outpoint != s.inputOutpoint &&
		refundOutput.AmountSat == s.inputAmount {
		return s.markRefunded(ctx, s.refundSessionID, true)
	}

	recoveryHandled, err := s.reconcileInputRefundRecovery(ctx)
	if err != nil {
		return err
	}
	if recoveryHandled {
		return nil
	}

	if s.refundSessionID != "" {
		session, err := s.client.daemon.GetOORSession(
			ctx, s.refundSessionID,
		)
		if err != nil {
			return newRetryableActionError(err)
		}
		if session != nil && session.GetStatus() ==
			waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED {
			return s.markRefunded(ctx, s.refundSessionID, true)
		}
		if session == nil || session.GetStatus() !=
			waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED {
			return waitForFixedPoll(ctx, s.client.waitPollInterval)
		}

		// FAILED only describes local OOR bookkeeping. Operator
		// finalization may already have consumed the input, so keep the
		// session identity and let exact indexing or recovery decide.
		failure := errors.New("refresh refund session failed")
		if session.GetFailureReason() != "" {
			failure = fmt.Errorf("refresh refund session "+
				"failed: %s", session.GetFailureReason())
		}
		if err := s.maybeEscalateRefreshRecovery(
			ctx, s.refundRecoveryID,
			s.cfg.VHTLCConfig.RefundLocktime,
			&s.refundRecoveryFailureAt, "input refund", failure,
		); err != nil {
			return newRetryableActionError(err)
		}

		return waitForFixedPoll(ctx, s.client.waitPollInterval)
	}
	if spent != nil {
		reason := "refresh input was spent without the shared " +
			"preimage or a confirmed client refund"
		if spent.SpentByTxID != "" {
			reason += " by " + spent.SpentByTxID
		}

		return newInterventionError(reason, nil)
	}

	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("get block height: %w", err),
		)
	}
	if height < s.cfg.VHTLCConfig.RefundLocktime {
		return waitForFixedPoll(ctx, s.client.waitPollInterval)
	}
	if err := s.ensureInputRefundRecoveryArmed(ctx); err != nil {
		return newRetryableActionError(err)
	}

	refundPath, err := s.inputPolicy.RefundWithoutReceiverPath()
	if err != nil {
		return newInterventionError(
			"build refresh input refund path", err,
		)
	}
	spendPath, err := refundPath.Encode()
	if err != nil {
		return newInterventionError(
			"encode refresh input refund path", err,
		)
	}
	refundSessionID, err := s.client.daemon.SendOORWithCustomInputs(
		ctx, s.refundReceivePubKey, s.inputAmount, []CustomInput{{
			Outpoint:           s.inputOutpoint,
			VTXOPolicyTemplate: s.inputPolicyTemplate,
			SpendPath:          spendPath,
			AmountSat:          s.inputAmount,
			PkScript:           s.inputPkScript,
		}},
	)
	if err != nil {
		if escalateErr := s.maybeEscalateRefreshRecovery(
			ctx, s.refundRecoveryID,
			s.cfg.VHTLCConfig.RefundLocktime,
			&s.refundRecoveryFailureAt, "input refund", err,
		); escalateErr != nil {
			return newRetryableActionError(escalateErr)
		}

		return newRetryableActionError(
			fmt.Errorf("refund refresh input vHTLC: %w", err),
		)
	}
	if refundSessionID == "" {
		return newRetryableActionError(
			fmt.Errorf("refresh refund returned empty session id"),
		)
	}

	s.refundSessionID = refundSessionID
	if err := s.persist(ctx); err != nil {
		return newRetryableActionError(
			fmt.Errorf("persist refresh refund session: %w", err),
		)
	}

	return waitForFixedPoll(ctx, s.client.waitPollInterval)
}

// markRefunded records safe terminal recovery after either the cooperative
// timeout spend or daemon-owned unroll completed.
func (s *RefreshSession) markRefunded(ctx context.Context, txid string,
	cancelRecovery bool) error {

	if cancelRecovery {
		_, _, err := cancelRefreshVHTLCRecovery(
			ctx, s.client.daemon, s.refundRecoveryID,
			recoveryReasonRefundSpendObserved, txid,
		)
		if err != nil {
			return newRetryableActionError(err)
		}
	}

	return s.mutateAndPersist(ctx, func() error {
		if txid != "" {
			s.refundSessionID = txid
		}

		return s.transition(refreshEventRefunded)
	})
}

// markRefundRecoveryWon records the daemon's already-completed refund through
// the same durable intent boundary used by cooperative timeout recovery.
func (s *RefreshSession) markRefundRecoveryWon(ctx context.Context) error {
	if s.state != RefreshStateRefundInitiated {
		if err := s.mutateAndPersist(ctx, func() error {
			return s.transition(refreshEventRefundInitiated)
		}); err != nil {
			return err
		}
	}

	return s.markRefunded(ctx, s.refundRecoveryID, false)
}

// cancelRefreshVHTLCRecovery reports which recovery branch actually won. The
// daemon treats cancellation as idempotent for terminal rows, so a nil RPC
// error alone is not evidence that the requested cancellation took effect.
func cancelRefreshVHTLCRecovery(ctx context.Context, daemon DaemonConn,
	recoveryID, reason, cooperativeTxid string) (waverpc.VHTLCRecoveryState,
	string, error) {

	if recoveryID == "" {
		return recoveryStateUnspecified, "", nil
	}

	resp, err := daemon.CancelVHTLCRecovery(
		ctx, &waverpc.CancelVHTLCRecoveryRequest{
			RecoveryId:      recoveryID,
			Reason:          reason,
			CooperativeTxid: cooperativeTxid,
		},
	)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return recoveryStateUnspecified, "", nil
		}

		return recoveryStateUnspecified, "", fmt.Errorf("cancel vhtlc "+
			"recovery %s: %w", recoveryID, err)
	}
	if resp == nil || resp.GetStatus() == nil {
		return recoveryStateUnspecified, "", fmt.Errorf("cancel vhtlc "+
			"recovery %s returned no status", recoveryID)
	}

	recoveryStatus := resp.GetStatus()
	state := recoveryStatus.GetState()
	if state == recoveryStateCompleted || state == recoveryStateFailed {
		return state, recoveryStatus.GetLastError(), nil
	}
	if recoveryStatus.GetUnrollFound() {
		switch recoveryStatus.GetUnrollStatus() {
		case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED:
			state = recoveryStateCompleted

		case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED:
			state = recoveryStateFailed

		case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING,
			waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING,
			waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING:

			state = recoveryStateUnrollStarted

		default:
			state = recoveryStateUnrollStarted
		}
	} else if recoveryStatus.GetEscalatedAtUnix() > 0 {
		// Cancellation does not stop an admitted unroll worker. A
		// crash before registry visibility can therefore omit the row
		// even though the opposite spend may still land.
		state = recoveryStateUnrollStarted
	}
	if state == recoveryStateArmed || state == recoveryStateUnspecified {
		return recoveryStateUnspecified, "", fmt.Errorf("cancel vhtlc "+
			"recovery %s returned non-cancelled state %s",
			recoveryID, state)
	}

	return state, recoveryStatus.GetLastError(), nil
}

// refreshRecoveryCancelReason describes an incompatible recovery result
// without losing the daemon's terminal failure detail.
func refreshRecoveryCancelReason(role string, state waverpc.VHTLCRecoveryState,
	lastErr string) string {

	reason := fmt.Sprintf("refresh %s recovery won the cancellation race "+
		"in state %s", role, state)
	if lastErr != "" {
		reason += ": " + lastErr
	}

	return reason
}

// reconcileInputRefundRecovery keeps the cooperative refund quiescent after
// the daemon has started unilateral recovery for the first-leg outpoint.
func (s *RefreshSession) reconcileInputRefundRecovery(ctx context.Context) (
	bool, error) {

	state, lastErr, err := getVHTLCRecoveryState(
		ctx, s.client.daemon, s.refundRecoveryID,
	)
	if err != nil {
		return false, newRetryableActionError(err)
	}

	switch {
	case state == recoveryStateCompleted:
		s.client.log.InfoS(
			ctx,
			"Refresh input refund recovery completed",
			btclog.Hex("hash", s.paymentHash[:]),
			slog.String("recovery_id", s.refundRecoveryID),
		)

		return true, s.markRefunded(
			ctx, s.refundRecoveryID, false,
		)

	case state == recoveryStateFailed:
		reason := "refresh input refund recovery failed"
		if lastErr != "" {
			reason += ": " + lastErr
		}

		return true, newInterventionError(reason, nil)

	case recoveryIsActive(state):
		s.client.log.DebugS(ctx, "Refresh input refund recovery active",
			btclog.Hex("hash", s.paymentHash[:]),
			slog.String("recovery_id", s.refundRecoveryID),
			slog.String("recovery_state", state.String()),
		)

		return true, waitForFixedPoll(
			ctx, s.client.waitPollInterval,
		)

	default:
		return false, nil
	}
}

// ensureInputRefundRecoveryArmed stores the sender-only recovery job before
// the server is allowed to proceed with the output leg.
func (s *RefreshSession) ensureInputRefundRecoveryArmed(
	ctx context.Context) error {

	if s.refundRecoveryID != "" {
		return nil
	}
	if s.inputOutpoint == "" || s.inputAmount <= 0 || s.cfg == nil {
		return fmt.Errorf("funded refresh input vHTLC is required")
	}
	recoveryID, err := s.armRefreshRecovery(
		ctx, refreshRecoveryLabel(
			refreshInputRecoveryLabel, s.inputOutpoint,
		),
		recoveryDirectionPay,
		recoveryActionRefundWithoutReceiver,
		s.inputOutpoint,
		s.inputAmount,
		s.clientPubKey,
		s.serverPubKey,
		s.cfg.VHTLCConfig,
		s.refundReceiveScript,
	)
	if err != nil {
		return fmt.Errorf("arm refresh input refund recovery: %w", err)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.refundRecoveryID = recoveryID

		return nil
	})
}

// ensureOutputClaimRecoveryArmed stores a dormant receiver claim using only
// the hash; Store.ResolvePreimage supplies the durable refresh preimage if the
// daemon later escalates unilaterally.
func (s *RefreshSession) ensureOutputClaimRecoveryArmed(
	ctx context.Context) error {

	if s.claimRecoveryID != "" {
		return nil
	}
	if s.outputOutpoint == "" || s.outputAmount <= 0 {
		return fmt.Errorf("funded refresh output vHTLC is required")
	}
	recoveryID, err := s.armRefreshRecovery(
		ctx, refreshRecoveryLabel(
			refreshOutputRecoveryLabel, s.outputOutpoint,
		),
		recoveryDirectionReceive,
		recoveryActionClaim,
		s.outputOutpoint,
		s.outputAmount,
		s.outputSenderPubKey,
		s.clientPubKey,
		s.outputConfig,
		s.claimReceiveScript,
	)
	if err != nil {
		return fmt.Errorf("arm refresh output claim recovery: %w", err)
	}

	return s.mutateAndPersist(ctx, func() error {
		s.claimRecoveryID = recoveryID

		return nil
	})
}

// refreshRecoveryLabel scopes one daemon idempotency key to the current live
// outpoint so a cooperative rollover can arm a distinct replacement recovery.
func refreshRecoveryLabel(label, outpoint string) string {
	return label + ":" + outpoint
}

// armRefreshRecovery builds either leg's daemon request from one shared set of
// vHTLC invariants while keeping request IDs role-scoped for the shared hash.
func (s *RefreshSession) armRefreshRecovery(ctx context.Context, label string,
	direction waverpc.VHTLCRecoveryDirection,
	action waverpc.VHTLCRecoveryAction, outpoint string, amount int64,
	sender, receiver *btcec.PublicKey, cfg VHTLCConfig,
	destinationScript []byte) (string, error) {

	senderBytes, err := pubKeyBytesForRecovery(sender, "sender")
	if err != nil {
		return "", err
	}
	receiverBytes, err := pubKeyBytesForRecovery(receiver, "receiver")
	if err != nil {
		return "", err
	}
	operatorBytes, err := pubKeyBytesForRecovery(
		s.operatorPubKey, "operator",
	)
	if err != nil {
		return "", err
	}
	if len(destinationScript) == 0 {
		return "", fmt.Errorf("recovery destination script is required")
	}
	policy := s.client.recoveryPolicy.WithDefaults()
	resp, err := s.client.daemon.ArmVHTLCRecovery(
		ctx, &waverpc.ArmVHTLCRecoveryRequest{
			RequestId: recoveryRequestID(
				label, s.paymentHash, action,
			),
			SwapId: append(
				[]byte(nil), s.paymentHash[:]...,
			),
			Direction:      direction,
			Action:         action,
			VtxoOutpoint:   outpoint,
			VtxoAmountSat:  amount,
			SenderPubkey:   senderBytes,
			ReceiverPubkey: receiverBytes,
			ServerPubkey:   operatorBytes,
			RefundLocktime: int32(cfg.RefundLocktime),
			UnilateralClaimDelay: int32(
				cfg.UnilateralClaimDelay,
			),
			UnilateralRefundDelay: int32(
				cfg.UnilateralRefundDelay,
			),
			UnilateralRefundWithoutReceiverDelay: int32(
				cfg.UnilateralRefundWithoutReceiverDelay,
			),
			PreimageHash: append(
				[]byte(nil), s.paymentHash[:]...,
			),
			SignerKeyFamily: recoverySignerFamily(),
			SignerKeyIndex:  recoverySignerKeyIndex,
			DestinationScript: append(
				[]byte(nil), destinationScript...,
			),
			MaxFeeRateSatPerKw: policy.MaxFeeRateSatPerKW,
		},
	)
	if err != nil {
		return "", err
	}
	if resp.GetRecoveryId() == "" {
		return "", fmt.Errorf("arm refresh recovery returned empty id")
	}

	return resp.GetRecoveryId(), nil
}

// maybeEscalateRefreshRecovery applies the existing SDK grace and deadline
// policy to either refresh leg after cooperative spending fails.
func (s *RefreshSession) maybeEscalateRefreshRecovery(ctx context.Context,
	recoveryID string, deadline uint32, firstFailureAt *time.Time,
	phase string, cause error) error {

	if recoveryID == "" {
		return nil
	}
	if firstFailureAt.IsZero() {
		*firstFailureAt = s.client.currentTime()
	}
	height, err := s.client.daemon.BlockHeight(ctx)
	if err != nil {
		return fmt.Errorf("get block height for %s recovery: %w", phase,
			err)
	}
	decision := decideRecoveryEscalation(
		s.client.recoveryPolicy, *firstFailureAt,
		s.client.currentTime(), height, deadline,
	)
	if !decision.Escalate {
		return nil
	}

	reason := fmt.Sprintf("refresh %s failed: %v", phase, cause)

	return escalateVHTLCRecovery(
		ctx, s.client.daemon, recoveryID, reason,
	)
}

// bytesEqual is retained as a small helper for persisted key and script
// comparisons that should treat nil and empty slices equivalently.
func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
