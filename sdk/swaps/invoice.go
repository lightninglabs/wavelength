package swaps

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
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

	cfg := &invoicesrpc.AddInvoiceConfig{
		AddInvoice: store.AddInvoice,
		IsChannelActive: func(
			chanID lnwire.ChannelID) bool {

			return true
		},
		ChainParams:       chainParams,
		NodeSigner:        nodeSigner,
		DefaultCLTVExpiry: defaultCLTVExpiry,
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
	}

	return &InvoiceGenerator{
		invoiceCfg:  cfg,
		chainParams: chainParams,
	}
}

// CreateInvoice builds a signed BOLT-11 Lightning invoice with a
// route hint pointing through the swap server's virtual channel.
// Returns the invoice, its payment hash, and any error.
func (g *InvoiceGenerator) CreateInvoice(ctx context.Context,
	amountSat btcutil.Amount, memo string,
	routeHint *RouteHint,
	expiry time.Duration) (*invoices.Invoice, lntypes.Hash,
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
		Memo: memo,
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
