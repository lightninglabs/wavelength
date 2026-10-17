//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
)

const sendIntentTTL = 5 * time.Minute

type preparedSendKind uint8

const (
	preparedSendInvoice preparedSendKind = iota + 1
	preparedSendOnchain
)

type preparedSendIntent struct {
	id        string
	kind      preparedSendKind
	expiresAt time.Time

	invoice           string
	onchainAddress    string
	amountSat         uint64
	note              string
	maxFeeSat         uint64
	maxCreditSat      uint64
	maxCreditTopupSat uint64
	creditEarmarkSat  uint64
	creditPreview     *wavewalletrpc.CreditPreview
	sweepAll          bool

	selectedOutpoints []string
	actualAmountSat   int64
}

type prepareSendPreview struct {
	rail                    wavewalletrpc.SendRail
	quoteStatus             wavewalletrpc.SendQuoteStatus
	amountSat               int64
	expectedFeeSat          int64
	feeKnown                bool
	expectedTotalOutflowSat int64
	totalOutflowKnown       bool
	destinationSummary      string
	invoiceDescription      string
	paymentHash             string
	warning                 string
	creditPreview           *wavewalletrpc.CreditPreview
}

type preparedSendStore struct {
	mu      sync.Mutex
	intents map[string]*preparedSendIntent
}

func newPreparedSendStore() *preparedSendStore {
	return &preparedSendStore{
		intents: make(map[string]*preparedSendIntent),
	}
}

// put stores a prepared intent and returns the generated id.
func (s *preparedSendStore) put(intent *preparedSendIntent) (string, error) {
	if s == nil || intent == nil {
		return "", ErrInvalidSendIntent
	}

	id, err := newSendIntentID()
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneExpiredLocked(now)

	intent.id = id
	intent.expiresAt = now.Add(sendIntentTTL)
	s.intents[id] = intent

	return id, nil
}

// consume returns and removes one live prepared intent. Send calls this before
// dispatching to the backend, so any dispatch failure intentionally burns the
// intent and requires the caller to prepare again.
func (s *preparedSendStore) consume(id string) (*preparedSendIntent, error) {
	if s == nil || id == "" {
		return nil, ErrInvalidSendIntent
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneExpiredLocked(now)

	intent, ok := s.intents[id]
	if !ok || now.After(intent.expiresAt) {
		delete(s.intents, id)

		return nil, ErrInvalidSendIntent
	}

	delete(s.intents, id)

	return intent, nil
}

// earmarkedCreditSat sums the credit balance reserved by live prepared sends.
// A complete quote earmarks its exact credit requirement, while a local-only
// fallback conservatively earmarks the caller's cap because the requirement is
// unknown. The auto-redeem sweep subtracts this balance so it never redeems
// credits a prepared-but-unsent send is about to spend. Each intent earmarks
// only for its TTL, after which the credits are free again.
func (s *preparedSendStore) earmarkedCreditSat() uint64 {
	if s == nil {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	s.pruneExpiredLocked(now)

	var total uint64
	for _, intent := range s.intents {
		if now.After(intent.expiresAt) || intent.creditEarmarkSat == 0 {
			continue
		}
		if total > ^uint64(0)-intent.creditEarmarkSat {
			return ^uint64(0)
		}
		total += intent.creditEarmarkSat
	}

	return total
}

func (s *preparedSendStore) pruneExpiredLocked(now time.Time) {
	for id, intent := range s.intents {
		if now.After(intent.expiresAt) {
			delete(s.intents, id)
		}
	}
}

func newSendIntentID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate send intent id: %w", err)
	}

	return hex.EncodeToString(buf[:]), nil
}

func prepareResponseFromIntent(intent *preparedSendIntent,
	preview prepareSendPreview) *wavewalletrpc.PrepareSendResponse {

	return &wavewalletrpc.PrepareSendResponse{
		SendIntentId:            intent.id,
		AmountSat:               preview.amountSat,
		ExpectedFeeSat:          preview.expectedFeeSat,
		FeeKnown:                preview.feeKnown,
		ExpectedTotalOutflowSat: preview.expectedTotalOutflowSat,
		TotalOutflowKnown:       preview.totalOutflowKnown,
		Rail:                    preview.rail,
		QuoteStatus:             preview.quoteStatus,
		DestinationSummary:      preview.destinationSummary,
		InvoiceDescription:      preview.invoiceDescription,
		PaymentHash:             preview.paymentHash,
		ExpiresAtUnix:           intent.expiresAt.Unix(),
		SelectedOutpoints: append(
			[]string(nil), intent.selectedOutpoints...,
		),
		Warning:       preview.warning,
		CreditPreview: preview.creditPreview,
	}
}
