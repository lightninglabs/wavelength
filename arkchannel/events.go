package arkchannel

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

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

// RecoveryPackageInstalled records that the complete source ancestry and OOR
// package chain are installed and watched at both channel endpoints.
type RecoveryPackageInstalled struct{}

func (*RecoveryPackageInstalled) eventSealed() {}

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

// SourceSpent records confirmed chain evidence that one of the channel
// source's on-path ancestors was spent.
type SourceSpent struct {
	OutPoint     wire.OutPoint
	SpendingTxID chainhash.Hash
}

func (*SourceSpent) eventSealed() {}

// BackingPublished records publication of the expected backing transaction.
type BackingPublished struct {
	TxID chainhash.Hash
}

func (*BackingPublished) eventSealed() {}

// BackingObserved records independent chain evidence for the expected backing
// transaction. It lets lnd resolution supersede an unfinished off-chain close
// without accepting a peer's claim as proof of publication.
type BackingObserved struct {
	TxID chainhash.Hash
}

func (*BackingObserved) eventSealed() {}

// ChannelClosed records completion of lnd's channel lifecycle.
type ChannelClosed struct{}

func (*ChannelClosed) eventSealed() {}

// RequestCooperativeClose fixes the replacement VTXO owners before either lnd
// endpoint stops accepting new HTLCs.
type RequestCooperativeClose struct {
	Request CooperativeCloseRequest
}

func (*RequestCooperativeClose) eventSealed() {}

// CooperativeCloseSigned records the hub-authorized 3-of-3 OOR close and
// acknowledges which endpoint durably stored it. Both acknowledgements are
// required before ordinary OOR submission becomes replayable. The historical
// type name is retained for durable compatibility.
type CooperativeCloseSigned struct {
	Close CooperativeClose
	Party Party
}

func (*CooperativeCloseSigned) eventSealed() {}

// CooperativeClosePublished records finalization of the exact ordinary OOR
// close. The historical type name is retained for durable compatibility.
type CooperativeClosePublished struct {
	TxID chainhash.Hash
}

func (*CooperativeClosePublished) eventSealed() {}

// CooperativeCloseFinalized records that one endpoint archived its lnd
// channel after the OOR close finalized.
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

// PrepareRecovery installs the finalized source package at both endpoints
// before either native lnd channel may activate.
type PrepareRecovery struct {
	Terms  Terms
	Source VTXOBinding
}

func (*PrepareRecovery) actionSealed() {}

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

// PublishChannel first hands the channel to lnd's on-chain lifecycle, then
// publishes ancestry before the backing from either recovery-ready endpoint.
type PublishChannel struct {
	Terms   Terms
	Source  VTXOBinding
	Backing Backing
}

func (*PublishChannel) actionSealed() {}

// ForceCloseChannel asks native lnd to publish its latest commitment after
// the backing transaction is on chain.
type ForceCloseChannel struct {
	Backing Backing
}

func (*ForceCloseChannel) actionSealed() {}

// NegotiateCooperativeClose quiesces both lnd endpoints and records the hub's
// signature for an immediate 3-of-3 OOR checkpoint spend.
type NegotiateCooperativeClose struct {
	Terms   Terms
	Source  VTXOBinding
	Backing Backing
	Request CooperativeCloseRequest
}

func (*NegotiateCooperativeClose) actionSealed() {}

// PublishCooperativeClose asks the client-owned ordinary OOR actor to settle
// the authorized 3-of-3 close. The historical action name is retained for
// durable compatibility.
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
