package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/batchcanon"
)

// roundBatchRegistrar adapts the batch-canonicality actor's request/response
// API to the narrow synchronous seam used by the round FSM.
type roundBatchRegistrar struct {
	ref actor.ActorRef[batchcanon.ManagerMsg, batchcanon.ManagerResp]
}

// RegisterBatch waits until the complete registration is durable and its
// watches are armed. Returning before that point would let the round expose a
// VTXO whose lineage is still unknown to the fail-closed admission gate.
func (r *roundBatchRegistrar) RegisterBatch(ctx context.Context,
	req *batchcanon.RegisterBatchRequest) error {

	resp, err := r.ref.Ask(ctx, req).Await(ctx).Unpack()
	if err != nil {
		return err
	}
	if _, ok := resp.(*batchcanon.RegisterBatchResponse); !ok {
		return fmt.Errorf("unexpected batch registration response %T",
			resp)
	}

	return nil
}
