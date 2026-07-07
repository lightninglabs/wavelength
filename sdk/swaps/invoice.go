package swaps

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/channeldb"
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
	// defaultInvoiceExpiry is the default expiry duration for swap invoices
	// when none is specified by the caller.
	defaultInvoiceExpiry = time.Hour

	// defaultCLTVExpiry is the default CLTV expiry delta for invoices.
	defaultCLTVExpiry = 40

	// maxRouteHintPaths caps the number of BOLT-11 "r" fields embedded in
	// one invoice, matching lnd's invoicesrpc hop-hint limit. Rejecting an
	// oversized quote here names the actual path count instead of failing
	// later inside lnd with a generic error.
	maxRouteHintPaths = 20
)

// InvoiceStore persists created invoices so they can be looked up when HTLCs
// arrive.
type InvoiceStore interface {
	// AddInvoice stores a new invoice keyed by payment hash.
	AddInvoice(ctx context.Context, invoice *invoices.Invoice,
		paymentHash lntypes.Hash) (uint64, error)
}

// NewPreimage generates a cryptographically random 32-byte preimage suitable
// for use as a Lightning payment preimage.
func NewPreimage() (lntypes.Preimage, error) {
	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		return preimage, fmt.Errorf("generate preimage: %w", err)
	}

	return preimage, nil
}

// InvoiceGenerator creates properly signed Lightning invoices with route hints
// pointing through the swap server. It delegates invoice construction to lnd's
// invoicesrpc.AddInvoice machinery so feature bits, payment address handling,
// and invoice storage follow the same path as normal lnd invoices.
type InvoiceGenerator struct {
	invoiceCfg  *invoicesrpc.AddInvoiceConfig
	chainParams *chaincfg.Params
}

// DirectInvoiceCreator is kept for source compatibility with older SDK users.
// It now delegates to InvoiceGenerator instead of manually encoding invoices.
type DirectInvoiceCreator struct {
	generator *InvoiceGenerator
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
// The bestHeight function returns the current best block height. The store
// persists created invoices.
func NewInvoiceGenerator(signer keychain.SingleKeyMessageSigner,
	bestHeight func() (uint32, error), store InvoiceStore,
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

	if bestHeight == nil {
		bestHeight = func() (uint32, error) {
			return 0, nil
		}
	}
	if store == nil {
		store = NewMemoryInvoiceStore()
	}

	return &invoicesrpc.AddInvoiceConfig{
		AddInvoice: store.AddInvoice,
		IsChannelActive: func(lnwire.ChannelID) bool {
			return true
		},
		ChainParams:       chainParams,
		NodeSigner:        nodeSigner,
		DefaultCLTVExpiry: defaultCLTVExpiry,
		ChanDB:            &mockChanDB{},
		Graph:             &mockGraph{},
		GenInvoiceFeatures: func() *lnwire.FeatureVector {
			return lnwire.NewFeatureVector(
				lnwire.NewRawFeatureVector(
					lnwire.TLVOnionPayloadRequired,
					lnwire.PaymentAddrRequired,
					lnwire.MPPOptional,
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
		GetAlias: func(lnwire.ChannelID) (lnwire.ShortChannelID,
			error) {

			return lnwire.ShortChannelID{}, nil
		},
		BestHeight: bestHeight,
		QueryBlindedRoutes: func(lnwire.MilliSatoshi) ([]*route.Route,
			error) {

			return nil, nil
		},
	}
}

// mockGraph satisfies the subset of graph lookups AddInvoice expects when
// normalizing caller-provided route hints for virtual channels.
type mockGraph struct{}

// IsPublicNode reports all nodes as public for route hint validation.
func (m *mockGraph) IsPublicNode(_ context.Context, _ [33]byte) (bool, error) {
	return true, nil
}

// FetchChannelEdgesByID reports no backing graph edges for swap virtual
// channels.
func (m *mockGraph) FetchChannelEdgesByID(_ context.Context, _ uint64) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	return nil, nil, nil, nil
}

// mockChanDB satisfies chanstate.OpenChannelStore for the swap invoice
// generator. These invoices are built from explicit caller-provided route
// hints for virtual channels and have no backing channel database. lnd's
// AddInvoice unconditionally dereferences ChanDB.FetchAllChannels while
// assembling hop hints, so a nil store panics; every method here reports an
// empty channel set instead.
type mockChanDB struct{}

// FetchAllChannels reports no stored channels.
func (m *mockChanDB) FetchAllChannels() ([]*channeldb.OpenChannel, error) {
	return nil, nil
}

// FetchAllOpenChannels reports no open channels.
func (m *mockChanDB) FetchAllOpenChannels() ([]*channeldb.OpenChannel, error) {
	return nil, nil
}

// FetchPendingChannels reports no pending channels.
func (m *mockChanDB) FetchPendingChannels() ([]*channeldb.OpenChannel, error) {
	return nil, nil
}

// FetchWaitingCloseChannels reports no waiting-close channels.
func (m *mockChanDB) FetchWaitingCloseChannels() ([]*channeldb.OpenChannel,
	error) {

	return nil, nil
}

// FetchOpenChannels reports no channels for the given node.
func (m *mockChanDB) FetchOpenChannels(_ *btcec.PublicKey) (
	[]*channeldb.OpenChannel, error) {

	return nil, nil
}

// FetchChannel reports no channel for the given outpoint.
func (m *mockChanDB) FetchChannel(_ wire.OutPoint) (*channeldb.OpenChannel,
	error) {

	return nil, channeldb.ErrChannelNotFound
}

// FetchChannelByID reports no channel for the given channel ID.
func (m *mockChanDB) FetchChannelByID(_ lnwire.ChannelID) (
	*channeldb.OpenChannel, error) {

	return nil, channeldb.ErrChannelNotFound
}

// FetchPermAndTempPeers reports no known peers.
func (m *mockChanDB) FetchPermAndTempPeers(_ []byte) (
	map[string]channeldb.ChanCount, error) {

	return nil, nil
}

// RestoreChannelShells is a no-op; the swap invoice generator never restores
// channel state.
func (m *mockChanDB) RestoreChannelShells(_ ...*channeldb.ChannelShell) error {
	return nil
}

// NewEphemeralInvoiceGenerator creates an ephemeral invoice creator backed by
// a private key and the standard AddInvoice invoice construction path.
func NewEphemeralInvoiceGenerator(privKey *btcec.PrivateKey,
	bestHeight func() (uint32, error),
	chainParams *chaincfg.Params) InvoiceCreator {

	signer := keychain.NewPrivKeyMessageSigner(
		privKey, keychain.KeyLocator{},
	)

	return &DirectInvoiceCreator{
		generator: NewInvoiceGenerator(
			signer, bestHeight, NewMemoryInvoiceStore(),
			chainParams,
		),
	}
}

// CreateInvoice builds a signed BOLT-11 Lightning invoice with a route hint
// pointing through the swap server's virtual channel. When preimage is non-nil,
// the invoice is locked to that preimage so the caller can construct a matching
// vHTLC.
func (g *InvoiceGenerator) CreateInvoice(ctx context.Context,
	amountSat btcutil.Amount, memo string, routeHint *RouteHint,
	expiry time.Duration, preimage *lntypes.Preimage) (*invoices.Invoice,
	lntypes.Hash, error) {

	return g.createInvoice(
		ctx, g.invoiceCfg, amountSat, memo, [][]*RouteHint{{routeHint}},
		expiry, preimage,
	)
}

// CreateInvoiceWithKey creates one invoice signed by the supplied auth key.
func (g *InvoiceGenerator) CreateInvoiceWithKey(ctx context.Context,
	amountSat btcutil.Amount, memo string, routeHint *RouteHint,
	expiry time.Duration, authKey keychain.SingleKeyMessageSigner,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	if authKey == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice auth key is " +
			"required")
	}
	if g == nil || g.invoiceCfg == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice generator is " +
			"required")
	}

	invoiceCfg := *g.invoiceCfg
	invoiceCfg.NodeSigner = netann.NewNodeSigner(authKey)

	return g.createInvoice(
		ctx, &invoiceCfg, amountSat, memo, [][]*RouteHint{{routeHint}},
		expiry, preimage,
	)
}

// CreateInvoiceWithKeyRouteHintPaths creates one invoice signed by the
// supplied auth key and carrying one BOLT-11 "r" field per route-hint path.
// A multi-backend swap server hands back one path per backend node, all
// terminating at the same virtual channel, so the sender can route through
// whichever backend its pathfinding can reach.
func (g *InvoiceGenerator) CreateInvoiceWithKeyRouteHintPaths(
	ctx context.Context, amountSat btcutil.Amount, memo string,
	routeHintPaths [][]*RouteHint, expiry time.Duration,
	authKey keychain.SingleKeyMessageSigner, preimage *lntypes.Preimage) (
	*invoices.Invoice, lntypes.Hash, error) {

	if authKey == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice auth key is " +
			"required")
	}
	if g == nil || g.invoiceCfg == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice generator is " +
			"required")
	}

	invoiceCfg := *g.invoiceCfg
	invoiceCfg.NodeSigner = netann.NewNodeSigner(authKey)

	return g.createInvoice(
		ctx, &invoiceCfg, amountSat, memo, routeHintPaths, expiry,
		preimage,
	)
}

// createInvoice builds one signed BOLT-11 invoice through invoicesrpc.
func (g *InvoiceGenerator) createInvoice(ctx context.Context,
	cfg *invoicesrpc.AddInvoiceConfig, amountSat btcutil.Amount,
	memo string, routeHintPaths [][]*RouteHint, expiry time.Duration,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	if g == nil || cfg == nil || cfg.NodeSigner == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice generator is " +
			"required")
	}
	if cfg.ChainParams == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("chain parameters are " +
			"required")
	}
	if err := validateInvoiceAmount(amountSat); err != nil {
		return nil, lntypes.Hash{}, err
	}

	if len(routeHintPaths) == 0 {
		return nil, lntypes.Hash{}, fmt.Errorf("route hint path is " +
			"required")
	}
	if len(routeHintPaths) > maxRouteHintPaths {
		return nil, lntypes.Hash{}, fmt.Errorf("%d route hint paths "+
			"exceed the maximum of %d", len(routeHintPaths),
			maxRouteHintPaths)
	}
	routeHints := make([][]zpay32.HopHint, 0, len(routeHintPaths))
	for i, routeHintPath := range routeHintPaths {
		hopHints, err := zpayHopHintPath(routeHintPath)
		if err != nil {
			return nil, lntypes.Hash{}, fmt.Errorf("route hint "+
				"path %d: %w", i, err)
		}

		routeHints = append(routeHints, hopHints)
	}
	if expiry == 0 {
		expiry = defaultInvoiceExpiry
	}

	invoiceData := &invoicesrpc.AddInvoiceData{
		Memo:     memo,
		Preimage: preimage,
		Value: lnwire.NewMSatFromSatoshis(
			amountSat,
		),
		RouteHints: routeHints,
		Expiry:     int64(expiry.Seconds()),
	}

	paymentHash, invoice, err := invoicesrpc.AddInvoice(
		ctx, cfg, invoiceData,
	)
	if err != nil {
		return nil, lntypes.Hash{}, fmt.Errorf("create invoice: %w",
			err)
	}

	return invoice, *paymentHash, nil
}

// CreateInvoice creates one signed BOLT-11 invoice using AddInvoice.
func (g *DirectInvoiceCreator) CreateInvoice(ctx context.Context,
	amountSat btcutil.Amount, memo string, routeHint *RouteHint,
	expiry time.Duration, preimage *lntypes.Preimage) (*invoices.Invoice,
	lntypes.Hash, error) {

	if g == nil || g.generator == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice generator is " +
			"required")
	}

	return g.generator.CreateInvoice(
		ctx, amountSat, memo, routeHint, expiry, preimage,
	)
}

// CreateInvoiceWithKey creates one invoice signed by the supplied auth key.
func (g *DirectInvoiceCreator) CreateInvoiceWithKey(ctx context.Context,
	amountSat btcutil.Amount, memo string, routeHint *RouteHint,
	expiry time.Duration, authKey keychain.SingleKeyMessageSigner,
	preimage *lntypes.Preimage) (*invoices.Invoice, lntypes.Hash, error) {

	if g == nil || g.generator == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice generator is " +
			"required")
	}

	return g.generator.CreateInvoiceWithKey(
		ctx, amountSat, memo, routeHint, expiry, authKey, preimage,
	)
}

// CreateInvoiceWithKeyRouteHintPaths creates one signed invoice carrying one
// BOLT-11 "r" field per route-hint path, signed by the supplied auth key.
func (g *DirectInvoiceCreator) CreateInvoiceWithKeyRouteHintPaths(
	ctx context.Context, amountSat btcutil.Amount, memo string,
	routeHintPaths [][]*RouteHint, expiry time.Duration,
	authKey keychain.SingleKeyMessageSigner, preimage *lntypes.Preimage) (
	*invoices.Invoice, lntypes.Hash, error) {

	if g == nil || g.generator == nil {
		return nil, lntypes.Hash{}, fmt.Errorf("invoice generator is " +
			"required")
	}

	return g.generator.CreateInvoiceWithKeyRouteHintPaths(
		ctx, amountSat, memo, routeHintPaths, expiry, authKey, preimage,
	)
}

func validateInvoiceAmount(amountSat btcutil.Amount) error {
	return validateSatoshiAmount(amountSat, "invoice amount")
}

func validateSatoshiAmount(amountSat btcutil.Amount, label string) error {
	switch {
	case amountSat <= 0:
		return fmt.Errorf("%s must be positive", label)

	case amountSat > btcutil.MaxSatoshi:
		return fmt.Errorf("%s %d exceeds max bitcoin supply %d", label,
			amountSat, btcutil.Amount(btcutil.MaxSatoshi))

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
		return zpay32.HopHint{}, fmt.Errorf("parse route hint "+
			"node ID: %w", err)
	}

	return zpay32.HopHint{
		NodeID:                    nodePubkey,
		ChannelID:                 routeHint.ChannelID,
		FeeBaseMSat:               uint32(routeHint.FeeBaseMsat),
		FeeProportionalMillionths: uint32(routeHint.FeePropPpm),
		CLTVExpiryDelta:           uint16(routeHint.CltvExpiryDelta),
	}, nil
}

// zpayHopHintPath converts the ordered SDK route-hint path into the zpay32
// representation embedded in a BOLT-11 invoice.
func zpayHopHintPath(routeHintPath []*RouteHint) ([]zpay32.HopHint, error) {
	if len(routeHintPath) == 0 {
		return nil, fmt.Errorf("route hint path is required")
	}

	hopHints := make([]zpay32.HopHint, 0, len(routeHintPath))
	for i, routeHint := range routeHintPath {
		hopHint, err := zpayHopHint(routeHint)
		if err != nil {
			return nil, fmt.Errorf("route hint path hop %d: %w", i,
				err)
		}

		hopHints = append(hopHints, hopHint)
	}

	return hopHints, nil
}

// validateRouteHint checks that one SDK route hint can be safely converted
// into the narrower zpay32 hop hint shape.
func validateRouteHint(routeHint *RouteHint) error {
	if routeHint == nil {
		return fmt.Errorf("route hint is required")
	}

	if len(routeHint.NodeID) == 0 {
		return fmt.Errorf("route hint node ID is required")
	}

	if routeHint.ChannelID == 0 {
		return fmt.Errorf("route hint channel ID is required")
	}

	if routeHint.FeeBaseMsat > uint64(^uint32(0)) {
		return fmt.Errorf("route hint fee base msat %d exceeds uint32",
			routeHint.FeeBaseMsat)
	}

	if routeHint.FeePropPpm > uint64(^uint32(0)) {
		return fmt.Errorf("route hint fee proportional ppm %d "+
			"exceeds uint32", routeHint.FeePropPpm)
	}

	if routeHint.CltvExpiryDelta > uint32(^uint16(0)) {
		return fmt.Errorf("route hint CLTV expiry delta %d "+
			"exceeds uint16", routeHint.CltvExpiryDelta)
	}
	if routeHint.CltvExpiryDelta == 0 {
		return fmt.Errorf("route hint CLTV expiry delta is required")
	}

	return nil
}
