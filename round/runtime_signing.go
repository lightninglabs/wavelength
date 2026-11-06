package round

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
)

// interruptedSigningRound identifies commands whose internal transition chain
// can enter private MuSig2 work. Persisting its identity before that chain
// closes the first-call window, when the saved state does not yet contain
// sessions.
func (a *RoundClientActor) interruptedSigningRound(ctx context.Context,
	message actor.TLVMessage) ([]byte, error) {

	push, ok := message.(*ServerMessageNotification)
	if !ok {
		return nil, nil
	}
	if len(push.wireMethod) != 0 {
		if err := push.restoreWireEvent(
			a.cfg.MaxTreeNodes,
		); err != nil {
			return nil, err
		}
	}
	var id RoundID
	switch event := push.Message.(type) {
	case *CommitmentTxBuilt:
		id = event.RoundID

	case *NoncesAggregated:
		id = event.RoundID

	default:
		return nil, nil
	}
	if id == (RoundID{}) {
		return nil, fmt.Errorf("signing command has no round identity")
	}

	round := a.rounds[RoundKeyStr(id.KeyString())]
	if round == nil || round.coldSigning {
		return nil, nil
	}
	state, err := fsmState(ctx, round.FSM)
	if err != nil {
		return nil, err
	}
	switch push.Message.(type) {
	case *CommitmentTxBuilt:
		if _, ok := state.(*RoundJoinedState); !ok {
			return nil, nil
		}

	case *NoncesAggregated:
		if _, ok := state.(*NoncesSentState); !ok {
			return nil, nil
		}
	}

	return append([]byte(nil), id[:]...), nil
}

// markInterruptedSigning fences exactly the attempt staged before the signer
// call. Other rounds retain their original recovery classification.
func markInterruptedSigning(snapshot *clientActorSnapshot, raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	id, err := parseDurableRoundID(raw)
	if err != nil {
		return err
	}
	restored := snapshot.rounds[RoundKeyStr(id.KeyString())]
	if restored == nil {
		return fmt.Errorf("interrupted signing round is missing")
	}
	restored.lostSignerSessions = true

	return nil
}
