package swaps

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// defaultInvoiceExpiry is the default expiry duration for
	// swap invoices when none is specified by the caller.
	defaultInvoiceExpiry = time.Hour
)

type compactInvoiceSigner func([]byte) ([]byte, error)

// InvoiceStore persists created invoices so they can be looked up
// when HTLCs arrive.
type InvoiceStore interface {
	// AddInvoice stores a new invoice keyed by payment hash.
	AddInvoice(
		ctx context.Context,
		invoice *invoices.Invoice,
		paymentHash lntypes.Hash,
	) (uint64, error)
}

// NewPreimage generates a cryptographically random 32-byte
// preimage suitable for use as a Lightning payment preimage.
func NewPreimage() (lntypes.Preimage, error) {
	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		return preimage, fmt.Errorf(
			"generate preimage: %w", err,
		)
	}

	return preimage, nil
}

// InvoiceGenerator creates properly signed Lightning invoices
// with route hints pointing through the swap server using direct
// BOLT-11 encoding and a SingleKeyMessageSigner.
type InvoiceGenerator struct {
	signer      keychain.SingleKeyMessageSigner
	chainParams *chaincfg.Params
}

// DirectInvoiceCreator creates signed BOLT-11 invoices directly from a private
// key without depending on a full lnd invoice registry.
type DirectInvoiceCreator struct {
	privKey     *btcec.PrivateKey
	chainParams *chaincfg.Params
}

// MemoryInvoiceStore keeps invoices in memory for ephemeral callers such as
// tests or simple SDK consumers that only need signed payment requests.
type MemoryInvoiceStore struct {
	mu       sync.Mutex
	nextID   uint64
	invoices map[lntypes.Hash]*invoices.Invoice
}

// NewMemoryInvoiceStore creates an empty in-memory invoice store.
func NewMemoryInvoiceStore() *MemoryInvoiceStore {
	return &MemoryInvoiceStore{
		invoices: make(map[lntypes.Hash]*invoices.Invoice),
	}
}

// AddInvoice stores one invoice keyed by payment hash and returns its add
// index.
func (s *MemoryInvoiceStore) AddInvoice(_ context.Context,
	invoice *invoices.Invoice, paymentHash lntypes.Hash) (uint64, error) {

	if s == nil {
		return 0, fmt.Errorf("invoice store must be provided")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	s.invoices[paymentHash] = invoice

	return s.nextID, nil
}

// NewInvoiceGenerator creates an InvoiceGenerator.
//
// The signer is used to sign invoices, typically through the wallet key ring.
// The bestHeight and store parameters are retained only for source
// compatibility with earlier swap SDK revisions and are ignored by this
// stateless invoice encoder.
func NewInvoiceGenerator(
	signer keychain.SingleKeyMessageSigner,
	bestHeight func() (uint32, error),
	store InvoiceStore,
	chainParams *chaincfg.Params) *InvoiceGenerator {

	_ = bestHeight
	_ = store

	return &InvoiceGenerator{
		signer:      signer,
		chainParams: chainParams,
	}
}

// NewEphemeralInvoiceGenerator creates an ephemeral invoice creator backed by a
// private key and direct BOLT-11 encoding.
//
// The bestHeight parameter is retained only for source compatibility with
// earlier swap SDK revisions and is ignored by this stateless invoice encoder.
func NewEphemeralInvoiceGenerator(privKey *btcec.PrivateKey,
	bestHeight func() (uint32, error),
	chainParams *chaincfg.Params) InvoiceCreator {

	_ = bestHeight

	return &DirectInvoiceCreator{
		privKey:     privKey,
		chainParams: chainParams,
	}
}

// CreateInvoice builds a signed BOLT-11 Lightning invoice with a
// route hint pointing through the swap server's virtual channel.
// When preimage is non-nil, the invoice is locked to that preimage
// so the caller can construct a matching vHTLC. Returns the
// invoice, its payment hash, and any error.
func (g *InvoiceGenerator) CreateInvoice(_ context.Context,
	amountSat btcutil.Amount, memo string,
	routeHint *RouteHint, expiry time.Duration,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash,
	error) {

	if g == nil || g.signer == nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"invoice signer is required",
		)
	}

	return buildSignedInvoice(
		amountSat, memo, routeHint, expiry, preimage,
		g.chainParams, func(hash []byte) ([]byte, error) {
			return g.signer.SignMessageCompact(hash, false)
		},
	)
}

// CreateInvoice creates one signed BOLT-11 invoice using direct zpay32
// encoding.
func (g *DirectInvoiceCreator) CreateInvoice(_ context.Context,
	amountSat btcutil.Amount, memo string, routeHint *RouteHint,
	expiry time.Duration, preimage *lntypes.Preimage) (*invoices.Invoice,
	lntypes.Hash, error) {

	if g == nil || g.privKey == nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"invoice creator private key is required",
		)
	}

	return buildSignedInvoice(
		amountSat, memo, routeHint, expiry, preimage,
		g.chainParams, func(hash []byte) ([]byte, error) {
			return ecdsa.SignCompact(g.privKey, hash, true), nil
		},
	)
}

func buildSignedInvoice(amountSat btcutil.Amount, memo string,
	routeHint *RouteHint, expiry time.Duration,
	preimage *lntypes.Preimage, chainParams *chaincfg.Params,
	sign compactInvoiceSigner) (*invoices.Invoice, lntypes.Hash, error) {

	if chainParams == nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"chain parameters are required",
		)
	}

	if sign == nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"invoice signer is required",
		)
	}

	if err := validateInvoiceAmount(amountSat); err != nil {
		return nil, lntypes.Hash{}, err
	}

	hopHint, err := zpayHopHint(routeHint)
	if err != nil {
		return nil, lntypes.Hash{}, err
	}

	if expiry == 0 {
		expiry = defaultInvoiceExpiry
	}

	invoicePreimage := preimage
	if invoicePreimage == nil {
		generatedPreimage, err := NewPreimage()
		if err != nil {
			return nil, lntypes.Hash{}, err
		}

		invoicePreimage = &generatedPreimage
	}

	paymentHash := invoicePreimage.Hash()
	var paymentAddr [32]byte
	if _, err := rand.Read(paymentAddr[:]); err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"generate payment address: %w", err,
		)
	}

	createdAt := time.Now()
	msat := lnwire.NewMSatFromSatoshis(amountSat)
	features := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadRequired,
			lnwire.PaymentAddrRequired,
		),
		lnwire.Features,
	)
	invoice, err := zpay32.NewInvoice(
		chainParams, paymentHash, createdAt,
		zpay32.Amount(msat),
		zpay32.Description(memo),
		zpay32.RouteHint([]zpay32.HopHint{hopHint}),
		zpay32.Expiry(expiry),
		zpay32.PaymentAddr(paymentAddr),
		zpay32.Features(features),
	)
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"create invoice: %w", err,
		)
	}

	paymentRequest, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: sign,
	})
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"encode invoice: %w", err,
		)
	}

	storedPreimage := *invoicePreimage

	return &invoices.Invoice{
		Memo:           []byte(memo),
		PaymentRequest: []byte(paymentRequest),
		CreationDate:   createdAt,
		Terms: invoices.ContractTerm{
			PaymentPreimage: &storedPreimage,
			Value:           msat,
			PaymentAddr:     paymentAddr,
			Expiry:          expiry,
			Features:        features.Clone(),
		},
	}, paymentHash, nil
}

func validateInvoiceAmount(amountSat btcutil.Amount) error {
	return validateSatoshiAmount(amountSat, "invoice amount")
}

func validateSatoshiAmount(amountSat btcutil.Amount, label string) error {
	switch {
	case amountSat <= 0:
		return fmt.Errorf("%s must be positive", label)

	case amountSat > btcutil.MaxSatoshi:
		return fmt.Errorf(
			"%s %d exceeds max bitcoin supply %d",
			label, amountSat, btcutil.Amount(btcutil.MaxSatoshi),
		)

	default:
		return nil
	}
}

func zpayHopHint(routeHint *RouteHint) (zpay32.HopHint, error) {
	if err := validateRouteHint(routeHint); err != nil {
		return zpay32.HopHint{}, err
	}

	nodePubkey, err := btcec.ParsePubKey(routeHint.NodeID)
	if err != nil {
		return zpay32.HopHint{}, fmt.Errorf(
			"parse route hint node ID: %w", err,
		)
	}

	return zpay32.HopHint{
		NodeID:                    nodePubkey,
		ChannelID:                 routeHint.ChannelID,
		FeeBaseMSat:               uint32(routeHint.FeeBaseMsat),
		FeeProportionalMillionths: uint32(routeHint.FeePropPpm),
		CLTVExpiryDelta:           uint16(routeHint.CltvExpiryDelta),
	}, nil
}

func validateRouteHint(routeHint *RouteHint) error {
	if routeHint == nil {
		return fmt.Errorf("route hint is required")
	}

	if len(routeHint.NodeID) == 0 {
		return fmt.Errorf("route hint node ID is required")
	}

	if routeHint.FeeBaseMsat > uint64(^uint32(0)) {
		return fmt.Errorf(
			"route hint fee base msat %d exceeds uint32",
			routeHint.FeeBaseMsat,
		)
	}

	if routeHint.FeePropPpm > uint64(^uint32(0)) {
		return fmt.Errorf(
			"route hint fee proportional ppm %d exceeds uint32",
			routeHint.FeePropPpm,
		)
	}

	if routeHint.CltvExpiryDelta > uint32(^uint16(0)) {
		return fmt.Errorf(
			"route hint CLTV expiry delta %d exceeds uint16",
			routeHint.CltvExpiryDelta,
		)
	}

	return nil
}
