package unroll

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
)

// RegistryMsg is the sealed message surface accepted by the unroll registry.
type RegistryMsg interface {
	actor.Message

	registryMsgSealed()
}

// RegistryResp is the sealed response surface returned by the unroll
// registry.
type RegistryResp interface {
	actor.Message

	registryRespSealed()
}

// EnsureUnrollRequest asks the registry to ensure one target has a running
// unroll actor.
type EnsureUnrollRequest struct {
	actor.BaseMessage

	// Outpoint identifies the target VTXO to unroll.
	Outpoint wire.OutPoint

	// Trigger identifies why the unroll was requested.
	Trigger StartTrigger
}

// MessageType returns the stable message type identifier.
func (m *EnsureUnrollRequest) MessageType() string {
	return "EnsureUnrollRequest"
}

// registryMsgSealed seals EnsureUnrollRequest into the registry surface.
func (m *EnsureUnrollRequest) registryMsgSealed() {}

// EnsureUnrollResp acknowledges an EnsureUnrollRequest.
type EnsureUnrollResp struct {
	actor.BaseMessage

	// ActorID is the spawned or existing per-target actor ID.
	ActorID string

	// Created reports whether this request created a new running actor.
	Created bool
}

// MessageType returns the stable message type identifier.
func (m *EnsureUnrollResp) MessageType() string {
	return "EnsureUnrollResp"
}

// registryRespSealed seals EnsureUnrollResp into the registry surface.
func (m *EnsureUnrollResp) registryRespSealed() {}

// GetStatusRequest asks the registry for one target's current status.
type GetStatusRequest struct {
	actor.BaseMessage

	// Outpoint identifies the target VTXO.
	Outpoint wire.OutPoint
}

// MessageType returns the stable message type identifier.
func (m *GetStatusRequest) MessageType() string {
	return "GetStatusRequest"
}

// registryMsgSealed seals GetStatusRequest into the registry surface.
func (m *GetStatusRequest) registryMsgSealed() {}

// GetStatusResp reports one target's current status.
type GetStatusResp struct {
	actor.BaseMessage

	// Found reports whether the target exists in the registry view.
	Found bool

	// Active reports whether the target currently has a running actor.
	Active bool

	// ActorID is the local actor ID when known.
	ActorID string

	// State is the detailed child state when an active actor was queried.
	State *GetStateResp

	// Phase is the last known coarse phase when no active child was
	// queried.
	Phase Phase

	// Trigger is the original start trigger when known.
	Trigger StartTrigger

	// FailReason is the last known terminal failure when present.
	FailReason string

	// SweepTxid is the last known sweep txid when present.
	SweepTxid *chainhash.Hash
}

// MessageType returns the stable message type identifier.
func (m *GetStatusResp) MessageType() string {
	return "GetStatusResp"
}

// registryRespSealed seals GetStatusResp into the registry surface.
func (m *GetStatusResp) registryRespSealed() {}

// UnrollTerminatedMsg notifies the registry that one child actor reached a
// terminal state.
type UnrollTerminatedMsg struct {
	actor.BaseMessage

	// Outpoint identifies the target VTXO.
	Outpoint wire.OutPoint

	// ActorID identifies the child actor instance.
	ActorID string

	// Phase is the terminal phase reached by the actor.
	Phase Phase

	// FailReason is populated for terminal failures.
	FailReason string

	// SweepTxid is populated when the actor built a sweep transaction.
	SweepTxid *chainhash.Hash
}

// MessageType returns the stable message type identifier.
func (m *UnrollTerminatedMsg) MessageType() string {
	return "UnrollTerminatedMsg"
}

// registryMsgSealed seals UnrollTerminatedMsg into the registry surface.
func (m *UnrollTerminatedMsg) registryMsgSealed() {}

// RegistryAckResp is a generic acknowledgement response.
type RegistryAckResp struct {
	actor.BaseMessage
}

// MessageType returns the stable message type identifier.
func (m *RegistryAckResp) MessageType() string {
	return "RegistryAckResp"
}

// registryRespSealed seals RegistryAckResp into the registry surface.
func (m *RegistryAckResp) registryRespSealed() {}
