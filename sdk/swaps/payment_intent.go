package swaps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	swapsqlc "github.com/lightninglabs/darepo-client/sdk/swaps/sqlc"
	"github.com/lightningnetwork/lnd/lntypes"
)

// PaymentIntentState identifies the durable wallet-level orchestration state
// that runs before the pay-swap FSM exists.
type PaymentIntentState string

const (
	PaymentIntentAccepted           PaymentIntentState = "Accepted"
	PaymentIntentCreditTopUpCreated PaymentIntentState = "CreditTopUp" +
		"Created"
	PaymentIntentCreditTopUpSent PaymentIntentState = "CreditTopUpSent"
	PaymentIntentCreditAvailable PaymentIntentState = "CreditAvailable"
	PaymentIntentPayStarted      PaymentIntentState = "PayStarted"
	PaymentIntentExpired         PaymentIntentState = "Expired"
	PaymentIntentFailed          PaymentIntentState = "Failed"
)

// IsTerminal returns true once no payment-intent worker should resume.
func (s PaymentIntentState) IsTerminal() bool {
	return s == PaymentIntentPayStarted ||
		s == PaymentIntentExpired ||
		s == PaymentIntentFailed
}

// PaymentIntentSummary is the restart-resume view for one durable payment
// orchestration intent.
type PaymentIntentSummary struct {
	PaymentHash lntypes.Hash
	Invoice     string
	State       PaymentIntentState
	Pending     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LastError   string
}

type paymentIntent struct {
	client *SwapClient

	paymentHash          lntypes.Hash
	invoice              string
	maxFeeSat            uint64
	maxCreditSat         uint64
	maxCreditTopUpSat    uint64
	state                PaymentIntentState
	creditIdempotencyKey string
	creditOperationID    string
	creditTopUpSat       uint64
	creditDestinationKey []byte
	creditOORSessionID   string
	payStartedHash       *lntypes.Hash
	lastError            string
	createdAt            time.Time
	updatedAt            time.Time
}

// StartPayViaLightningWithCreditTopUp starts or resumes a durable wallet-level
// payment orchestration flow, funding credits first when the accepted quote
// requires a bounded Ark top-up.
func (c *SwapClient) StartPayViaLightningWithCreditTopUp(ctx context.Context,
	invoice string, maxFeeSat uint64, maxCreditSat uint64,
	maxCreditTopUpSat uint64) (*PaySession, error) {

	paymentHash, err := c.paymentHashForInvoice(invoice)
	if err != nil {
		return nil, err
	}

	intent, err := c.getPaymentIntent(ctx, paymentHash)
	switch {
	case err == nil && !intent.state.IsTerminal():
		return intent.run(ctx)

	case err == nil && intent.state == PaymentIntentPayStarted:
		return c.ResumePayViaLightning(ctx, paymentHash)

	case err == nil:
		return nil, fmt.Errorf("payment intent %x is %s: %s",
			paymentHash[:], intent.state, intent.lastError)

	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	if maxCreditTopUpSat == 0 {
		return c.StartPayViaLightningWithCredits(
			ctx, invoice, maxFeeSat, maxCreditSat,
		)
	}
	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf(
			"swap store is required for credit top-up " +
				"payment intent",
		)
	}

	intent = &paymentIntent{
		client:               c,
		paymentHash:          paymentHash,
		invoice:              invoice,
		maxFeeSat:            maxFeeSat,
		maxCreditSat:         maxCreditSat,
		maxCreditTopUpSat:    maxCreditTopUpSat,
		state:                PaymentIntentAccepted,
		creditIdempotencyKey: creditTopUpID(paymentHash),
		creditDestinationKey: nil,
		creditOORSessionID:   "",
		creditOperationID:    "",
		creditTopUpSat:       0,
		payStartedHash:       nil,
		lastError:            "",
		createdAt:            c.currentTime(),
		updatedAt:            c.currentTime(),
	}
	if err := intent.persist(ctx); err != nil {
		return nil, err
	}

	return intent.run(ctx)
}

// ResumePaymentIntent reloads and advances one durable payment intent by
// payment hash.
func (c *SwapClient) ResumePaymentIntent(ctx context.Context,
	paymentHash lntypes.Hash) (*PaySession, error) {

	intent, err := c.getPaymentIntent(ctx, paymentHash)
	if err != nil {
		return nil, err
	}

	return intent.run(ctx)
}

// ListPendingPaymentIntents returns every non-terminal payment intent.
func (c *SwapClient) ListPendingPaymentIntents(ctx context.Context) (
	[]PaymentIntentSummary, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, fmt.Errorf("swap store is not configured")
	}

	rows, err := c.store.queries.ListPendingPaymentIntents(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pending payment intents: %w", err)
	}

	summaries := make([]PaymentIntentSummary, 0, len(rows))
	for i := range rows {
		summary, err := paymentIntentSummaryFromRow(rows[i])
		if err != nil {
			return nil, err
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

func (c *SwapClient) paymentHashForInvoice(invoice string) (lntypes.Hash,
	error) {

	decoded, err := decodePayInvoice(invoice, c.chainParams)
	if err != nil {
		return lntypes.Hash{}, err
	}
	if decoded.PaymentHash == nil {
		return lntypes.Hash{}, fmt.Errorf(
			"invoice payment hash is required",
		)
	}

	return lntypes.Hash(*decoded.PaymentHash), nil
}

func (c *SwapClient) getPaymentIntent(ctx context.Context,
	paymentHash lntypes.Hash) (*paymentIntent, error) {

	if c == nil || c.store == nil || c.store.queries == nil {
		return nil, sql.ErrNoRows
	}

	row, err := c.store.queries.GetPaymentIntent(ctx, paymentHash[:])
	if err != nil {
		return nil, err
	}

	return paymentIntentFromRow(c, row)
}

func (p *paymentIntent) run(ctx context.Context) (*PaySession, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("payment intent must be provided")
	}

	if session, ok, err := p.existingPaySession(ctx); ok || err != nil {
		if err != nil {
			return nil, err
		}

		if markErr := p.markPayStarted(ctx); markErr != nil {
			return nil, markErr
		}

		return session, nil
	}

	switch p.state {
	case PaymentIntentAccepted:
		if err := p.createTopUpIfNeeded(ctx); err != nil {
			return nil, err
		}

	case PaymentIntentCreditTopUpCreated,
		PaymentIntentCreditTopUpSent,
		PaymentIntentCreditAvailable:

	default:
		return nil, fmt.Errorf("payment intent %x is %s: %s",
			p.paymentHash[:], p.state, p.lastError)
	}

	if p.creditOperationID != "" &&
		p.state != PaymentIntentCreditAvailable {

		available, err := p.creditAvailable(ctx)
		if err != nil {
			return nil, p.fail(ctx, err)
		}
		if !available {
			if err := p.submitTopUp(ctx); err != nil {
				return nil, err
			}
			if err := p.waitCreditAvailable(ctx); err != nil {
				return nil, err
			}
		}
	}

	if err := p.setState(
		ctx, PaymentIntentCreditAvailable, "",
	); err != nil {
		return nil, err
	}

	session, err := p.client.StartPayViaLightningWithCredits(
		ctx, p.invoice, p.maxFeeSat, p.maxCreditSat,
	)
	if err != nil {
		return nil, err
	}

	p.payStartedHash = &p.paymentHash
	if err := p.markPayStarted(ctx); err != nil {
		return nil, err
	}

	return session, nil
}

func (p *paymentIntent) existingPaySession(ctx context.Context) (*PaySession,
	bool, error) {

	session, err := p.client.ResumePayViaLightning(ctx, p.paymentHash)
	if err == nil {
		return session, true, nil
	}
	if errors.Is(err, ErrPaySessionNotFound) {
		return nil, false, nil
	}

	return nil, false, err
}

func (p *paymentIntent) createTopUpIfNeeded(ctx context.Context) error {
	quote, err := p.client.QuotePayViaLightningWithCredits(
		ctx, p.invoice, p.maxFeeSat, p.maxCreditSat,
	)
	if err != nil {
		return err
	}

	creditQuote := quote.CreditQuote
	if creditQuote == nil || creditQuote.CreditShortfallSat == 0 {
		return p.setState(ctx, PaymentIntentCreditAvailable, "")
	}

	topupSat := creditQuote.CreditTopupSat
	if topupSat == 0 {
		return p.fail(ctx, fmt.Errorf("credit top-up amount missing"))
	}
	if topupSat > p.maxCreditTopUpSat {
		return p.fail(ctx, fmt.Errorf("credit top-up %d exceeds "+
			"accepted bound %d", topupSat, p.maxCreditTopUpSat))
	}
	if topupSat > math.MaxInt64 {
		return p.fail(
			ctx, fmt.Errorf("credit top-up exceeds int64 range"),
		)
	}

	op, err := p.client.CreateCredit(ctx, CreateCreditRequest{
		IdempotencyKey: p.creditIdempotencyKey,
		Source:         CreditFundingArkTopUp,
		AmountSat:      topupSat,
	})
	if err != nil {
		return err
	}
	if op == nil {
		return p.fail(
			ctx, fmt.Errorf("credit top-up operation missing"),
		)
	}

	p.creditOperationID = op.OperationID
	p.creditTopUpSat = topupSat
	p.creditDestinationKey = append(
		[]byte(nil), op.DestinationKey...,
	)
	if op.State == CreditStateCredited {
		return p.setState(ctx, PaymentIntentCreditAvailable, "")
	}

	return p.setState(ctx, PaymentIntentCreditTopUpCreated, "")
}

func (p *paymentIntent) submitTopUp(ctx context.Context) error {
	if p.creditTopUpSat == 0 {
		return p.fail(ctx, fmt.Errorf("credit top-up amount missing"))
	}
	if len(p.creditDestinationKey) == 0 {
		return p.fail(
			ctx, fmt.Errorf("credit top-up destination missing"),
		)
	}
	if p.creditTopUpSat > math.MaxInt64 {
		return p.fail(
			ctx, fmt.Errorf("credit top-up exceeds int64 range"),
		)
	}

	result, err := p.client.daemon.SendOORToPubKey(
		ctx, p.creditDestinationKey, int64(p.creditTopUpSat),
		p.creditIdempotencyKey,
	)
	if err != nil {
		return err
	}

	if result != nil {
		p.creditOORSessionID = result.SessionID
	}

	return p.setState(ctx, PaymentIntentCreditTopUpSent, "")
}

func (p *paymentIntent) waitCreditAvailable(ctx context.Context) error {
	for {
		available, err := p.creditAvailable(ctx)
		if err != nil {
			return p.fail(ctx, err)
		}
		if available {
			return p.setState(ctx, PaymentIntentCreditAvailable, "")
		}

		if err := waitForFixedPoll(
			ctx, p.client.waitPollInterval,
		); err != nil {
			return err
		}
	}
}

func (p *paymentIntent) creditAvailable(ctx context.Context) (bool, error) {
	snapshot, err := p.client.ListCredits(ctx, ^uint32(0))
	if err != nil {
		return false, err
	}
	if snapshot == nil {
		return false, nil
	}

	for _, op := range snapshot.Operations {
		if op.OperationID != p.creditOperationID {
			continue
		}

		switch op.State {
		case CreditStateCredited:
			return snapshot.AvailableSat >= p.maxCreditSat, nil

		case CreditStateFailed, CreditStateExpired, CreditStateReleased:
			return false, fmt.Errorf("credit top-up %s ended in %s",
				p.creditOperationID, op.State)

		case CreditStateCreated, CreditStateAwaitingPayment,
			CreditStateReserved, CreditStatePayingLightning,
			CreditStateDebited, CreditStateSendingOOR,
			CreditStateRedeemed:

			return false, nil
		}
	}

	return false, nil
}

func (p *paymentIntent) markPayStarted(ctx context.Context) error {
	p.payStartedHash = &p.paymentHash

	return p.setState(ctx, PaymentIntentPayStarted, "")
}

func (p *paymentIntent) fail(ctx context.Context, err error) error {
	if err == nil {
		err = fmt.Errorf("payment intent failed")
	}
	if setErr := p.setState(
		ctx, PaymentIntentFailed, err.Error(),
	); setErr != nil {
		return setErr
	}

	return err
}

func (p *paymentIntent) setState(ctx context.Context, state PaymentIntentState,
	lastError string) error {

	p.state = state
	p.lastError = lastError

	return p.persist(ctx)
}

func (p *paymentIntent) persist(ctx context.Context) error {
	if p == nil || p.client == nil || p.client.store == nil ||
		p.client.store.queries == nil {
		return fmt.Errorf("swap store is not configured")
	}

	now := p.client.currentTime().Unix()
	if p.createdAt.IsZero() {
		p.createdAt = time.Unix(now, 0)
	}

	var payStartedHash []byte
	if p.payStartedHash != nil {
		payStartedHash = append([]byte(nil), p.payStartedHash[:]...)
	}

	err := p.client.store.queries.UpsertPaymentIntent(
		ctx, swapsqlc.UpsertPaymentIntentParams{
			PaymentHash: append([]byte(nil), p.paymentHash[:]...),
			Invoice:     p.invoice,
			MaxFeeSat:   int64(p.maxFeeSat),
			MaxCreditSat: int64(
				p.maxCreditSat,
			),
			MaxCreditTopupSat: int64(p.maxCreditTopUpSat),
			State:             string(p.state),
			CreditIdempotencyKey: p.
				creditIdempotencyKey,
			CreditOperationID: p.creditOperationID,
			CreditTopupSat:    int64(p.creditTopUpSat),
			CreditDestinationPubkey: append(
				[]byte(nil), p.creditDestinationKey...,
			),
			CreditOorSessionID: p.creditOORSessionID,
			PayStartedHash:     payStartedHash,
			LastError:          p.lastError,
			CreatedAtUnix:      p.createdAt.Unix(),
			UpdatedAtUnix:      now,
		},
	)
	if err != nil {
		return fmt.Errorf("persist payment intent: %w", err)
	}

	p.updatedAt = time.Unix(now, 0)

	return nil
}

func paymentIntentFromRow(c *SwapClient,
	row swapsqlc.PaymentIntent) (*paymentIntent, error) {

	hash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return nil, err
	}

	var payStartedHash *lntypes.Hash
	if len(row.PayStartedHash) != 0 {
		hash, err := hashFromBytes(row.PayStartedHash)
		if err != nil {
			return nil, err
		}
		payStartedHash = &hash
	}

	return &paymentIntent{
		client:               c,
		paymentHash:          hash,
		invoice:              row.Invoice,
		maxFeeSat:            uint64(row.MaxFeeSat),
		maxCreditSat:         uint64(row.MaxCreditSat),
		maxCreditTopUpSat:    uint64(row.MaxCreditTopupSat),
		state:                PaymentIntentState(row.State),
		creditIdempotencyKey: row.CreditIdempotencyKey,
		creditOperationID:    row.CreditOperationID,
		creditTopUpSat:       uint64(row.CreditTopupSat),
		creditDestinationKey: cloneBytesOrEmpty(
			row.CreditDestinationPubkey,
		),
		creditOORSessionID: row.CreditOorSessionID,
		payStartedHash:     payStartedHash,
		lastError:          row.LastError,
		createdAt:          time.Unix(row.CreatedAtUnix, 0),
		updatedAt:          time.Unix(row.UpdatedAtUnix, 0),
	}, nil
}

func paymentIntentSummaryFromRow(row swapsqlc.PaymentIntent) (
	PaymentIntentSummary, error) {

	hash, err := hashFromBytes(row.PaymentHash)
	if err != nil {
		return PaymentIntentSummary{}, err
	}

	state := PaymentIntentState(row.State)

	return PaymentIntentSummary{
		PaymentHash: hash,
		Invoice:     row.Invoice,
		State:       state,
		Pending:     !state.IsTerminal(),
		CreatedAt:   time.Unix(row.CreatedAtUnix, 0),
		UpdatedAt:   time.Unix(row.UpdatedAtUnix, 0),
		LastError:   row.LastError,
	}, nil
}

func creditTopUpID(paymentHash lntypes.Hash) string {
	return fmt.Sprintf("credit-topup-pay-%x", paymentHash[:])
}
