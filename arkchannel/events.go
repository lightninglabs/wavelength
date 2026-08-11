package arkchannel

import "github.com/btcsuite/btcd/chainhash/v2"

// Event is a closed set of durable Ark-channel facts.
type Event interface {
	eventSealed()
}

// BindVTXO records the exact output validated by a round or wallet lookup.
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

// FundingCanceled records that lnd removed the abandoned pending channel.
type FundingCanceled struct{}

func (*FundingCanceled) eventSealed() {}

// RoundCommitted records that a receive round crossed nonce release.
type RoundCommitted struct {
	RoundID        string
	CommitmentTxID chainhash.Hash
}

func (*RoundCommitted) eventSealed() {}

// RoundConfirmed records confirmation of the bound receive round.
type RoundConfirmed struct {
	RoundID        string
	CommitmentTxID chainhash.Hash
}

func (*RoundConfirmed) eventSealed() {}

// ChannelActive records that native lnd activated the expected channel point.
type ChannelActive struct {
	ChannelPointHash  chainhash.Hash
	ChannelPointIndex uint32
}

func (*ChannelActive) eventSealed() {}

// Materialize requests publication of the VTXO ancestry and backing.
type Materialize struct{}

func (*Materialize) eventSealed() {}

// BackingPublished records publication of the expected backing transaction.
type BackingPublished struct {
	TxID chainhash.Hash
}

func (*BackingPublished) eventSealed() {}

// ChannelClosed records completion of lnd's channel lifecycle.
type ChannelClosed struct{}

func (*ChannelClosed) eventSealed() {}

// Fail abandons negotiation before the signed-backing safety boundary.
type Fail struct {
	Reason string
}

func (*Fail) eventSealed() {}

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

// ActivateChannel asks the virtual notifier to confirm the backing to lnd.
type ActivateChannel struct {
	Terms   Terms
	Backing Backing
}

func (*ActivateChannel) actionSealed() {}

// CancelFunding asks the native funding adapter to remove an abandoned lnd
// reservation before the Ark workflow becomes terminal.
type CancelFunding struct {
	Terms Terms
}

func (*CancelFunding) actionSealed() {}

// PublishChannel asks the unroller to publish ancestry before the backing.
type PublishChannel struct {
	Terms   Terms
	Source  VTXOBinding
	Backing Backing
}

func (*PublishChannel) actionSealed() {}
