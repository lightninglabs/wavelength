package chainresolver

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
)

// buildBatchSpendWatch constructs a RegisterSpendWatchOutMsg for watching
// the batch outpoint of a VTXO. This is used in the fraud-reactive path
// where the resolver waits for the batch outpoint to be spent by another
// party before beginning the unroll process.
func buildBatchSpendWatch(resolverID wire.OutPoint,
	batchOutpoint wire.OutPoint,
	batchPkScript []byte,
	heightHint uint32) *RegisterSpendWatchOutMsg {

	callerID := fmt.Sprintf(
		"resolver.%s.batch-spend", resolverID.String(),
	)

	return &RegisterSpendWatchOutMsg{
		Outpoint:   batchOutpoint,
		PkScript:   batchPkScript,
		HeightHint: heightHint,
		CallerID:   callerID,
	}
}
