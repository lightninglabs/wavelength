package vtxo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// startTipEvent carries the chain source's answer to the best-height ask a
// VTXO actor sends itself on start. It never reaches the FSM: Receive turns
// a successful answer into an ordinary BlockEpochEvent and drops a failure,
// or an answer no newer than a block the actor has already seen.
type startTipEvent struct {
	actor.BaseMessage

	tip fn.Result[chainsource.ChainSourceResp]
}

// MessageType returns the message type identifier for logging and debugging.
func (*startTipEvent) MessageType() string {
	return "StartTipEvent"
}

// VTXOActorMsg marks startTipEvent as a VTXO actor message.
func (*startTipEvent) VTXOActorMsg() {}

// startTipEpoch maps the answer to the start ask onto the epoch the actor
// would otherwise have waited a block for. A failure is logged, not
// returned: the actor is already running and its block subscription still
// drives expiry, so the only cost is classifying it one block later.
func (a *VTXOActor) startTipEpoch(ctx context.Context, msg *startTipEvent) (
	*BlockEpochEvent, bool) {

	// The tip exists to move an expired coin out of Live, so the wallet
	// stops counting it as spendable. Any other state already keeps the
	// coin out of the spendable set, and there an epoch does more than
	// classify: an expired actor starts its reclaim on it, and a reserved
	// one can escalate to an exit. None of that should run the moment
	// an actor is spawned, so those states keep waiting for a block.
	if _, live := a.state.(*LiveState); !live {
		a.logger(ctx).DebugS(ctx, "Ignoring tip on start outside live",
			slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
			slog.String("state", fmt.Sprintf("%T", a.state)),
		)

		return nil, false
	}

	response, err := msg.tip.Unpack()
	if err != nil {
		a.logger(ctx).WarnS(ctx, "Could not apply current tip on start",
			err,
			slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
		)

		return nil, false
	}

	tip, ok := response.(*chainsource.BestHeightResponse)
	if !ok {
		a.logger(ctx).WarnS(ctx, "Unexpected best height response",
			nil,
			slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
			slog.Any("response", response),
		)

		return nil, false
	}

	// The ask and the block subscription are separate delivery paths, so
	// a block can overtake the answer. Applying a tip the actor has
	// already moved past would walk it back, for instance from Expired to
	// Live, and persist that until the next block. A tip at a height it
	// has already seen adds nothing either. Only this answer is fenced:
	// a real block below the last one is a reorg and must still reach
	// the FSM.
	if tip.Height <= a.lastEpochHeight {
		a.logger(ctx).DebugS(ctx, "Dropping stale tip on start",
			slog.String("outpoint", a.cfg.VTXO.Outpoint.String()),
			slog.Int("tip_height", int(tip.Height)),
			slog.Int("seen_height", int(a.lastEpochHeight)),
		)

		return nil, false
	}

	return &BlockEpochEvent{
		Height: tip.Height,
		Hash:   tip.Hash,
	}, true
}
