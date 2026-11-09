package arkchannel

import (
	"github.com/btcsuite/btcd/chainhash/v2"
)

// Event is a closed set of durable Ark-channel facts.
type Event interface {
	eventSealed()
}

// OORPreparationStarted records that the funder may now reserve wallet VTXOs
// for this channel. Persisting this fact before selection makes an interrupted
// preparation discoverable without funding an unpaid receive intent.
type OORPreparationStarted struct{}

func (*OORPreparationStarted) eventSealed() {}

// BindVTXO records the exact output of a durable prepared OOR transfer.
type BindVTXO struct {
	Binding VTXOBinding
}

func (*BindVTXO) eventSealed() {}

// FundingFinalized records that one endpoint's native lnd funding manager has
// persisted both initial commitments.
type FundingFinalized struct {
	Party Party
}

func (*FundingFinalized) eventSealed() {}

// BackingSigned records the fully signed VTXO-to-channel transaction.
type BackingSigned struct {
	Backing Backing
}

func (*BackingSigned) eventSealed() {}

// OORFinalized records that the prepared OOR transfer completed at the Ark
// operator and the channel-policy VTXO now exists.
type OORFinalized struct {
	SessionID [32]byte
}

func (*OORFinalized) eventSealed() {}

// RecoveryPackageInstalled records that one endpoint installed and watches the
// complete source ancestry and OOR package chain.
type RecoveryPackageInstalled struct {
	Party Party
}

func (*RecoveryPackageInstalled) eventSealed() {}

// ChannelActive records that native lnd activated the expected channel point.
type ChannelActive struct {
	ChannelPointHash  chainhash.Hash
	ChannelPointIndex uint32
}

func (*ChannelActive) eventSealed() {}

// Action is side effect requested after its producing state is durable.
type Action interface {
	actionSealed()
}

// NegotiateFunding asks native lnd to negotiate against the bound VTXO.
type NegotiateFunding struct {
	Terms  Terms
	Source VTXOBinding
}

func (*NegotiateFunding) actionSealed() {}

// CommitOOR releases signing and transport for the prepared transfer only
// after both lnd endpoints have durably stored their initial commitments.
type CommitOOR struct {
	Terms  Terms
	Source VTXOBinding
}

func (*CommitOOR) actionSealed() {}

// PrepareRecovery installs the finalized source package at both endpoints
// before either native lnd channel may activate.
type PrepareRecovery struct {
	Terms  Terms
	Source VTXOBinding
}

func (*PrepareRecovery) actionSealed() {}

// ActivateChannel asks the virtual notifier to confirm the backing to lnd.
type ActivateChannel struct {
	Terms   Terms
	Backing Backing
}

func (*ActivateChannel) actionSealed() {}
