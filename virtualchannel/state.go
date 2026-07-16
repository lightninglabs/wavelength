package virtualchannel

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ID is the stable Wavelength identifier for a virtual channel registration.
type ID [32]byte

// PendingChannelID is lnd's temporary channel identifier during funding.
type PendingChannelID [32]byte

// NodePubKey is a compressed lnd node public key.
type NodePubKey [33]byte

// Role identifies which side of the virtual channel the local daemon owns.
type Role string

const (
	// RoleClient means the local daemon owns the client side.
	RoleClient Role = "client"

	// RoleOperator means the local daemon owns the operator side.
	RoleOperator Role = "operator"
)

// IntentKind identifies how the VTXO backing a channel is obtained.
type IntentKind string

const (
	// KindPromoteVTXO promotes one existing, confirmed VTXO to a channel.
	KindPromoteVTXO IntentKind = "promote_vtxo"

	// KindReceiveChannel creates an operator-funded VTXO in a round and
	// binds it to a channel before the round can complete.
	KindReceiveChannel IntentKind = "receive_channel"
)

// Status describes the persisted virtual channel lifecycle.
type Status string

const (
	// StatusRequested means a receive-channel request has been persisted
	// but no exact funding VTXO has been bound yet.
	StatusRequested Status = "requested"

	// StatusRoundRequested means the receive-channel request is durably
	// queued for the round FSM, but the round has not fixed its backing
	// VTXO yet.
	StatusRoundRequested Status = "round_requested"

	// StatusFundingBound means the exact VTXO descriptor is durable.
	StatusFundingBound Status = "funding_bound"

	// StatusLNDNegotiating means lnd's no-publish funding flow is in
	// progress.
	StatusLNDNegotiating Status = "lnd_negotiating"

	// StatusNegotiating is retained as a source-compatible alias while
	// callers move to the explicit FSM name.
	StatusNegotiating = StatusLNDNegotiating

	// StatusFundingVerified means lnd durably stored the pending channel
	// for the exact persisted no-publish PSBT. Backing signatures may only
	// be collected after this milestone.
	StatusFundingVerified Status = "funding_verified"

	// StatusBackingArmed means the VTXO-to-channel transaction is fully
	// signed and script verified. A receive round may release final
	// signatures in this state, but the channel is not yet advertised as
	// usable.
	StatusBackingArmed Status = "backing_armed"

	// StatusRoundConfirmed means the round that created a receive channel's
	// backing VTXO has confirmed.
	StatusRoundConfirmed Status = "round_confirmed"

	// StatusActive means lnd can route over the channel without
	// broadcasting.
	StatusActive Status = "active"

	// StatusFundingPublished means the operator has materialized the signed
	// VTXO ancestry and the channel funding transaction has reached a
	// mempool or confirmed. The channel remains routable as an ordinary lnd
	// channel.
	StatusFundingPublished Status = "funding_published"

	// StatusMaterializing means Wavelength is publishing the backing
	// parent.
	StatusMaterializing Status = "materializing"

	// StatusClosing means lnd or Wavelength is resolving the channel close.
	StatusClosing Status = "closing"

	// StatusClosed means the channel reached a terminal close state.
	StatusClosed Status = "closed"

	// StatusFailed means negotiation or materialization reached a terminal
	// failure.
	StatusFailed Status = "failed"
)

// ValidateTransition checks one durable virtual-channel FSM edge.
func ValidateTransition(kind IntentKind, from, to Status) error {
	if kind == "" {
		kind = KindPromoteVTXO
	}

	allowed := false
	switch from {
	case StatusRequested:
		allowed = to == StatusRoundRequested || to == StatusFailed

	case StatusRoundRequested:
		allowed = to == StatusFundingBound || to == StatusFailed

	case StatusFundingBound:
		allowed = to == StatusLNDNegotiating || to == StatusFailed

	case StatusLNDNegotiating:
		allowed = to == StatusFundingVerified || to == StatusFailed

	case StatusFundingVerified:
		allowed = to == StatusBackingArmed || to == StatusFailed

	case StatusBackingArmed:
		allowed = to == StatusMaterializing
		if kind == KindPromoteVTXO {
			allowed = allowed || to == StatusActive
		} else if kind == KindReceiveChannel {
			// A failed round stops activation but keeps its signed
			// backing recoverable if the round transaction later
			// appears.
			allowed = allowed || to == StatusRoundConfirmed ||
				to == StatusClosing
		}

	case StatusRoundConfirmed:
		allowed = kind == KindReceiveChannel &&
			(to == StatusActive || to == StatusMaterializing)

	case StatusActive:
		allowed = to == StatusFundingPublished ||
			to == StatusMaterializing || to == StatusClosing ||
			to == StatusClosed

	case StatusFundingPublished:
		allowed = to == StatusClosing || to == StatusClosed

	case StatusMaterializing:
		allowed = to == StatusFundingPublished ||
			to == StatusClosing || to == StatusClosed

	case StatusClosing:
		allowed = to == StatusMaterializing || to == StatusClosed

	case StatusClosed, StatusFailed:
	}

	if !allowed {
		return fmt.Errorf("invalid %s channel transition %s -> %s",
			kind, from, to)
	}

	return nil
}

// IsRoutableStatus reports whether lnd may expose and route over the channel.
func IsRoutableStatus(status Status) bool {
	switch status {
	case StatusActive, StatusFundingPublished:
		return true

	default:
		return false
	}
}

// HasArmedBacking reports whether the exact fully signed VTXO-to-channel
// transaction is durable. These states must retain ownership of the backing
// VTXO even while the channel is materialized or closed.
func HasArmedBacking(status Status) bool {
	switch status {
	case StatusBackingArmed, StatusRoundConfirmed, StatusActive,
		StatusFundingPublished, StatusMaterializing, StatusClosing,
		StatusClosed:
		return true

	default:
		return false
	}
}

// ValidateInitialBalances enforces role-relative channel ownership. Receive
// channels must start entirely operator-owned; the client earns balance only
// by settling a Lightning payment after activation.
func ValidateInitialBalances(kind IntentKind, role Role, capacity, local,
	remote btcutil.Amount) error {

	if capacity <= 0 {
		return fmt.Errorf("channel capacity must be positive")
	}
	if capacity > btcutil.MaxSatoshi {
		return fmt.Errorf("channel capacity exceeds Bitcoin money " +
			"supply")
	}
	if local < 0 || remote < 0 {
		return fmt.Errorf("channel balances must be non-negative")
	}
	if local > capacity || remote > capacity-local {
		return fmt.Errorf("channel balances exceed capacity")
	}
	if kind != KindReceiveChannel {
		return nil
	}

	switch role {
	case RoleOperator:
		if local != capacity || remote != 0 {
			return fmt.Errorf("receive channel must start fully " +
				"owned by the operator")
		}

	case RoleClient:
		if local != 0 || remote != capacity {
			return fmt.Errorf("receive channel must start fully " +
				"owned by the operator")
		}

	default:
		return fmt.Errorf("unknown virtual channel role %q", role)
	}

	return nil
}

// BackingVTXO records one existing VTXO consumed by the backing transaction.
type BackingVTXO struct {
	OutPoint       wire.OutPoint
	Amount         btcutil.Amount
	PkScript       []byte
	PolicyTemplate []byte
}

// Registration is the durable material required to activate or materialize a
// virtual channel.
type Registration struct {
	ID               ID
	Kind             IntentKind
	RoundID          string
	StateVersion     uint64
	PendingChannelID PendingChannelID
	ChannelPoint     wire.OutPoint
	RemoteNodePubKey NodePubKey
	Role             Role
	Status           Status
	Capacity         btcutil.Amount
	LocalBalance     btcutil.Amount
	RemoteBalance    btcutil.Amount
	BackingTx        *wire.MsgTx
	FundingPsbt      []byte
	BackingVTXOs     []BackingVTXO
}

// PendingOpen is the durable pre-lnd-open intent used by the inbound acceptor
// before the final funding parent and channel point are known.
type PendingOpen struct {
	Kind             IntentKind
	RequestKey       string
	RoundID          string
	StateVersion     uint64
	PendingChannelID PendingChannelID
	RemoteNodePubKey NodePubKey
	Role             Role
	Status           Status
	Capacity         btcutil.Amount
	LocalBalance     btcutil.Amount
	RemoteBalance    btcutil.Amount
	BackingVTXOs     []BackingVTXO
}

// Channel is a persisted virtual channel registration with lifecycle
// timestamps.
type Channel struct {
	Registration

	CreatedAt      time.Time
	UpdatedAt      time.Time
	MaterializedAt time.Time
	ClosedAt       time.Time
	CloseTx        *wire.MsgTx
}
