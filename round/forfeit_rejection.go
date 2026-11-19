package round

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// handleForfeitSignatureRejected routes a local refusal only to the attempt
// that requested it. The collecting state owns validation and safe rollback;
// other states ignore it, especially after the signature checkpoint.
func (a *RoundClientActor) handleForfeitSignatureRejected(ctx context.Context,
	msg *ForfeitSignatureRejected) fn.Result[actormsg.RoundActorResp] {

	if id, err := ParseRoundID(msg.RoundID); err == nil &&
		a.isAbandonedAttempt(id) {
		return fn.Ok[actormsg.RoundActorResp](nil)
	}

	fsm, exists := a.rounds[RoundKeyStr(msg.RoundID)]
	if !exists {
		return fn.Ok[actormsg.RoundActorResp](nil)
	}

	if err := a.askEventAndProcessOutbox(ctx, fsm, msg); err != nil {
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("process forfeit rejection: %w", err),
		)
	}

	return fn.Ok[actormsg.RoundActorResp](nil)
}
