package waved

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/types"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	"github.com/lightninglabs/wavelength/serverconn"
)

// arkVersionNegotiation is the outcome of the bootstrap GetInfo negotiation.
// connectAndBootstrapMailbox is the sole owner of this negotiation: it caches
// every value below before constructing the mailbox runtime, so the later
// round-actor initialization and refresh-only GetInfo calls never renegotiate.
type arkVersionNegotiation struct {
	// operatorPubKey is the operator's main public key.
	operatorPubKey *btcec.PublicKey

	// selectedArkVersion is the Ark protocol version to bind to the
	// runtime. It is always non-zero on a successful negotiation.
	selectedArkVersion uint32

	// terms is the initial operator terms parsed from the same response, so
	// the round actor does not need a second negotiating call.
	terms *types.OperatorTerms
}

// operatorTermsFromResponse parses operator terms from a GetInfo response. It
// is shared by the bootstrap negotiation and the refresh-only path so both
// produce identical terms from the same fields.
func operatorTermsFromResponse(resp *arkrpc.GetInfoResponse) (
	*types.OperatorTerms, error) {

	if resp == nil {
		return nil, fmt.Errorf("nil GetInfo response")
	}

	if len(resp.Pubkey) == 0 {
		return nil, fmt.Errorf("operator pubkey is missing")
	}

	pubKey, err := btcec.ParsePubKey(resp.Pubkey)
	if err != nil {
		return nil, fmt.Errorf("parse operator pubkey: %w", err)
	}

	minVTXOAmount := resp.MinVtxoAmountSat
	if minVTXOAmount < resp.DustLimit {
		minVTXOAmount = resp.DustLimit
	}

	// The forfeit penalty key, sweep key and sweep delay are no longer
	// global operator terms; they are delivered per round in the batch
	// info, so GetInfo no longer carries them.
	return &types.OperatorTerms{
		PubKey:                  pubKey,
		BoardingExitDelay:       resp.BoardingExitDelay,
		VTXOExitDelay:           resp.VtxoExitDelay,
		DustLimit:               btcutil.Amount(resp.DustLimit),
		MinVTXOAmount:           btcutil.Amount(minVTXOAmount),
		MinBoardingAmount:       btcutil.Amount(resp.MinBoardingAmount),
		MaxVTXOAmount:           btcutil.Amount(resp.MaxVtxoAmount),
		FeeRate:                 btcutil.Amount(resp.FeeRate),
		MinOperatorFee:          btcutil.Amount(resp.MinOperatorFee),
		FreeRefreshWindowBlocks: resp.FreeRefreshWindowBlocks,
		MinConfirmations:        resp.MinConfirmations,
		MaxOORLineageVBytes:     resp.MaxOorLineageVbytes,
		MaxUserBalance:          btcutil.Amount(resp.MaxUserBalance),
	}, nil
}

// clientSupportedArkVersions returns the Ark protocol versions this client
// advertises during bootstrap, ordered by preference. V2 remains deliberately
// absent until the complete one-confirmation safety stack is enabled.
func clientSupportedArkVersions() []uint32 {
	return []uint32{arkrpc.ArkProtocolVersionV1}
}

// onServerIncompatible is the connector compatibility callback. When the
// mailbox connector hits its first permanent version error it transitions to a
// terminal incompatible state and invokes this once. We mark the daemon
// disconnected and log the actionable upgrade information. This is an external
// compatibility condition rather than an internal bug, so it logs below error
// level.
func (s *Server) onServerIncompatible(statusErr *mailboxconn.StatusError) {
	s.serverConnected.Store(false)

	ctx := context.Background()

	if statusErr == nil {
		s.log.WarnS(ctx, "Server connection became incompatible", nil)

		return
	}

	s.log.WarnS(ctx, "Server connection became incompatible", statusErr,
		slog.String("code", statusErr.Code()),
		slog.Any(
			"supported_mailbox_versions",
			statusErr.SupportedMailboxVersions(),
		),
		slog.Any(
			"supported_ark_versions",
			statusErr.SupportedArkVersions(),
		),
	)
}

// negotiateArkBootstrap performs the single bootstrap GetInfo call that owns
// Ark protocol version selection. It delegates the version decision to a
// serverconn.ArkVersionNegotiator over the direct ArkService connection, then
// parses the initial operator terms from the same response so the round actor
// never renegotiates.
//
// It returns an error without selecting a version when no compatible version
// exists, so the caller can refuse to create the runtime.
func (s *Server) negotiateArkBootstrap(ctx context.Context,
	clientSupported []uint32) (*arkVersionNegotiation, error) {

	client := s.operatorArkClient()
	if client == nil {
		return nil, fmt.Errorf("operator connection not initialized")
	}

	negotiator := serverconn.NewArkVersionNegotiator(
		client, clientSupported,
	)

	resp, selected, err := negotiator.Bootstrap(ctx)
	if err != nil {
		return nil, err
	}

	terms, err := operatorTermsFromResponse(resp)
	if err != nil {
		return nil, err
	}

	return &arkVersionNegotiation{
		operatorPubKey:     terms.PubKey,
		selectedArkVersion: selected,
		terms:              terms,
	}, nil
}
