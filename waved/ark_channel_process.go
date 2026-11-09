package waved

import (
	"context"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/unrollbridge"
	"github.com/lightninglabs/wavelength/lnruntime"
)

// ArkChannelRecoveryController is the complete endpoint-local source archive,
// watcher, and unroll preparation boundary.
type ArkChannelRecoveryController interface {
	lnruntime.ChannelRecoveryManager
	arkchannel.ChannelEventSinkBinder
	unrollbridge.SourcePreparer

	RestoreWatches(context.Context, []arkchannel.Record) error

	Stop()
}
