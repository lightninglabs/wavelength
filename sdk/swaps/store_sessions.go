package swaps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	swapsqlc "github.com/lightninglabs/darepo-client/sdk/swaps/sqlc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ResumeReceiveViaLightning reloads one persisted receive session by payment
// hash from the isolated swap store.
func (c *SwapClient) ResumeReceiveViaLightning(ctx context.Context,
	paymentHash lntypes.Hash) (*ReceiveSession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	row, err := c.store.queries.GetReceiveSwap(ctx, paymentHash[:])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("receive session not found")
		}

		return nil, fmt.Errorf("load receive session: %w", err)
	}

	return receiveSessionFromRow(c, row)
}

// ResumePayViaLightning reloads one persisted pay session by payment hash from
// the isolated swap store.
func (c *SwapClient) ResumePayViaLightning(ctx context.Context,
	paymentHash lntypes.Hash) (*PaySession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	row, err := c.store.queries.GetPaySwap(ctx, paymentHash[:])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("pay session not found")
		}

		return nil, fmt.Errorf("load pay session: %w", err)
	}

	return paySessionFromRow(c, row)
}

// ListSwapSummaries returns persisted pay and receive sessions in creation
// order. When pendingOnly is true, terminal sessions are omitted.
func (c *SwapClient) ListSwapSummaries(ctx context.Context,
	pendingOnly bool) ([]SwapSummary, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	var (
		payRows     []swapsqlc.PaySwap
		receiveRows []swapsqlc.ReceiveSwap
		err         error
	)

	if pendingOnly {
		payRows, err = c.store.queries.ListPendingPaySwaps(ctx)
	} else {
		payRows, err = c.store.queries.ListPaySwaps(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("list pay sessions: %w", err)
	}

	if pendingOnly {
		receiveRows, err = c.store.queries.ListPendingReceiveSwaps(ctx)
	} else {
		receiveRows, err = c.store.queries.ListReceiveSwaps(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("list receive sessions: %w", err)
	}

	summaries := make(
		[]SwapSummary, 0, len(payRows)+len(receiveRows),
	)
	for i := range payRows {
		summary, err := paySummaryFromRow(payRows[i])
		if err != nil {
			return nil, err
		}

		summaries = append(summaries, summary)
	}
	for i := range receiveRows {
		summary, err := receiveSummaryFromRow(receiveRows[i])
		if err != nil {
			return nil, err
		}

		summaries = append(summaries, summary)
	}

	sort.SliceStable(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt.Before(summaries[j].CreatedAt)
	})

	return summaries, nil
}

// ListPaySessions returns every persisted pay session from the isolated swap
// store.
func (c *SwapClient) ListPaySessions(
	ctx context.Context) ([]*PaySession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	rows, err := c.store.queries.ListPaySwaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pay sessions: %w", err)
	}

	sessions := make([]*PaySession, 0, len(rows))
	for i := range rows {
		session, err := paySessionFromRow(c, rows[i])
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// ListPendingReceiveSessions returns every non-terminal persisted receive
// session from the isolated swap store.
func (c *SwapClient) ListPendingReceiveSessions(
	ctx context.Context) ([]*ReceiveSession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	rows, err := c.store.queries.ListPendingReceiveSwaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending receive sessions: %w", err)
	}

	sessions := make([]*ReceiveSession, 0, len(rows))
	for i := range rows {
		session, err := receiveSessionFromRow(c, rows[i])
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// ListReceiveSessions returns every persisted receive session from the
// isolated swap store.
func (c *SwapClient) ListReceiveSessions(
	ctx context.Context) ([]*ReceiveSession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	rows, err := c.store.queries.ListReceiveSwaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list receive sessions: %w", err)
	}

	sessions := make([]*ReceiveSession, 0, len(rows))
	for i := range rows {
		session, err := receiveSessionFromRow(c, rows[i])
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// ListPendingPaySessions returns every non-terminal persisted pay session from
// the isolated swap store.
func (c *SwapClient) ListPendingPaySessions(
	ctx context.Context) ([]*PaySession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	rows, err := c.store.queries.ListPendingPaySwaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending pay sessions: %w", err)
	}

	sessions := make([]*PaySession, 0, len(rows))
	for i := range rows {
		session, err := paySessionFromRow(c, rows[i])
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// paySummaryFromRow converts one persisted pay row into the public list view.
func paySummaryFromRow(row swapsqlc.PaySwap) (SwapSummary, error) {
	state, err := parsePayState(row.State)
	if err != nil {
		return SwapSummary{}, err
	}

	paymentHash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return SwapSummary{}, err
	}

	return SwapSummary{
		Direction:        SwapDirectionPay,
		PaymentHash:      paymentHash,
		State:            state.String(),
		Pending:          !state.IsTerminal(),
		AmountSat:        row.AmountSat,
		FeeSat:           uint64(row.FeeSat),
		MaxFeeSat:        uint64(row.MaxFeeSat),
		VHTLCOutpoint:    row.VhtlcOutpoint,
		VHTLCAmountSat:   row.VhtlcAmount,
		FundingSessionID: row.FundingSessionID,
		RefundSessionID:  row.RefundSessionID,
		TerminalReason:   row.InterventionReason,
		CreatedAt:        time.Unix(row.CreatedAtUnix, 0),
		UpdatedAt:        time.Unix(row.UpdatedAtUnix, 0),
		Deadline:         time.Unix(row.ExpiryUnix, 0),
		RefundLocktime:   uint32(row.RefundLocktime),
	}, nil
}

// receiveSummaryFromRow converts one persisted receive row into the public
// list view.
func receiveSummaryFromRow(row swapsqlc.ReceiveSwap) (SwapSummary, error) {
	state, err := parseReceiveState(row.State)
	if err != nil {
		return SwapSummary{}, err
	}

	paymentHash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return SwapSummary{}, err
	}

	return SwapSummary{
		Direction:      SwapDirectionReceive,
		PaymentHash:    paymentHash,
		State:          state.String(),
		Pending:        !state.IsTerminal(),
		AmountSat:      row.AmountSat,
		VHTLCOutpoint:  row.VhtlcOutpoint,
		VHTLCAmountSat: row.VhtlcAmount,
		ClaimSessionID: row.ClaimSessionID,
		TerminalReason: row.InterventionReason,
		CreatedAt:      time.Unix(row.CreatedAtUnix, 0),
		UpdatedAt:      time.Unix(row.UpdatedAtUnix, 0),
		Deadline:       time.Unix(row.DeadlineUnix, 0),
		RefundLocktime: uint32(row.RefundLocktime),
	}, nil
}

// rememberReceiveFunding updates the live vHTLC funding details and persists
// them when swap-store durability is enabled.
func (s *ReceiveSession) rememberReceiveFunding(ctx context.Context,
	outpoint string, amount int64) error {

	if s.vhtlcOutpoint == outpoint && s.vhtlcAmount == amount {
		return nil
	}

	return s.mutateAndPersist(ctx, func() error {
		if outpoint != "" {
			s.vhtlcOutpoint = outpoint
		}
		if amount != 0 {
			s.vhtlcAmount = amount
		}

		return nil
	})
}

// persistClaimSessionID stores the accepted claim session id without rolling
// back the in-memory id on persistence failure.
func (s *ReceiveSession) persistClaimSessionID(ctx context.Context,
	claimSessionID string) error {

	s.claimSessionID = claimSessionID
	if err := s.persist(ctx); err != nil {
		return fmt.Errorf("persist receive claim session id: %w", err)
	}

	return nil
}

// mutateAndPersist applies one in-memory mutation and rolls it back if the
// follow-up store write fails.
func (s *ReceiveSession) mutateAndPersist(ctx context.Context,
	mutate func() error) error {

	if s == nil {
		return fmt.Errorf("receive session must be provided")
	}

	snapshot := *s
	err := mutate()
	if err != nil {
		*s = snapshot

		return err
	}

	err = s.persist(ctx)
	if err != nil {
		*s = snapshot

		return err
	}

	return nil
}

// persist writes the full receive session snapshot into the isolated swap
// store when one is configured.
func (s *ReceiveSession) persist(ctx context.Context) error {
	if s == nil || s.client == nil || s.client.store == nil {
		return nil
	}
	if s.client.store.queries == nil ||
		s.PaymentHash == (lntypes.Hash{}) {

		return nil
	}

	now := s.client.currentTime().Unix()

	params := swapsqlc.UpsertReceiveSwapParams{
		PaymentHash:  append([]byte(nil), s.PaymentHash[:]...),
		AmountSat:    int64(s.amountSat),
		State:        s.state.String(),
		Invoice:      s.Invoice,
		Preimage:     append([]byte(nil), s.Preimage[:]...),
		DeadlineUnix: s.deadline.Unix(),
		ClientPubkey: append(
			[]byte(nil), s.clientPubKey.SerializeCompressed()...,
		),
		OperatorPubkey: append(
			[]byte(nil), s.operatorPubKey.SerializeCompressed()...,
		),
		SwapServerPubkey: append([]byte(nil),
			s.swapServerPubKey.SerializeCompressed()...),
		RefundLocktime: int64(s.vhtlcConfig.RefundLocktime),
		UnilateralClaimDelay: int64(
			s.vhtlcConfig.UnilateralClaimDelay,
		),
		UnilateralRefundDelay: int64(
			s.vhtlcConfig.UnilateralRefundDelay,
		),
		UnilateralRefundWithoutReceiverDelay: int64(
			s.vhtlcConfig.UnilateralRefundWithoutReceiverDelay,
		),
		VhtlcPkscript: append([]byte(nil), s.vhtlcPkScript...),
		VhtlcPolicyTemplate: append(
			[]byte(nil), s.vhtlcPolicyTemplate...,
		),
		VhtlcOutpoint:      s.vhtlcOutpoint,
		VhtlcAmount:        s.vhtlcAmount,
		ClaimSessionID:     s.claimSessionID,
		InterventionReason: s.interventionReason,
		CreatedAtUnix:      s.createdAt.Unix(),
		UpdatedAtUnix:      now,
	}

	if s.createdAt.IsZero() {
		params.CreatedAtUnix = now
		s.createdAt = time.Unix(now, 0)
	}

	err := s.client.store.queries.UpsertReceiveSwap(ctx, params)
	if err != nil {
		return fmt.Errorf("persist receive session: %w", err)
	}

	s.updatedAt = time.Unix(now, 0)

	return nil
}

// mutateAndPersist applies one in-memory pay-session mutation and rolls it
// back if the store write fails.
func (s *paySession) mutateAndPersist(ctx context.Context,
	mutate func() error) error {

	if s == nil {
		return fmt.Errorf("pay session must be provided")
	}

	snapshot := *s
	err := mutate()
	if err != nil {
		*s = snapshot

		return err
	}

	err = s.persist(ctx)
	if err != nil {
		*s = snapshot

		return err
	}

	return nil
}

// persistFundingSessionID stores the accepted funding session id without
// rolling back the in-memory id on persistence failure.
func (s *paySession) persistFundingSessionID(ctx context.Context,
	fundingSessionID string) error {

	s.fundingSessionID = fundingSessionID
	if err := s.persist(ctx); err != nil {
		return fmt.Errorf("persist pay funding session id: %w", err)
	}

	return nil
}

// persist writes the full pay session snapshot into the isolated swap store
// when one is configured.
func (s *paySession) persist(ctx context.Context) error {
	if s == nil || s.client == nil || s.client.store == nil ||
		s.client.store.queries == nil || s.cfg == nil ||
		s.cfg.PaymentHash == (lntypes.Hash{}) {

		return nil
	}

	now := s.client.currentTime().Unix()

	params := swapsqlc.UpsertPaySwapParams{
		PaymentHash: append([]byte(nil), s.cfg.PaymentHash[:]...),
		Invoice:     s.invoice,
		MaxFeeSat:   int64(s.maxFeeSat),
		State:       s.state.String(),
		AmountSat:   s.cfg.AmountSat,
		FeeSat:      int64(s.cfg.FeeSat),
		ExpiryUnix:  s.cfg.Expiry.Unix(),
		ClientPubkey: append(
			[]byte(nil), s.clientPubKey.SerializeCompressed()...,
		),
		OperatorPubkey: append(
			[]byte(nil), s.operatorPubKey.SerializeCompressed()...,
		),
		ServerPubkey: append(
			[]byte(nil), s.serverPubKey.SerializeCompressed()...,
		),
		RefundLocktime: int64(s.cfg.VHTLCConfig.RefundLocktime),
		UnilateralClaimDelay: int64(
			s.cfg.VHTLCConfig.UnilateralClaimDelay,
		),
		UnilateralRefundDelay: int64(
			s.cfg.VHTLCConfig.UnilateralRefundDelay,
		),
		UnilateralRefundWithoutReceiverDelay: int64(
			s.cfg.VHTLCConfig.UnilateralRefundWithoutReceiverDelay,
		),
		VhtlcPkscript: append([]byte(nil), s.vhtlcPkScript...),
		VhtlcPolicyTemplate: append(
			[]byte(nil), s.vhtlcPolicyTemplate...,
		),
		VhtlcOutpoint:    s.vhtlcOutpoint,
		VhtlcAmount:      s.vhtlcAmount,
		FundingSessionID: s.fundingSessionID,
		RefundReceivePubkey: append(
			[]byte(nil), s.refundReceivePubKey...,
		),
		RefundReceivePkscript: append(
			[]byte(nil), s.refundReceiveScript...,
		),
		RefundSessionID:    s.refundSessionID,
		InterventionReason: s.interventionReason,
		CreatedAtUnix:      s.createdAt.Unix(),
		UpdatedAtUnix:      now,
	}
	if s.preimage != nil {
		params.Preimage = append([]byte(nil), s.preimage[:]...)
	}

	if s.createdAt.IsZero() {
		params.CreatedAtUnix = now
		s.createdAt = time.Unix(now, 0)
	}

	err := s.client.store.queries.UpsertPaySwap(ctx, params)
	if err != nil {
		return fmt.Errorf("persist pay session: %w", err)
	}

	s.updatedAt = time.Unix(now, 0)

	return nil
}

// receiveSessionFromRow reconstructs one receive session from its persisted SQL
// row.
func receiveSessionFromRow(c *SwapClient,
	row swapsqlc.ReceiveSwap) (*ReceiveSession, error) {

	state, err := parseReceiveState(row.State)
	if err != nil {
		return nil, err
	}

	clientKey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse receive client pubkey: %w", err)
	}

	operatorKey, err := btcec.ParsePubKey(row.OperatorPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse receive operator pubkey: %w", err)
	}

	swapServerKey, err := btcec.ParsePubKey(row.SwapServerPubkey)
	if err != nil {
		return nil, fmt.Errorf(
			"parse receive swap-server pubkey: %w", err,
		)
	}

	paymentHash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return nil, err
	}

	preimage, err := preimageFromBytes(row.Preimage)
	if err != nil {
		return nil, err
	}

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:       swapServerKey,
		Receiver:     clientKey,
		Server:       operatorKey,
		PreimageHash: paymentHash,
		RefundLocktime: uint32(
			row.RefundLocktime,
		),
		UnilateralClaimDelay: uint32(
			row.UnilateralClaimDelay,
		),
		UnilateralRefundDelay: uint32(
			row.UnilateralRefundDelay,
		),
		UnilateralRefundWithoutReceiverDelay: uint32(
			row.UnilateralRefundWithoutReceiverDelay,
		),
	})
	if err != nil {
		return nil, fmt.Errorf("rebuild receive vHTLC policy: %w", err)
	}

	return &ReceiveSession{
		Invoice:     row.Invoice,
		Preimage:    preimage,
		PaymentHash: paymentHash,
		client:      c,
		amountSat:   btcutil.Amount(row.AmountSat),
		state:       state,
		deadline:    time.Unix(row.DeadlineUnix, 0),
		vhtlcConfig: restoreVHTLCConfig(row.RefundLocktime, row.UnilateralClaimDelay, row.UnilateralRefundDelay, row.UnilateralRefundWithoutReceiverDelay, row.SwapServerPubkey), //nolint:ll
		vhtlcPolicy: policy,
		vhtlcPolicyTemplate: append(
			[]byte(nil), row.VhtlcPolicyTemplate...,
		),
		vhtlcPkScript:      append([]byte(nil), row.VhtlcPkscript...),
		vhtlcOutpoint:      row.VhtlcOutpoint,
		vhtlcAmount:        row.VhtlcAmount,
		claimSessionID:     row.ClaimSessionID,
		interventionReason: row.InterventionReason,
		clientPubKey:       clientKey,
		operatorPubKey:     operatorKey,
		swapServerPubKey:   swapServerKey,
		createdAt:          time.Unix(row.CreatedAtUnix, 0),
		updatedAt:          time.Unix(row.UpdatedAtUnix, 0),
	}, nil
}

// paySessionFromRow reconstructs one pay session from its persisted SQL row.
func paySessionFromRow(c *SwapClient,
	row swapsqlc.PaySwap) (*PaySession, error) {

	state, err := parsePayState(row.State)
	if err != nil {
		return nil, err
	}

	clientKey, err := btcec.ParsePubKey(row.ClientPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse pay client pubkey: %w", err)
	}

	operatorKey, err := btcec.ParsePubKey(row.OperatorPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse pay operator pubkey: %w", err)
	}

	serverKey, err := btcec.ParsePubKey(row.ServerPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse pay server pubkey: %w", err)
	}

	paymentHash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return nil, err
	}

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:       clientKey,
		Receiver:     serverKey,
		Server:       operatorKey,
		PreimageHash: paymentHash,
		RefundLocktime: uint32(
			row.RefundLocktime,
		),
		UnilateralClaimDelay: uint32(
			row.UnilateralClaimDelay,
		),
		UnilateralRefundDelay: uint32(
			row.UnilateralRefundDelay,
		),
		UnilateralRefundWithoutReceiverDelay: uint32(
			row.UnilateralRefundWithoutReceiverDelay,
		),
	})
	if err != nil {
		return nil, fmt.Errorf("rebuild pay vHTLC policy: %w", err)
	}

	cfg := &InSwapConfig{
		PaymentHash:  paymentHash,
		AmountSat:    row.AmountSat,
		FeeSat:       uint64(row.FeeSat),
		ServerPubkey: serverKey,
		VHTLCConfig: restoreVHTLCConfig(
			row.RefundLocktime, row.UnilateralClaimDelay,
			row.UnilateralRefundDelay,
			row.UnilateralRefundWithoutReceiverDelay,
			row.ServerPubkey,
		),
		Expiry: time.Unix(row.ExpiryUnix, 0),
	}

	session := &paySession{
		client:        c,
		invoice:       row.Invoice,
		maxFeeSat:     uint64(row.MaxFeeSat),
		state:         state,
		cfg:           cfg,
		vhtlcPolicy:   policy,
		vhtlcPkScript: append([]byte(nil), row.VhtlcPkscript...),
		vhtlcPolicyTemplate: append(
			[]byte(nil), row.VhtlcPolicyTemplate...,
		),
		vhtlcOutpoint:    row.VhtlcOutpoint,
		vhtlcAmount:      row.VhtlcAmount,
		fundingSessionID: row.FundingSessionID,
		refundReceivePubKey: append(
			[]byte(nil), row.RefundReceivePubkey...,
		),
		refundReceiveScript: append(
			[]byte(nil), row.RefundReceivePkscript...,
		),
		refundSessionID:    row.RefundSessionID,
		interventionReason: row.InterventionReason,
		clientPubKey:       clientKey,
		operatorPubKey:     operatorKey,
		serverPubKey:       serverKey,
		createdAt:          time.Unix(row.CreatedAtUnix, 0),
		updatedAt:          time.Unix(row.UpdatedAtUnix, 0),
	}

	if len(row.Preimage) != 0 {
		preimage, err := preimageFromBytes(row.Preimage)
		if err != nil {
			return nil, err
		}

		session.preimage = &preimage
	}

	return session, nil
}

// restoreVHTLCConfig rebuilds the persisted vHTLC timing config.
func restoreVHTLCConfig(refundLocktime, claimDelay, refundDelay,
	refundWithoutReceiverDelay int64, swapServerPubkey []byte) VHTLCConfig {

	return VHTLCConfig{
		RefundLocktime: uint32(refundLocktime),
		UnilateralClaimDelay: uint32(
			claimDelay,
		),
		UnilateralRefundDelay: uint32(
			refundDelay,
		),
		UnilateralRefundWithoutReceiverDelay: uint32(
			refundWithoutReceiverDelay,
		),
		SwapServerPubkey: append(
			[]byte(nil), swapServerPubkey...,
		),
	}
}

// hashFromBytes decodes one persisted payment hash.
func hashFromBytes(raw []byte) (lntypes.Hash, error) {
	if len(raw) != lntypes.HashSize {
		return lntypes.Hash{}, fmt.Errorf(
			"payment hash must be %d bytes",
			lntypes.HashSize,
		)
	}

	var hash lntypes.Hash
	copy(hash[:], raw)

	return hash, nil
}

// preimageFromBytes decodes one persisted preimage.
func preimageFromBytes(raw []byte) (lntypes.Preimage, error) {
	if len(raw) != lntypes.PreimageSize {
		return lntypes.Preimage{}, fmt.Errorf(
			"preimage must be %d bytes",
			lntypes.PreimageSize,
		)
	}

	var preimage lntypes.Preimage
	copy(preimage[:], raw)

	return preimage, nil
}

// parseReceiveState decodes one persisted receive state string.
func parseReceiveState(state string) (ReceiveState, error) {
	switch state {
	case ReceiveStateCreated.String():
		return ReceiveStateCreated, nil
	case ReceiveStateInvoiceCreated.String():
		return ReceiveStateInvoiceCreated, nil
	case ReceiveStateVHTLCFunded.String():
		return ReceiveStateVHTLCFunded, nil
	case ReceiveStateClaimInitiated.String():
		return ReceiveStateClaimInitiated, nil
	case ReceiveStateCompleted.String():
		return ReceiveStateCompleted, nil
	case ReceiveStateExpired.String():
		return ReceiveStateExpired, nil
	case ReceiveStateNeedsIntervention.String():
		return ReceiveStateNeedsIntervention, nil
	case ReceiveStateFailed.String():
		return ReceiveStateFailed, nil
	default:
		return ReceiveStateFailed, fmt.Errorf(
			"unknown receive state %q", state,
		)
	}
}

// parsePayState decodes one persisted pay state string.
func parsePayState(state string) (PayState, error) {
	switch state {
	case PayStateCreated.String():
		return PayStateCreated, nil
	case PayStateSwapCreated.String():
		return PayStateSwapCreated, nil
	case PayStateFundingInitiated.String():
		return PayStateFundingInitiated, nil
	case PayStateVHTLCFunded.String():
		return PayStateVHTLCFunded, nil
	case PayStateWaitingForClaim.String():
		return PayStateWaitingForClaim, nil
	case PayStateCompleted.String():
		return PayStateCompleted, nil
	case PayStateExpired.String():
		return PayStateExpired, nil
	case PayStateRefundInitiated.String():
		return PayStateRefundInitiated, nil
	case PayStateRefunded.String():
		return PayStateRefunded, nil
	case PayStateNeedsIntervention.String():
		return PayStateNeedsIntervention, nil
	case PayStateFailed.String():
		return PayStateFailed, nil
	default:
		return PayStateFailed, fmt.Errorf(
			"unknown pay state %q", state,
		)
	}
}
