package virtualchannel

import (
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ID is the stable darepo identifier for a virtual channel registration.
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

// Status describes the persisted virtual channel lifecycle.
type Status string

const (
	// StatusNegotiating means funding negotiation has not activated in lnd.
	StatusNegotiating Status = "negotiating"

	// StatusActive means lnd can route over the channel without
	// broadcasting.
	StatusActive Status = "active"

	// StatusMaterializing means darepo is publishing the backing parent.
	StatusMaterializing Status = "materializing"

	// StatusClosing means lnd or darepo is resolving the channel close.
	StatusClosing Status = "closing"

	// StatusClosed means the channel reached a terminal close state.
	StatusClosed Status = "closed"

	// StatusFailed means negotiation or materialization reached a terminal
	// failure.
	StatusFailed Status = "failed"
)

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
