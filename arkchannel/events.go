package arkchannel

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
