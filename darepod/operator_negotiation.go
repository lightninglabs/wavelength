package darepod

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/types"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
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

	// selectedPolicy is the deprecation policy advertised for the selected
	// version, or nil when the operator advertised none.
	selectedPolicy *arkrpc.ArkVersionPolicy
}

// arkVersionSupported reports whether version is present in the client's
// supported list.
func arkVersionSupported(supported []uint32, version uint32) bool {
	for _, v := range supported {
		if v == version {
			return true
		}
	}

	return false
}

// resolveArkVersionSelection interprets a bootstrap GetInfo response against
// the client's supported Ark protocol versions. It returns the version to bind
// or an error when no compatible version exists. It is the single decision
// point for runtime-version selection and is deliberately pure so it can be
// unit-tested with fake responses.
//
// A non-zero selection is honored as long as the client supports it. A zero
// selection always means no compatible version exists and is fatal — the
// runtime must not be created. There is no legacy-server fallback: the client
// and operator are deployed together, so the operator always returns an
// explicit selection.
func resolveArkVersionSelection(resp *arkrpc.GetInfoResponse,
	clientSupported []uint32) (uint32, error) {

	if resp == nil {
		return 0, fmt.Errorf("nil GetInfo response")
	}

	// A zero selection means the operator advertised no common version (or
	// is a pre-versioning server that cannot stamp the field). Either way
	// it is fatal: the runtime must not be created.
	if resp.SelectedArkVersion == 0 {
		return 0, fmt.Errorf("no compatible ark protocol version "+
			"(operator supports %v, client supports %v)",
			resp.SupportedArkVersions, clientSupported)
	}

	// Honor the operator's selection as long as this client supports it.
	if !arkVersionSupported(clientSupported, resp.SelectedArkVersion) {
		return 0, fmt.Errorf("operator selected unsupported ark "+
			"protocol version %d (client supports %v)",
			resp.SelectedArkVersion, clientSupported)
	}

	return resp.SelectedArkVersion, nil
}

// selectedArkPolicy returns the policy advertised for the given version, or
// nil if the operator advertised none.
func selectedArkPolicy(resp *arkrpc.GetInfoResponse,
	version uint32) *arkrpc.ArkVersionPolicy {

	if resp == nil {
		return nil
	}

	for _, policy := range resp.ArkVersionPolicies {
		if policy != nil && policy.Version == version {
			return policy
		}
	}

	return nil
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

	var sweepKey *btcec.PublicKey
	if len(resp.SweepKey) > 0 {
		sweepKey, err = btcec.ParsePubKey(resp.SweepKey)
		if err != nil {
			return nil, fmt.Errorf("parse sweep key: %w", err)
		}
	}

	return &types.OperatorTerms{
		PubKey:              pubKey,
		BoardingExitDelay:   resp.BoardingExitDelay,
		VTXOExitDelay:       resp.VtxoExitDelay,
		ForfeitScript:       resp.ForfeitScript,
		SweepKey:            sweepKey,
		SweepDelay:          resp.SweepDelay,
		DustLimit:           btcutil.Amount(resp.DustLimit),
		MinBoardingAmount:   btcutil.Amount(resp.MinBoardingAmount),
		MaxBoardingAmount:   btcutil.Amount(resp.MaxBoardingAmount),
		FeeRate:             btcutil.Amount(resp.FeeRate),
		MinOperatorFee:      btcutil.Amount(resp.MinOperatorFee),
		MinConfirmations:    resp.MinConfirmations,
		MaxOORLineageVBytes: resp.MaxOorLineageVbytes,
	}, nil
}

// clientSupportedArkVersions returns the Ark protocol versions this client
// advertises during bootstrap, ordered by preference. Production supports only
// v1; no production default advertises a higher version. Tests negotiate
// against this same default unless they call resolveArkVersionSelection or
// negotiateArkBootstrap with an explicit list.
func clientSupportedArkVersions() []uint32 {
	return []uint32{arkrpc.ArkProtocolVersionV1}
}

// logArkVersionDeprecation surfaces the disable deadline and upgrade URL when
// the negotiated Ark protocol version is deprecating. Deprecation is an
// external operational condition rather than an internal bug, so it is logged
// below error level.
func (s *Server) logArkVersionDeprecation(ctx context.Context,
	policy *arkrpc.ArkVersionPolicy) {

	if policy == nil ||
		policy.State != arkrpc.ArkVersionPolicy_STATE_DEPRECATING {
		return
	}

	s.log.WarnS(ctx, "Negotiated ark protocol version is deprecating",
		nil,
		slog.Uint64("ark_protocol_version", uint64(policy.Version)),
		slog.Int64("disable_after_unix_s", policy.DisableAfterUnixS),
		slog.String("upgrade_url", policy.UpgradeUrl),
	)
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
		slog.String("upgrade_url", statusErr.UpgradeURL()),
	)
}

// negotiateArkBootstrap performs the single bootstrap GetInfo call that owns
// Ark protocol version selection. It sends the client's complete supported
// version list and parses everything needed before the mailbox runtime is
// created: the operator public key, the selected version, the initial
// operator terms, and the selected version's deprecation policy.
//
// It returns an error without selecting a version when no compatible version
// exists, so the caller can refuse to create the runtime.
func (s *Server) negotiateArkBootstrap(ctx context.Context,
	clientSupported []uint32) (*arkVersionNegotiation, error) {

	client := s.operatorArkClient()
	if client == nil {
		return nil, fmt.Errorf("operator connection not initialized")
	}

	resp, err := client.GetInfo(ctx, &arkrpc.GetInfoRequest{
		SupportedArkVersions: clientSupported,
	})
	if err != nil {
		return nil, fmt.Errorf("GetInfo RPC: %w", err)
	}

	terms, err := operatorTermsFromResponse(resp)
	if err != nil {
		return nil, err
	}

	selected, err := resolveArkVersionSelection(resp, clientSupported)
	if err != nil {
		return nil, err
	}

	return &arkVersionNegotiation{
		operatorPubKey:     terms.PubKey,
		selectedArkVersion: selected,
		terms:              terms,
		selectedPolicy:     selectedArkPolicy(resp, selected),
	}, nil
}
