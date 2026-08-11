package arkchannel

import "github.com/btcsuite/btcd/chainhash/v2"

// Event is a closed set of durable Ark-channel facts.
type Event interface {
	eventSealed()
}

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

// FundingCanceled records that lnd removed the abandoned pending channel.
type FundingCanceled struct{}

func (*FundingCanceled) eventSealed() {}

// OORFinalized records that the prepared OOR transfer completed at the Ark
// operator and the channel-policy VTXO now exists.
type OORFinalized struct {
	SessionID [32]byte
}

func (*OORFinalized) eventSealed() {}

// OORAborted records a definitive pre-PONR OOR failure. It proves the source
// reservation is safe to release and permits the pending lnd channel to be
// canceled.
type OORAborted struct {
	SessionID [32]byte
	Reason    string
}

func (*OORAborted) eventSealed() {}

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

// RequestCooperativeClose fixes the direct-settlement scripts and fee rate
// before either lnd endpoint stops accepting new HTLCs.
type RequestCooperativeClose struct {
	Request CooperativeCloseRequest
}

func (*RequestCooperativeClose) eventSealed() {}

// CooperativeCloseSigned records the fully signed direct spend of the
// channel-policy VTXO and acknowledges which endpoint durably stored it. Both
// acknowledgements are required before publication becomes replayable.
type CooperativeCloseSigned struct {
	Close CooperativeClose
	Party Party
}

func (*CooperativeCloseSigned) eventSealed() {}

// CooperativeClosePublished records confirmation of the exact direct VTXO
// settlement through the common unroller.
type CooperativeClosePublished struct {
	TxID chainhash.Hash
}

func (*CooperativeClosePublished) eventSealed() {}

// CooperativeCloseFinalized records that one endpoint archived its lnd
// channel after the direct settlement confirmed.
type CooperativeCloseFinalized struct {
	Party Party
}

func (*CooperativeCloseFinalized) eventSealed() {}

// CooperativeCloseAborted returns a pre-signature close attempt to the active
// phase and permits both lnd links to resume traffic.
type CooperativeCloseAborted struct{}

func (*CooperativeCloseAborted) eventSealed() {}

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

// CommitOOR releases signing and transport for the prepared transfer only
// after both lnd endpoints have durably stored their initial commitments.
type CommitOOR struct {
	Terms  Terms
	Source VTXOBinding
}

func (*CommitOOR) actionSealed() {}

// AbortOOR releases the source VTXO reservation before canceling native lnd
// funding.
type AbortOOR struct {
	Terms  Terms
	Source VTXOBinding
	Reason string
}

func (*AbortOOR) actionSealed() {}

// ActivateChannel asks the virtual notifier to confirm the backing to lnd.
type ActivateChannel struct {
	Terms   Terms
	Backing Backing
}

func (*ActivateChannel) actionSealed() {}

// CancelFunding asks the native funding adapter to remove an abandoned lnd
// reservation before the Ark workflow becomes terminal.
type CancelFunding struct {
	Terms   Terms
	Backing *Backing
}

func (*CancelFunding) actionSealed() {}

// PublishChannel asks the unroller to publish ancestry before the backing.
type PublishChannel struct {
	Terms   Terms
	Source  VTXOBinding
	Backing Backing
}

func (*PublishChannel) actionSealed() {}

// NegotiateCooperativeClose quiesces both lnd endpoints and collects the three
// signatures for an immediate channel-policy VTXO spend.
type NegotiateCooperativeClose struct {
	Terms   Terms
	Source  VTXOBinding
	Backing Backing
	Request CooperativeCloseRequest
}

func (*NegotiateCooperativeClose) actionSealed() {}

// PublishCooperativeClose asks the unroller to publish ancestry followed by
// the already signed direct VTXO settlement.
type PublishCooperativeClose struct {
	Terms  Terms
	Source VTXOBinding
	Close  CooperativeClose
}

func (*PublishCooperativeClose) actionSealed() {}

// FinalizeCooperativeClose archives the local lnd channel after the direct
// settlement confirmed.
type FinalizeCooperativeClose struct {
	Terms   Terms
	Source  VTXOBinding
	Backing Backing
	Request CooperativeCloseRequest
	Close   CooperativeClose
}

func (*FinalizeCooperativeClose) actionSealed() {}
