package swaps

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
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

// GetSwapSummary returns one persisted pay or receive swap summary by payment
// hash. Pay swaps are checked first because payment hashes are globally unique
// for the daemon-owned swap store, and the second lookup is only needed when
// the hash belongs to a receive swap.
func (c *SwapClient) GetSwapSummary(ctx context.Context,
	paymentHash lntypes.Hash) (SwapSummary, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return SwapSummary{}, fmt.Errorf("swap store is not configured")
	}

	payRow, err := c.store.queries.GetPaySwap(ctx, paymentHash[:])
	if err == nil {
		return paySummaryFromRow(payRow)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SwapSummary{}, fmt.Errorf("load pay summary: %w", err)
	}

	receiveRow, err := c.store.queries.GetReceiveSwap(ctx, paymentHash[:])
	if err == nil {
		return receiveSummaryFromRow(receiveRow)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return SwapSummary{}, ErrSwapSummaryNotFound
	}

	return SwapSummary{}, fmt.Errorf("load receive summary: %w", err)
}

// ListSwapSummaries returns persisted pay and receive sessions in creation
// order. When pendingOnly is true, terminal sessions are omitted.
func (c *SwapClient) ListSwapSummaries(ctx context.Context, pendingOnly bool) (
	[]SwapSummary, error) {

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
func (c *SwapClient) ListPaySessions(ctx context.Context) ([]*PaySession,
	error) {

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
func (c *SwapClient) ListPendingReceiveSessions(ctx context.Context) (
	[]*ReceiveSession, error) {

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
func (c *SwapClient) ListReceiveSessions(ctx context.Context) (
	[]*ReceiveSession, error) {

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
func (c *SwapClient) ListPendingPaySessions(ctx context.Context) ([]*PaySession,
	error) {

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

// ResolvePreimage loads the swap-owned raw preimage for daemon claim recovery.
// The vHTLC recovery row stores only preimage_hash plus swap_id; sdk/swaps uses
// the Lightning payment hash as swap_id, so this resolver verifies both
// identifiers before returning the durable receive-session preimage.
func (s *Store) ResolvePreimage(ctx context.Context, swapID []byte,
	preimageHash lntypes.Hash) (lntypes.Preimage, error) {

	if s == nil || s.queries == nil {
		return lntypes.Preimage{}, fmt.Errorf("swap store is not " +
			"configured")
	}
	if len(swapID) != 0 && !bytes.Equal(swapID, preimageHash[:]) {
		return lntypes.Preimage{}, fmt.Errorf("swap id does not " +
			"match preimage hash")
	}

	row, err := s.queries.GetReceiveSwap(ctx, preimageHash[:])
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lntypes.Preimage{}, fmt.Errorf("receive swap " +
				"preimage not found")
		}

		return lntypes.Preimage{}, fmt.Errorf("load receive swap "+
			"preimage: %w", err)
	}

	preimage, err := preimageFromBytes(row.Preimage)
	if err != nil {
		return lntypes.Preimage{}, err
	}
	if !preimage.Matches(preimageHash) {
		return lntypes.Preimage{}, fmt.Errorf("receive swap preimage " +
			"does not match requested hash")
	}

	return preimage, nil
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

	// The preimage column is only populated once the server's vHTLC claim
	// reveals it, so a pending pay swap leaves it nil. When present it is
	// the proof of payment for the paid invoice.
	var preimage *lntypes.Preimage
	if len(row.Preimage) != 0 {
		decoded, err := preimageFromBytes(row.Preimage)
		if err != nil {
			return SwapSummary{}, err
		}

		preimage = &decoded
	}

	return SwapSummary{
		Direction:        SwapDirectionPay,
		PaymentHash:      paymentHash,
		Preimage:         preimage,
		Invoice:          row.Invoice,
		State:            state.String(),
		Pending:          !state.IsTerminal(),
		AmountSat:        row.AmountSat,
		FeeSat:           uint64(row.FeeSat),
		MaxFeeSat:        uint64(row.MaxFeeSat),
		VHTLCOutpoint:    row.VhtlcOutpoint,
		VHTLCAmountSat:   row.VhtlcAmount,
		FundingSessionID: row.FundingSessionID,
		RefundSessionID:  row.RefundSessionID,
		SettlementType:   SettlementType(row.SettlementType),
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

	senderPubKey, err := optionalPubKeyFromBytes(
		row.SwapServerPubkey, "receive sender pubkey",
	)
	if err != nil {
		return SwapSummary{}, err
	}

	requestedAmountSat := uint64(row.RequestedAmountSat)
	if requestedAmountSat == 0 {
		requestedAmountSat = uint64(row.AmountSat)
	}

	return SwapSummary{
		Direction:          SwapDirectionReceive,
		PaymentHash:        paymentHash,
		Invoice:            row.Invoice,
		State:              state.String(),
		Pending:            !state.IsTerminal(),
		AmountSat:          row.AmountSat,
		PayerFeeMsat:       uint64(row.PayerFeeMsat),
		VHTLCOutpoint:      row.VhtlcOutpoint,
		VHTLCAmountSat:     row.VhtlcAmount,
		ClaimSessionID:     row.ClaimSessionID,
		SettlementType:     SettlementType(row.SettlementType),
		RequestedAmountSat: requestedAmountSat,
		AvailableCreditSat: uint64(row.AvailableCreditSat),
		AttachedCreditSat:  uint64(row.AttachedCreditSat),
		DustLimitSat:       uint64(row.DustLimitSat),
		SenderPubkey:       senderPubKey,
		TerminalReason:     row.InterventionReason,
		CreatedAt:          time.Unix(row.CreatedAtUnix, 0),
		UpdatedAt:          time.Unix(row.UpdatedAtUnix, 0),
		Deadline:           time.Unix(row.DeadlineUnix, 0),
		RefundLocktime:     uint32(row.RefundLocktime),
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
	if s.payerFeeMsat > math.MaxInt64 {
		return fmt.Errorf("payer fee %d msat overflows int64",
			s.payerFeeMsat)
	}

	params := swapsqlc.UpsertReceiveSwapParams{
		PaymentHash:  append([]byte(nil), s.PaymentHash[:]...),
		AmountSat:    int64(s.amountSat),
		PayerFeeMsat: int64(s.payerFeeMsat),
		State:        s.state.String(),
		Invoice:      s.Invoice,
		Preimage:     append([]byte(nil), s.Preimage[:]...),
		DeadlineUnix: s.deadline.Unix(),
		ClientPubkey: cloneBytesOrEmpty(pubKeyBytes(s.clientPubKey)),
		PaymentAddr:  cloneBytesOrEmpty(s.paymentAddr[:]),
		OperatorPubkey: cloneBytesOrEmpty(
			pubKeyBytes(s.operatorPubKey),
		),
		SwapServerPubkey: cloneBytesOrEmpty(
			pubKeyBytes(s.swapServerPubKey),
		),
		SettlementType: string(s.settlementType),
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
		VhtlcPkscript:       cloneBytesOrEmpty(s.vhtlcPkScript),
		VhtlcPolicyTemplate: cloneBytesOrEmpty(s.vhtlcPolicyTemplate),
		VhtlcOutpoint:       s.vhtlcOutpoint,
		VhtlcAmount:         s.vhtlcAmount,
		PendingHtlcAckCursor: int64(
			s.pendingHTLCAckCursor,
		),
		ClaimReceivePubkey:   cloneBytesOrEmpty(s.claimReceivePubKey),
		ClaimReceivePkscript: cloneBytesOrEmpty(s.claimReceiveScript),
		ClaimSessionID:       s.claimSessionID,
		ClaimRecoveryID:      s.claimRecoveryID,
		InterventionReason:   s.interventionReason,
		RequestedAmountSat:   int64(s.requestedAmountSat),
		AvailableCreditSat:   int64(s.availableCreditSat),
		AttachedCreditSat:    int64(s.attachedCreditSat),
		DustLimitSat:         int64(s.dustLimitSat),
		CreatedAtUnix:        s.createdAt.Unix(),
		UpdatedAtUnix:        now,
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

// cloneBytesOrEmpty keeps optional BLOB columns non-NULL while preserving a
// defensive copy for values that have been negotiated.
func cloneBytesOrEmpty(src []byte) []byte {
	if len(src) == 0 {
		return []byte{}
	}

	return append([]byte(nil), src...)
}

// pubKeyBytes serializes an optional public key for durable storage.
func pubKeyBytes(pubKey *btcec.PublicKey) []byte {
	if pubKey == nil {
		return nil
	}

	return pubKey.SerializeCompressed()
}

// optionalPubKeyFromBytes decodes an optional compressed public key.
func optionalPubKeyFromBytes(raw []byte,
	name string) (*btcec.PublicKey, error) {

	if len(raw) == 0 {
		return nil, nil
	}

	pubKey, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	return pubKey, nil
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

// persistFundingResult stores the accepted funding session metadata without
// rolling back in-memory fields on persistence failure. The funding OOR is a
// daemon-side side effect, so retries must remember the accepted session and
// resolved vHTLC outpoint when they are known.
func (s *paySession) persistFundingResult(ctx context.Context, fundingSessionID,
	vhtlcOutpoint string, vhtlcAmount int64) error {

	s.fundingSessionID = fundingSessionID
	if vhtlcOutpoint != "" {
		s.vhtlcOutpoint = vhtlcOutpoint
		s.vhtlcAmount = vhtlcAmount
	}

	if err := s.persist(ctx); err != nil {
		return fmt.Errorf("persist pay funding result: %w", err)
	}

	return nil
}

// persistRefundSessionID stores the accepted refund session id without
// rolling back the in-memory id on persistence failure. Custom-input OOR
// submission is a daemon-side side effect, so retries must remember that an
// accepted refund already exists even if the swap DB write fails afterwards.
func (s *paySession) persistRefundSessionID(ctx context.Context,
	refundSessionID, reason string) error {

	s.refundSessionID = refundSessionID
	s.interventionReason = reason
	if err := s.persist(ctx); err != nil {
		return fmt.Errorf("persist pay refund session id: %w", err)
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
		SettlementType: string(s.cfg.SettlementType),
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
		VhtlcPkscript:         cloneBytesOrEmpty(s.vhtlcPkScript),
		VhtlcPolicyTemplate:   cloneBytesOrEmpty(s.vhtlcPolicyTemplate),
		VhtlcOutpoint:         s.vhtlcOutpoint,
		VhtlcAmount:           s.vhtlcAmount,
		FundingSessionID:      s.fundingSessionID,
		RefundReceivePubkey:   cloneBytesOrEmpty(s.refundReceivePubKey),
		RefundReceivePkscript: cloneBytesOrEmpty(s.refundReceiveScript),
		RefundSessionID:       s.refundSessionID,
		RefundRecoveryID:      s.refundRecoveryID,
		InterventionReason:    s.interventionReason,
		CreatedAtUnix:         s.createdAt.Unix(),
		UpdatedAtUnix:         now,
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

	var swapServerKey *btcec.PublicKey
	if len(row.SwapServerPubkey) != 0 {
		swapServerKey, err = btcec.ParsePubKey(row.SwapServerPubkey)
		if err != nil {
			return nil, fmt.Errorf("parse receive swap-server "+
				"pubkey: %w", err)
		}
	}

	paymentHash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return nil, err
	}

	preimage, err := preimageFromBytes(row.Preimage)
	if err != nil {
		return nil, err
	}

	var (
		policy      *arkscript.VHTLCPolicy
		paymentAddr [32]byte
	)
	if len(row.PaymentAddr) == len(paymentAddr) {
		copy(paymentAddr[:], row.PaymentAddr)
	}
	if swapServerKey != nil {
		policy, err = arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
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
			return nil, fmt.Errorf("rebuild receive vHTLC "+
				"policy: %w", err)
		}
	}

	return &ReceiveSession{
		Invoice:     row.Invoice,
		Preimage:    preimage,
		PaymentHash: paymentHash,
		client:      c,
		amountSat:   btcutil.Amount(row.AmountSat),
		payerFeeMsat: uint64(
			row.PayerFeeMsat,
		),
		state:    state,
		deadline: time.Unix(row.DeadlineUnix, 0),
		vhtlcConfig: restoreVHTLCConfig(
			row.RefundLocktime, row.UnilateralClaimDelay,
			row.UnilateralRefundDelay,
			row.UnilateralRefundWithoutReceiverDelay,
			row.SwapServerPubkey,
		),
		vhtlcPolicy: policy,
		vhtlcPolicyTemplate: append(
			[]byte(nil), row.VhtlcPolicyTemplate...,
		),
		vhtlcPkScript: append([]byte(nil), row.VhtlcPkscript...),
		vhtlcOutpoint: row.VhtlcOutpoint,
		vhtlcAmount:   row.VhtlcAmount,
		requestedAmountSat: uint64(
			row.RequestedAmountSat,
		),
		availableCreditSat: uint64(row.AvailableCreditSat),
		attachedCreditSat:  uint64(row.AttachedCreditSat),
		expectedVHTLCSat:   receiveExpectedVHTLCSat(row),
		dustLimitSat:       uint64(row.DustLimitSat),
		pendingHTLCAckCursor: uint64(
			row.PendingHtlcAckCursor,
		),
		claimReceivePubKey: append(
			[]byte(nil), row.ClaimReceivePubkey...,
		),
		claimReceiveScript: append(
			[]byte(nil), row.ClaimReceivePkscript...,
		),
		claimSessionID:     row.ClaimSessionID,
		claimRecoveryID:    row.ClaimRecoveryID,
		interventionReason: row.InterventionReason,
		clientPubKey:       clientKey,
		operatorPubKey:     operatorKey,
		swapServerPubKey:   swapServerKey,
		settlementType:     SettlementType(row.SettlementType),
		paymentAddr:        paymentAddr,
		createdAt:          time.Unix(row.CreatedAtUnix, 0),
		updatedAt:          time.Unix(row.UpdatedAtUnix, 0),
	}, nil
}

func receiveExpectedVHTLCSat(row swapsqlc.ReceiveSwap) uint64 {
	requestedAmountSat := uint64(row.RequestedAmountSat)
	if requestedAmountSat == 0 {
		requestedAmountSat = uint64(row.AmountSat)
	}

	return requestedAmountSat + uint64(row.AttachedCreditSat)
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

	settlementType := SettlementType(row.SettlementType)
	var policy *arkscript.VHTLCPolicy
	if settlementType != SettlementTypeCredit {
		policy, err = arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
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
			return nil, fmt.Errorf("rebuild pay vHTLC policy: %w",
				err)
		}
	}

	cfg := &InSwapConfig{
		PaymentHash:    paymentHash,
		AmountSat:      row.AmountSat,
		FeeSat:         uint64(row.FeeSat),
		ServerPubkey:   serverKey,
		SettlementType: settlementType,
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
		refundRecoveryID:   row.RefundRecoveryID,
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
		return lntypes.Hash{}, fmt.Errorf("payment hash must be "+
			"%d bytes", lntypes.HashSize)
	}

	var hash lntypes.Hash
	copy(hash[:], raw)

	return hash, nil
}

// preimageFromBytes decodes one persisted preimage.
func preimageFromBytes(raw []byte) (lntypes.Preimage, error) {
	if len(raw) != lntypes.PreimageSize {
		return lntypes.Preimage{}, fmt.Errorf("preimage must be "+
			"%d bytes", lntypes.PreimageSize)
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

	case ReceiveStateHTLCEventAccepted.String():
		return ReceiveStateHTLCEventAccepted, nil

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
		return ReceiveStateFailed, fmt.Errorf("unknown receive "+
			"state %q", state)
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
		return PayStateFailed, fmt.Errorf("unknown pay state %q", state)
	}
}
