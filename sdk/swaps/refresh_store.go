package swaps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	swapsqlc "github.com/lightninglabs/wavelength/sdk/swaps/sqlc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// ResumeRefreshSwap restores one composite refresh session from its durable
// payment-hash identity.
func (c *SwapClient) ResumeRefreshSwap(ctx context.Context,
	paymentHash lntypes.Hash) (*RefreshSession, error) {

	session, err := c.loadRefreshSwap(ctx, paymentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("refresh session not found")
	}

	return session, err
}

// loadRefreshSwap preserves sql.ErrNoRows for StartRefreshSwap's idempotent
// create-or-resume decision.
func (c *SwapClient) loadRefreshSwap(ctx context.Context,
	paymentHash lntypes.Hash) (*RefreshSession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("refresh swap store is not configured")
	}
	row, err := c.store.queries.GetRefreshSwap(ctx, paymentHash[:])
	if err != nil {
		return nil, err
	}

	return refreshSessionFromRow(c, row)
}

// ListRefreshSessions returns every durable refresh session in creation order.
func (c *SwapClient) ListRefreshSessions(ctx context.Context) (
	[]*RefreshSession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("refresh swap store is not configured")
	}
	rows, err := c.store.queries.ListRefreshSwaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list refresh sessions: %w", err)
	}

	return refreshSessionsFromRows(c, rows)
}

// ListPendingRefreshSessions returns every non-terminal refresh session in
// creation order for background resumption.
func (c *SwapClient) ListPendingRefreshSessions(ctx context.Context) (
	[]*RefreshSession, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("refresh swap store is not configured")
	}
	rows, err := c.store.queries.ListPendingRefreshSwaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending refresh sessions: %w", err)
	}

	return refreshSessionsFromRows(c, rows)
}

// refreshSessionsFromRows converts durable rows without returning a partial
// list when any row is corrupt.
func refreshSessionsFromRows(c *SwapClient,
	rows []swapsqlc.RefreshSwap) ([]*RefreshSession, error) {

	sessions := make([]*RefreshSession, 0, len(rows))
	for i := range rows {
		session, err := refreshSessionFromRow(c, rows[i])
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}

	return sessions, nil
}

// mutateAndPersist applies one refresh mutation atomically with its snapshot
// write from the caller's point of view.
func (s *RefreshSession) mutateAndPersist(ctx context.Context,
	mutate func() error) error {

	if s == nil {
		return fmt.Errorf("refresh session must be provided")
	}
	snapshot := *s
	if err := mutate(); err != nil {
		*s = snapshot

		return err
	}
	if err := s.persist(ctx); err != nil {
		*s = snapshot

		return err
	}

	return nil
}

// ensureCurrentSnapshot rejects a separately resumed object before it can
// perform another external action from an obsolete durable state.
func (s *RefreshSession) ensureCurrentSnapshot(ctx context.Context) error {
	if s == nil || s.client == nil || s.client.store == nil ||
		s.client.store.queries == nil {
		return fmt.Errorf("refresh swap store is not configured")
	}
	row, err := s.client.store.queries.GetRefreshSwap(
		ctx, s.paymentHash[:],
	)
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("load current refresh session: %w", err),
		)
	}
	if row.StateVersion <= 0 || uint64(row.StateVersion) != s.stateVersion {
		return newRetryableActionError(ErrRefreshSessionStale)
	}

	return nil
}

// persist stores a full composite snapshot. The Created row intentionally has
// no server key or vHTLC terms yet, but already protects the preimage, source,
// age cap, and both recovery destinations.
func (s *RefreshSession) persist(ctx context.Context) error {
	if s == nil || s.client == nil || s.client.store == nil ||
		s.client.store.queries == nil {
		return fmt.Errorf("refresh swap store is not configured")
	}
	if s.paymentHash == (lntypes.Hash{}) {
		return fmt.Errorf("refresh payment hash is required")
	}
	if s.amountSat <= 0 || s.sourceOutpoint == "" {
		return fmt.Errorf("refresh immutable terms are incomplete")
	}
	if s.clientPubKey == nil || s.operatorPubKey == nil {
		return fmt.Errorf("refresh local participant keys are " +
			"incomplete")
	}
	if len(s.refundReceivePubKey) == 0 ||
		len(s.refundReceiveScript) == 0 ||
		len(s.claimReceivePubKey) == 0 ||
		len(s.claimReceiveScript) == 0 {
		return fmt.Errorf("refresh recovery destinations are " +
			"incomplete")
	}
	if s.pendingAckCursor > math.MaxInt64 {
		return fmt.Errorf("refresh ACK cursor overflows int64")
	}
	if s.stateVersion >= math.MaxInt64 {
		return fmt.Errorf("refresh state version overflows int64")
	}
	nextVersion := s.stateVersion + 1

	var (
		expiryUnix     int64
		settlementType string
		inputConfig    VHTLCConfig
	)
	if s.cfg != nil {
		expiryUnix = s.cfg.Expiry.Unix()
		settlementType = string(s.cfg.SettlementType)
		inputConfig = s.cfg.VHTLCConfig
	}

	now := s.client.currentTime().Unix()
	if err := s.client.claimPaymentHashOwner(
		ctx, s.paymentHash, SwapDirectionRefresh,
	); err != nil {

		if errors.Is(err, ErrSwapPaymentHashOwned) {
			return err
		}

		return newRetryableActionError(err)
	}
	params := swapsqlc.UpsertRefreshSwapParams{
		PaymentHash:      append([]byte(nil), s.paymentHash[:]...),
		Preimage:         append([]byte(nil), s.preimage[:]...),
		AmountSat:        int64(s.amountSat),
		SourceOutpoint:   s.sourceOutpoint,
		MaxVtxoAgeBlocks: int64(s.maxVTXOAgeBlocks),
		State:            s.state.String(),
		ExpiryUnix:       expiryUnix,
		ClientPubkey: cloneBytesOrEmpty(
			pubKeyBytes(s.clientPubKey),
		),
		OperatorPubkey: cloneBytesOrEmpty(
			pubKeyBytes(s.operatorPubKey),
		),
		ServerPubkey:   cloneBytesOrEmpty(pubKeyBytes(s.serverPubKey)),
		SettlementType: settlementType,
		InputRefundLocktime: int64(
			inputConfig.RefundLocktime,
		),
		InputUnilateralClaimDelay: int64(
			inputConfig.UnilateralClaimDelay,
		),
		InputUnilateralRefundDelay: int64(
			inputConfig.UnilateralRefundDelay,
		),
		InputUnilateralRefundWithoutReceiverDelay: int64(
			inputConfig.UnilateralRefundWithoutReceiverDelay,
		),
		InputVhtlcPkscript: cloneBytesOrEmpty(s.inputPkScript),
		InputVhtlcPolicyTemplate: cloneBytesOrEmpty(
			s.inputPolicyTemplate,
		),
		InputVhtlcOutpoint: s.inputOutpoint,
		InputVhtlcAmount:   s.inputAmount,
		FundingSessionID:   s.fundingSessionID,
		InputRefundReceivePubkey: cloneBytesOrEmpty(
			s.refundReceivePubKey,
		),
		InputRefundReceivePkscript: cloneBytesOrEmpty(
			s.refundReceiveScript,
		),
		InputRefundSessionID:  s.refundSessionID,
		InputRefundRecoveryID: s.refundRecoveryID,
		OutputSenderPubkey: cloneBytesOrEmpty(
			pubKeyBytes(s.outputSenderPubKey),
		),
		OutputRefundLocktime: int64(
			s.outputConfig.RefundLocktime,
		),
		OutputUnilateralClaimDelay: int64(
			s.outputConfig.UnilateralClaimDelay,
		),
		OutputUnilateralRefundDelay: int64(
			s.outputConfig.UnilateralRefundDelay,
		),
		OutputUnilateralRefundWithoutReceiverDelay: int64(
			s.outputConfig.UnilateralRefundWithoutReceiverDelay,
		),
		OutputVhtlcPkscript: cloneBytesOrEmpty(s.outputPkScript),
		OutputVhtlcPolicyTemplate: cloneBytesOrEmpty(
			s.outputPolicyTemplate,
		),
		OutputVhtlcOutpoint:  s.outputOutpoint,
		OutputVhtlcAmount:    s.outputAmount,
		OutputObservedHeight: int64(s.outputObservedHeight),
		OutputCreatedHeight:  int64(s.outputCreatedHeight),
		OutputBatchExpiry:    int64(s.outputBatchExpiry),
		PendingHtlcAckCursor: int64(s.pendingAckCursor),
		OutputClaimReceivePubkey: cloneBytesOrEmpty(
			s.claimReceivePubKey,
		),
		OutputClaimReceivePkscript: cloneBytesOrEmpty(
			s.claimReceiveScript,
		),
		OutputClaimSessionID:  s.claimSessionID,
		OutputClaimRecoveryID: s.claimRecoveryID,
		InputClaimTxid:        s.inputClaimTxID,
		InterventionReason:    s.interventionReason,
		CreatedAtUnix:         s.createdAt.Unix(),
		UpdatedAtUnix:         now,
		StateVersion:          int64(nextVersion),
	}
	if s.createdAt.IsZero() {
		params.CreatedAtUnix = now
		s.createdAt = time.Unix(now, 0)
	}
	persistedVersion, err := s.client.store.queries.UpsertRefreshSwap(
		ctx, params,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return newRetryableActionError(ErrRefreshSessionStale)
	}
	if err != nil {
		return newRetryableActionError(
			fmt.Errorf("persist refresh session: %w", err),
		)
	}
	if persistedVersion != int64(nextVersion) {
		return newRetryableActionError(
			fmt.Errorf("persist refresh session version: got "+
				"%d, want %d", persistedVersion, nextVersion),
		)
	}
	s.stateVersion = nextVersion
	s.updatedAt = time.Unix(now, 0)

	return nil
}

// refreshSessionFromRow reconstructs both vHTLC policies and rejects durable
// term drift before any resumed side effect is attempted.
func refreshSessionFromRow(c *SwapClient,
	row swapsqlc.RefreshSwap) (*RefreshSession, error) {

	state, err := parseRefreshState(row.State)
	if err != nil {
		return nil, err
	}
	paymentHash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return nil, err
	}
	preimage, err := preimageFromBytes(row.Preimage)
	if err != nil {
		return nil, err
	}
	if preimage.Hash() != paymentHash {
		return nil, fmt.Errorf("refresh preimage does not match " +
			"payment hash")
	}
	if row.AmountSat <= 0 || row.SourceOutpoint == "" {
		return nil, fmt.Errorf("refresh immutable terms are incomplete")
	}
	if row.StateVersion <= 0 {
		return nil, errors.New("stored refresh state version is " +
			"invalid")
	}
	maxAge, err := uint32FromStored(
		row.MaxVtxoAgeBlocks, "max VTXO age",
	)
	if err != nil {
		return nil, err
	}
	if maxAge == 0 {
		return nil, errors.New("stored refresh maximum VTXO age must " +
			"be positive")
	}
	clientKey, err := optionalPubKeyFromBytes(
		row.ClientPubkey, "refresh client pubkey",
	)
	if err != nil {
		return nil, err
	}
	if clientKey == nil {
		return nil, fmt.Errorf("refresh client pubkey is missing")
	}
	operatorKey, err := optionalPubKeyFromBytes(
		row.OperatorPubkey, "refresh operator pubkey",
	)
	if err != nil {
		return nil, err
	}
	if operatorKey == nil {
		return nil, fmt.Errorf("refresh operator pubkey is missing")
	}
	serverKey, err := optionalPubKeyFromBytes(
		row.ServerPubkey, "refresh server pubkey",
	)
	if err != nil {
		return nil, err
	}
	outputSender, err := optionalPubKeyFromBytes(
		row.OutputSenderPubkey, "refresh output sender pubkey",
	)
	if err != nil {
		return nil, err
	}

	inputConfig, err := storedVHTLCConfig(
		row.InputRefundLocktime, row.InputUnilateralClaimDelay,
		row.InputUnilateralRefundDelay,
		row.InputUnilateralRefundWithoutReceiverDelay, row.ServerPubkey,
	)
	if err != nil {
		return nil, fmt.Errorf("restore refresh input config: %w", err)
	}
	outputConfig, err := storedVHTLCConfig(
		row.OutputRefundLocktime, row.OutputUnilateralClaimDelay,
		row.OutputUnilateralRefundDelay,
		row.OutputUnilateralRefundWithoutReceiverDelay,
		row.OutputSenderPubkey,
	)
	if err != nil {
		return nil, fmt.Errorf("restore refresh output config: %w", err)
	}
	if err := validateStoredRefreshOutput(row, maxAge, state); err != nil {
		return nil, err
	}

	session := &RefreshSession{
		client:           c,
		preimage:         preimage,
		paymentHash:      paymentHash,
		amountSat:        btcutil.Amount(row.AmountSat),
		sourceOutpoint:   row.SourceOutpoint,
		maxVTXOAgeBlocks: maxAge,
		state:            state,
		stateVersion:     uint64(row.StateVersion),
		createdAt:        time.Unix(row.CreatedAtUnix, 0),
		updatedAt:        time.Unix(row.UpdatedAtUnix, 0),
		clientPubKey:     clientKey,
		operatorPubKey:   operatorKey,
		serverPubKey:     serverKey,
		inputPkScript:    cloneBytesOrEmpty(row.InputVhtlcPkscript),
		inputPolicyTemplate: cloneBytesOrEmpty(
			row.InputVhtlcPolicyTemplate,
		),
		inputOutpoint:    row.InputVhtlcOutpoint,
		inputAmount:      row.InputVhtlcAmount,
		fundingSessionID: row.FundingSessionID,
		refundReceivePubKey: cloneBytesOrEmpty(
			row.InputRefundReceivePubkey,
		),
		refundReceiveScript: cloneBytesOrEmpty(
			row.InputRefundReceivePkscript,
		),
		refundSessionID:    row.InputRefundSessionID,
		refundRecoveryID:   row.InputRefundRecoveryID,
		outputSenderPubKey: outputSender,
		outputConfig:       outputConfig,
		outputPkScript: cloneBytesOrEmpty(
			row.OutputVhtlcPkscript,
		),
		outputPolicyTemplate: cloneBytesOrEmpty(
			row.OutputVhtlcPolicyTemplate,
		),
		outputOutpoint:       row.OutputVhtlcOutpoint,
		outputAmount:         row.OutputVhtlcAmount,
		outputObservedHeight: uint32(row.OutputObservedHeight),
		outputCreatedHeight:  int32(row.OutputCreatedHeight),
		outputBatchExpiry:    int32(row.OutputBatchExpiry),
		pendingAckCursor:     uint64(row.PendingHtlcAckCursor),
		claimReceivePubKey: cloneBytesOrEmpty(
			row.OutputClaimReceivePubkey,
		),
		claimReceiveScript: cloneBytesOrEmpty(
			row.OutputClaimReceivePkscript,
		),
		claimSessionID:     row.OutputClaimSessionID,
		claimRecoveryID:    row.OutputClaimRecoveryID,
		inputClaimTxID:     row.InputClaimTxid,
		interventionReason: row.InterventionReason,
	}

	if err := restoreRefreshPolicies(
		session, row, inputConfig, outputConfig,
	); err != nil {
		return nil, err
	}

	return session, nil
}

// restoreRefreshPolicies rederives both signed policy scripts so durable
// corruption cannot redirect a resumed spend.
func restoreRefreshPolicies(session *RefreshSession, row swapsqlc.RefreshSwap,
	inputConfig, outputConfig VHTLCConfig) error {

	if session.serverPubKey != nil {
		session.cfg = &RefreshSwapConfig{
			PaymentHash:    session.paymentHash,
			AmountSat:      row.AmountSat,
			ServerPubkey:   session.serverPubKey,
			VHTLCConfig:    inputConfig,
			Expiry:         time.Unix(row.ExpiryUnix, 0),
			SettlementType: SettlementType(row.SettlementType),
		}
		if err := session.validateServerConfig(
			session.cfg,
		); err != nil {
			return fmt.Errorf("restore refresh server terms: %w",
				err)
		}

		inputPolicy, pkScript, template, err := buildVHTLCPolicy(
			session.clientPubKey, session.serverPubKey,
			session.operatorPubKey, session.paymentHash,
			inputConfig,
		)
		if err != nil {
			return err
		}
		if !bytesEqual(pkScript, row.InputVhtlcPkscript) ||
			!bytesEqual(template, row.InputVhtlcPolicyTemplate) {
			return fmt.Errorf("stored refresh input policy " +
				"mismatch")
		}
		session.inputPolicy = inputPolicy
	}

	if session.outputSenderPubKey != nil {
		outputPolicy, pkScript, template, err := buildVHTLCPolicy(
			session.outputSenderPubKey, session.clientPubKey,
			session.operatorPubKey, session.paymentHash,
			outputConfig,
		)
		if err != nil {
			return err
		}
		if !bytesEqual(pkScript, row.OutputVhtlcPkscript) ||
			!bytesEqual(template, row.OutputVhtlcPolicyTemplate) {
			return fmt.Errorf("stored refresh output policy " +
				"mismatch")
		}
		session.outputPolicy = outputPolicy
	}

	preFundingFailure := session.state == RefreshStateFailed &&
		session.fundingSessionID == "" && session.inputOutpoint == ""
	if session.state != RefreshStateCreated && session.cfg == nil &&
		!preFundingFailure {
		return fmt.Errorf("refresh server terms are missing in "+
			"state %s", session.state)
	}

	return nil
}

// validateStoredRefreshOutput checks every persisted age input before the
// values are narrowed to the daemon-facing integer representation.
func validateStoredRefreshOutput(row swapsqlc.RefreshSwap, maxAge uint32,
	state RefreshState) error {

	if row.PendingHtlcAckCursor < 0 || row.OutputObservedHeight < 0 ||
		row.OutputCreatedHeight < 0 ||
		row.OutputCreatedHeight > math.MaxInt32 ||
		row.OutputBatchExpiry < 0 ||
		row.OutputBatchExpiry > math.MaxInt32 {
		return fmt.Errorf("stored refresh output metadata is invalid")
	}
	if row.OutputObservedHeight > math.MaxUint32 {
		return fmt.Errorf("stored refresh observed height overflows " +
			"uint32")
	}
	requiresAgeEvidence := state == RefreshStateOutputVHTLCFunded ||
		state == RefreshStateClaimInitiated ||
		state == RefreshStateOutputClaimed ||
		state == RefreshStateCompleted
	if requiresAgeEvidence && row.OutputObservedHeight == 0 {
		return fmt.Errorf("stored refresh output age evidence is " +
			"missing")
	}
	if row.OutputObservedHeight == 0 {
		return nil
	}

	err := validateRefreshOutputAge(&VTXOInfo{
		CreatedHeight: int32(row.OutputCreatedHeight),
		BatchExpiry:   int32(row.OutputBatchExpiry),
	}, uint32(row.OutputObservedHeight), maxAge)
	if err != nil {
		return fmt.Errorf("stored refresh output age: %w", err)
	}

	return nil
}

// storedVHTLCConfig converts the signed SQL representation while allowing an
// all-zero config for states that have not accepted the corresponding leg.
func storedVHTLCConfig(refund, claimDelay, refundDelay,
	refundWithoutReceiver int64, serverPubkey []byte) (VHTLCConfig, error) {

	values := []struct {
		name  string
		value int64
	}{
		{
			name:  "refund locktime",
			value: refund,
		},
		{
			name:  "claim delay",
			value: claimDelay,
		},
		{
			name:  "refund delay",
			value: refundDelay,
		},
		{
			name:  "refund-without-receiver delay",
			value: refundWithoutReceiver,
		},
	}
	converted := make([]uint32, len(values))
	for i := range values {
		value, err := uint32FromStored(values[i].value, values[i].name)
		if err != nil {
			return VHTLCConfig{}, err
		}
		converted[i] = value
	}

	return VHTLCConfig{
		RefundLocktime:                       converted[0],
		UnilateralClaimDelay:                 converted[1],
		UnilateralRefundDelay:                converted[2],
		UnilateralRefundWithoutReceiverDelay: converted[3],
		SwapServerPubkey: cloneBytesOrEmpty(
			serverPubkey,
		),
	}, nil
}

// uint32FromStored rejects negative and overflowing durable block metadata.
func uint32FromStored(value int64, name string) (uint32, error) {
	if value < 0 || value > math.MaxUint32 {
		return 0, fmt.Errorf("stored refresh %s is invalid", name)
	}

	return uint32(value), nil
}

// parseRefreshState decodes one durable refresh state name.
func parseRefreshState(state string) (RefreshState, error) {
	for candidate := RefreshStateCreated; ; candidate++ {
		if state == candidate.String() {
			return candidate, nil
		}
		if candidate == RefreshStateFailed {
			break
		}
	}

	return RefreshStateFailed, fmt.Errorf("unknown refresh state %q", state)
}
