package serverconn

import (
	"context"
	"fmt"
	"slices"

	"github.com/lightninglabs/wavelength/arkrpc"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"google.golang.org/grpc"
)

// ArkVersionGetInfoClient is the minimal direct-gRPC surface the negotiator
// needs. Negotiation deliberately uses the operator's direct ArkService
// connection, NOT the mailbox edge: a restarted client can have queued
// server-push envelopes for not-yet-registered actors, so routing negotiation
// over the mailbox could deadlock bootstrap behind redelivery.
type ArkVersionGetInfoClient interface {
	// GetInfo returns the operator's info, including its Ark protocol
	// version selection and policies.
	GetInfo(ctx context.Context, in *arkrpc.GetInfoRequest,
		opts ...grpc.CallOption) (*arkrpc.GetInfoResponse, error)
}

// ArkVersionNegotiator owns Ark protocol version selection against an operator
// over its direct ArkService connection. It is the single home for the
// version-compatibility decision logic; the daemon supplies the transport
// client and parses domain terms out of the returned response.
type ArkVersionNegotiator struct {
	// client is the direct ArkService connection used for GetInfo.
	client ArkVersionGetInfoClient

	// clientSupported is the client's Ark protocol versions, ordered by
	// preference, advertised to the operator during bootstrap.
	clientSupported []uint32
}

// NewArkVersionNegotiator builds a negotiator over the given direct ArkService
// client and the client's supported Ark protocol versions.
func NewArkVersionNegotiator(client ArkVersionGetInfoClient,
	clientSupported []uint32) *ArkVersionNegotiator {

	return &ArkVersionNegotiator{
		client:          client,
		clientSupported: clientSupported,
	}
}

// Bootstrap performs the single bootstrap GetInfo call that owns Ark protocol
// version selection. It returns the operator's full response (so the caller
// can parse domain terms from the same response without a second call) and the
// selected version to bind to the runtime. It returns an error without
// selecting a version when no compatible version exists, so the caller can
// refuse to create the runtime.
func (n *ArkVersionNegotiator) Bootstrap(ctx context.Context) (
	*arkrpc.GetInfoResponse, uint32, error) {

	if n.client == nil {
		return nil, 0, fmt.Errorf("operator connection not initialized")
	}

	resp, err := n.client.GetInfo(ctx, &arkrpc.GetInfoRequest{
		SupportedArkVersions: n.clientSupported,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("GetInfo RPC: %w", err)
	}

	selected, err := resolveArkVersionSelection(resp, n.clientSupported)
	if err != nil {
		return nil, 0, err
	}

	return resp, selected, nil
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
			activeArkVersions(resp), clientSupported)
	}

	// Honor the operator's selection as long as this client supports it.
	if !slices.Contains(clientSupported, resp.SelectedArkVersion) {
		return 0, fmt.Errorf("operator selected unsupported ark "+
			"protocol version %d (client supports %v)",
			resp.SelectedArkVersion, clientSupported)
	}

	// A contradictory operator response selects a version it simultaneously
	// advertises as DISABLED. Fail closed: refuse to bind a runtime to a
	// version the operator says is retired. Surface it as a permanent
	// UPGRADE_REQUIRED status error carrying the operator's advertised
	// enabled versions. An absent policy for the selected version is
	// allowed (no contradiction), preserving compatibility with operators
	// that advertise no policy for it.
	policy := selectedArkPolicy(resp, resp.SelectedArkVersion)
	if policy != nil &&
		policy.State == arkrpc.ArkVersionPolicy_STATE_DISABLED {
		return 0, disabledSelectionStatus(
			"bootstrap", resp.SelectedArkVersion, resp,
		)
	}

	return resp.SelectedArkVersion, nil
}

// ValidateRefreshSelection enforces that a refresh-only GetInfo response keeps
// the runtime bound to boundVersion. A matching selection passes (returns nil).
// Any other selection — zero, or a different non-zero version — is a terminal
// compatibility failure, returned as a typed ARK_VERSION_MISMATCH StatusError
// so callers can both classify it as permanent and surface actionable guidance.
// There is no legacy fallback: the client and operator are deployed together.
//
// The status carries the operator's currently enabled versions.
func ValidateRefreshSelection(resp *arkrpc.GetInfoResponse,
	boundVersion uint32) *mailboxconn.StatusError {

	if resp == nil {
		return mailboxconn.NewStatusError("refresh", &mailboxpb.Status{
			Ok:   false,
			Code: mailboxconn.StatusArkVersionMismatch,
			Message: fmt.Sprintf("operator refresh returned nil "+
				"GetInfo response, runtime bound to %d",
				boundVersion),
		})
	}

	if resp.SelectedArkVersion == boundVersion {
		// The operator re-selected the bound version, which is normally
		// a successful refresh. But a contradictory response also
		// advertises that same selected version as DISABLED. Fail
		// closed: this is a terminal, mandatory-upgrade condition, so
		// return a permanent UPGRADE_REQUIRED error. An absent policy
		// is allowed (no contradiction), preserving compatibility with
		// operators that advertise no policy for the bound version.
		policy := selectedArkPolicy(resp, boundVersion)
		if policy != nil &&
			policy.State == arkrpc.ArkVersionPolicy_STATE_DISABLED {
			return disabledSelectionStatus(
				"refresh", boundVersion, resp,
			)
		}

		return nil
	}

	return mailboxconn.NewStatusError("refresh", &mailboxpb.Status{
		Ok:   false,
		Code: mailboxconn.StatusArkVersionMismatch,
		Message: fmt.Sprintf("operator refresh selected ark version "+
			"%d, runtime bound to %d", resp.SelectedArkVersion,
			boundVersion),
		SupportedArkVersions: activeArkVersions(resp),
	})
}

// disabledSelectionStatus builds the permanent UPGRADE_REQUIRED status error
// returned when the operator selects an Ark protocol version it simultaneously
// advertises as DISABLED. op names the originating path ("bootstrap" or
// "refresh"); the operator's enabled versions are preserved so the daemon can
// surface actionable guidance.
func disabledSelectionStatus(op string, version uint32,
	resp *arkrpc.GetInfoResponse) *mailboxconn.StatusError {

	return mailboxconn.NewStatusError(op, &mailboxpb.Status{
		Ok:   false,
		Code: mailboxconn.StatusUpgradeRequired,
		Message: fmt.Sprintf("operator selected ark protocol version "+
			"%d but advertises it as disabled", version),
		SupportedArkVersions: activeArkVersions(resp),
	})
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

// activeArkVersions returns the operator's enabled (ACTIVE) Ark protocol
// versions, read from the response's per-version policy list. The enabled set
// is derived from the policies because every ACTIVE policy is an enabled
// version. The result is used only for diagnostic messages and status errors,
// never for selection, so policy order (version-sorted) rather than operator
// preference order is acceptable here.
func activeArkVersions(resp *arkrpc.GetInfoResponse) []uint32 {
	if resp == nil {
		return nil
	}

	var versions []uint32
	for _, policy := range resp.ArkVersionPolicies {
		if policy == nil {
			continue
		}

		if policy.State == arkrpc.ArkVersionPolicy_STATE_ACTIVE {
			versions = append(versions, policy.Version)
		}
	}

	return versions
}
