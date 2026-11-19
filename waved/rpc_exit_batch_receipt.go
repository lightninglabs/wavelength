package waved

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BeginExitReceipt claims the new request before wallet selection or
// reservation.
func (r *RPCServer) BeginExitReceipt(ctx context.Context, id string) error {
	if err := r.requireWalletReady(); err != nil {
		return err
	}
	if r.server.durableRoundActor == nil || r.server.roundStore == nil {
		return status.Error(
			codes.FailedPrecondition,
			"keyed exits require the durable round runtime",
		)
	}
	err := r.server.roundStore.BeginExitBatch(ctx, id)
	if errors.Is(err, db.ErrExitBatchIDUsed) {
		return status.Error(
			codes.AlreadyExists, "exit request ID already used",
		)
	}

	return err
}

// LeaveWithReceipt records the accepted subset exactly when its composed intent
// commits in the round state machine. Volatile runtimes cannot offer this.
func (r *RPCServer) LeaveWithReceipt(ctx context.Context,
	req *waverpc.LeaveVTXOsRequest, id string,
	encode func(map[string]string) ([]byte, error)) (
	*waverpc.LeaveVTXOsResponse, error) {

	store := r.server.roundStore
	ref := r.server.durableRoundActor.Ref()

	return r.leaveVTXOs(ctx, req, func(ctx context.Context,
		msg *actormsg.RegisterIntentMsg,
		result *wallet.LeaveVTXOsResponse) (bool, error) {

		failures := make(map[string]string, len(result.Errors))
		for op, reason := range result.Errors {
			failures[op.String()] = reason.Error()
		}
		receipt, err := encode(failures)
		if err != nil {
			return true, err
		}
		msg.ExitRequestID, msg.ExitReceipt = id, receipt
		err = ref.Ask(ctx, msg).Await(ctx).Err()
		if err == nil {
			return false, nil
		}
		// An interrupted wait can hide a committed round turn. Only
		// release reservations after checking the authoritative
		// decision.
		checkCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 10*time.Second,
		)
		defer cancel()
		found, checkErr := store.ResolveExitBatch(checkCtx, id)
		if checkErr != nil {
			return false, errors.Join(err, checkErr)
		}
		// Only this submit could fill the receipt.
		if bytes.Equal(found, receipt) {
			return false, nil
		}

		return true, err
	})
}

// ResolveExitReceipt seals an unused ID or returns its committed receipt.
func (r *RPCServer) ResolveExitReceipt(ctx context.Context, id string) ([]byte,
	error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if r.server.roundStore == nil {
		return nil, status.Error(
			codes.Unavailable, "round store unavailable",
		)
	}

	return r.server.roundStore.ResolveExitBatch(ctx, id)
}
