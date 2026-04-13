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
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// defaultInvoiceExpiry is the default expiry duration for
	// swap invoices when none is specified by the caller.
	defaultInvoiceExpiry = time.Hour

	// defaultCLTVExpiry is the default CLTV expiry delta for
	// invoices.
	defaultCLTVExpiry = 40
)

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
// with route hints pointing through the swap server. It uses
// lnd's invoicesrpc.AddInvoice machinery with a
// keychain.SingleKeyMessageSigner for signing — the same pattern
// as the original ark swap client.
type InvoiceGenerator struct {
	invoiceCfg  *invoicesrpc.AddInvoiceConfig
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

// NewInvoiceGenerator creates an InvoiceGenerator. The signer is
// used to sign invoices (typically obtained from the wallet's
// KeyRing via keychain.NewPrivKeyMessageSigner or from
// btcwallet's key ring). The bestHeight function returns the
// current best block height. The store persists created invoices.
func NewInvoiceGenerator(
	signer keychain.SingleKeyMessageSigner,
	bestHeight func() (uint32, error),
	store InvoiceStore,
	chainParams *chaincfg.Params) *InvoiceGenerator {

	nodeSigner := netann.NewNodeSigner(signer)

	return &InvoiceGenerator{
		invoiceCfg: genInvoiceCfg(
			nodeSigner, bestHeight, store, chainParams,
		),
		chainParams: chainParams,
	}
}

// genInvoiceCfg returns the minimal AddInvoice configuration needed for swap
// invoices that carry explicit route hints and do not depend on a real graph.
func genInvoiceCfg(nodeSigner *netann.NodeSigner,
	bestHeight func() (uint32, error), store InvoiceStore,
	chainParams *chaincfg.Params) *invoicesrpc.AddInvoiceConfig {

	return &invoicesrpc.AddInvoiceConfig{
		AddInvoice: store.AddInvoice,
		IsChannelActive: func(
			chanID lnwire.ChannelID) bool {

			return true
		},
		ChainParams:       chainParams,
		NodeSigner:        nodeSigner,
		DefaultCLTVExpiry: defaultCLTVExpiry,
		ChanDB:            nil,
		Graph:             &mockGraph{},
		GenInvoiceFeatures: func() *lnwire.FeatureVector {
			return lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadRequired,
					lnwire.PaymentAddrRequired,
				),
				lnwire.Features,
			)
		},
		GenAmpInvoiceFeatures: func() *lnwire.FeatureVector {
			return lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadRequired,
					lnwire.PaymentAddrRequired,
					lnwire.AMPRequired,
				),
				lnwire.Features,
			)
		},
		GetAlias: func(
			chanID lnwire.ChannelID,
		) (lnwire.ShortChannelID, error) {

			return lnwire.ShortChannelID{}, nil
		},
		BestHeight: bestHeight,
		QueryBlindedRoutes: func(
			amt lnwire.MilliSatoshi,
		) ([]*route.Route, error) {

			return nil, nil
		},
	}
}

// mockGraph satisfies the subset of graph lookups AddInvoice expects when
// normalizing caller-provided route hints for virtual channels.
type mockGraph struct{}

// IsPublicNode reports all nodes as public for route hint validation.
func (m *mockGraph) IsPublicNode(_ [33]byte) (bool, error) {
	return true, nil
}

// FetchChannelEdgesByID reports no backing graph edges for swap virtual
// channels.
func (m *mockGraph) FetchChannelEdgesByID(_ uint64) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	return nil, nil, nil, nil
}

// NewEphemeralInvoiceGenerator creates an ephemeral invoice creator backed by a
// private key and direct BOLT-11 encoding.
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
func (g *InvoiceGenerator) CreateInvoice(ctx context.Context,
	amountSat btcutil.Amount, memo string,
	routeHint *RouteHint, expiry time.Duration,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash,
	error) {

	if expiry == 0 {
		expiry = defaultInvoiceExpiry
	}

	// Parse the swap server's node pubkey from the route hint.
	nodePubkey, err := btcec.ParsePubKey(routeHint.NodeID)
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"parse route hint node ID: %w", err,
		)
	}

	hopHint := zpay32.HopHint{
		NodeID:    nodePubkey,
		ChannelID: routeHint.ChannelID,
		FeeBaseMSat: uint32(
			routeHint.FeeBaseMsat,
		),
		FeeProportionalMillionths: uint32(
			routeHint.FeePropPpm,
		),
		CLTVExpiryDelta: uint16(
			routeHint.CltvExpiryDelta,
		),
	}

	invoiceData := &invoicesrpc.AddInvoiceData{
		Memo:     memo,
		Preimage: preimage,
		Value: lnwire.NewMSatFromSatoshis(
			amountSat,
		),
		RouteHints: [][]zpay32.HopHint{
			{hopHint},
		},
		Expiry: int64(expiry.Seconds()),
	}

	paymentHash, invoice, err := invoicesrpc.AddInvoice(
		ctx, g.invoiceCfg, invoiceData,
	)
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"create invoice: %w", err,
		)
	}

	return invoice, *paymentHash, nil
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

	if routeHint == nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"route hint is required",
		)
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

	nodePubkey, err := btcec.ParsePubKey(routeHint.NodeID)
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"parse route hint node ID: %w", err,
		)
	}

	hopHint := zpay32.HopHint{
		NodeID:    nodePubkey,
		ChannelID: routeHint.ChannelID,
		FeeBaseMSat: uint32(
			routeHint.FeeBaseMsat,
		),
		FeeProportionalMillionths: uint32(
			routeHint.FeePropPpm,
		),
		CLTVExpiryDelta: uint16(
			routeHint.CltvExpiryDelta,
		),
	}

	msat := lnwire.NewMSatFromSatoshis(amountSat)
	invoice, err := zpay32.NewInvoice(
		g.chainParams, paymentHash, time.Now(),
		zpay32.Amount(msat),
		zpay32.Description(memo),
		zpay32.RouteHint([]zpay32.HopHint{hopHint}),
		zpay32.Expiry(expiry),
	)
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"create invoice: %w", err,
		)
	}

	paymentRequest, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(hash []byte) ([]byte, error) {
			return ecdsa.SignCompact(
				g.privKey, hash, true,
			), nil
		},
	})
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf(
			"encode invoice: %w", err,
		)
	}

	return &invoices.Invoice{
		Memo:           []byte(memo),
		PaymentRequest: []byte(paymentRequest),
		CreationDate:   time.Now(),
	}, paymentHash, nil
}
