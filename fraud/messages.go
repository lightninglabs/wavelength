package fraud

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/vtxo"
)

// Msg is the sealed fraud watcher message surface.
type Msg interface {
	actor.Message

	fraudMsgSealed()
}

// Resp is the sealed fraud watcher response surface.
type Resp interface {
	actor.Message

	fraudRespSealed()
}

// TrackVTXOsRequest asks the watcher to arm passive watches for descriptors.
type TrackVTXOsRequest struct {
	actor.BaseMessage

	// VTXOs are locally materialized descriptors to consider for tracking.
	VTXOs []*vtxo.Descriptor
}

// MessageType returns the stable actor message type.
func (m *TrackVTXOsRequest) MessageType() string {
	return "TrackVTXOsRequest"
}

// fraudMsgSealed seals TrackVTXOsRequest into Msg.
func (m *TrackVTXOsRequest) fraudMsgSealed() {}

// TrackVTXOsResp summarizes watcher admission.
type TrackVTXOsResp struct {
	actor.BaseMessage

	// Tracked is the number of descriptors that resulted in active watches.
	Tracked int
}

// MessageType returns the stable actor message type.
func (m *TrackVTXOsResp) MessageType() string {
	return "TrackVTXOsResp"
}

// fraudRespSealed seals TrackVTXOsResp into Resp.
func (m *TrackVTXOsResp) fraudRespSealed() {}

// UntrackRequest asks the watcher to release one target's passive watches.
type UntrackRequest struct {
	actor.BaseMessage

	// TargetOutpoint identifies the no-longer-live target VTXO.
	TargetOutpoint wire.OutPoint
}

// MessageType returns the stable actor message type.
func (m *UntrackRequest) MessageType() string {
	return "UntrackRequest"
}

// fraudMsgSealed seals UntrackRequest into Msg.
func (m *UntrackRequest) fraudMsgSealed() {}

// UntrackResp acknowledges an untrack request.
type UntrackResp struct {
	actor.BaseMessage

	// Removed reports whether an active target was removed.
	Removed bool
}

// MessageType returns the stable actor message type.
func (m *UntrackResp) MessageType() string {
	return "UntrackResp"
}

// fraudRespSealed seals UntrackResp into Resp.
func (m *UntrackResp) fraudRespSealed() {}

// SpendObservedMsg reports a spend of one watched ancestor outpoint.
type SpendObservedMsg struct {
	actor.BaseMessage

	// Outpoint is the watched outpoint that was spent.
	Outpoint wire.OutPoint

	// SpendingTxid is the transaction that spent Outpoint.
	SpendingTxid chainhash.Hash

	// SpendingTx is the confirmed transaction that spent Outpoint. The
	// watcher uses its revealed tapleaf to distinguish the operator's
	// committed expiry sweep from ancestry materialization fraud.
	SpendingTx *wire.MsgTx

	// SpenderInputIndex identifies the input in SpendingTx that consumed
	// Outpoint.
	SpenderInputIndex uint32

	// Height is the confirmation height of SpendingTxid.
	Height int32
}

// MessageType returns the stable actor message type.
func (m *SpendObservedMsg) MessageType() string {
	return "SpendObservedMsg"
}

// fraudMsgSealed seals SpendObservedMsg into Msg.
func (m *SpendObservedMsg) fraudMsgSealed() {}

// AckResp is a generic acknowledgement.
type AckResp struct {
	actor.BaseMessage
}

// MessageType returns the stable actor message type.
func (m *AckResp) MessageType() string {
	return "AckResp"
}

// fraudRespSealed seals AckResp into Resp.
func (m *AckResp) fraudRespSealed() {}
