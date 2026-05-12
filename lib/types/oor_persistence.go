package types

import (
	"errors"
	"fmt"
)

// OORPackageDirection encodes transfer direction from this wallet's
// perspective.
type OORPackageDirection int32

const (
	// OORPackageDirectionIncoming marks a package received by this wallet.
	OORPackageDirectionIncoming OORPackageDirection = 0

	// OORPackageDirectionOutgoing marks a package sent by this wallet.
	OORPackageDirectionOutgoing OORPackageDirection = 1
)

// OORPackageLinkKind encodes local outpoint relation to a package.
type OORPackageLinkKind int32

const (
	// OORPackageLinkKindCreatedOutput marks bindings where local outpoints
	// are created by the package Ark transaction.
	OORPackageLinkKindCreatedOutput OORPackageLinkKind = 0

	// OORPackageLinkKindConsumedInput marks bindings where local outpoints
	// are consumed as inputs by the package Ark transaction.
	OORPackageLinkKindConsumedInput OORPackageLinkKind = 1
)

var (
	// ErrOORBindingOutpointNotFound indicates that a binding references an
	// outpoint that is not present in the local VTXO store.
	ErrOORBindingOutpointNotFound = errors.New("oor binding outpoint not " +
		"found in local vtxo store")

	// ErrOORPackageDirectionConflict indicates that a session package was
	// already persisted with a different wallet-relative direction.
	ErrOORPackageDirectionConflict = errors.New("oor package direction " +
		"conflict")
)

// String returns the stable direction label used in logs/errors.
func (d OORPackageDirection) String() string {
	switch d {
	case OORPackageDirectionIncoming:
		return "incoming"

	case OORPackageDirectionOutgoing:
		return "outgoing"

	default:
		return fmt.Sprintf("unknown(%d)", d)
	}
}

// String returns the stable link-kind label used in logs/errors.
func (k OORPackageLinkKind) String() string {
	switch k {
	case OORPackageLinkKindCreatedOutput:
		return "created_output"

	case OORPackageLinkKindConsumedInput:
		return "consumed_input"

	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}
