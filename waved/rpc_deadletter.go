package waved

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultDeadLetterListLimit caps a listing when the request leaves
	// the limit unset.
	defaultDeadLetterListLimit = 100

	// maxDeadLetterListLimit is the server-side ceiling on a single
	// listing, so a request cannot ask the daemon to materialize the
	// whole queue (payloads included) in one response.
	maxDeadLetterListLimit = 1000

	// maxPurgeAgeSeconds bounds the purge age so the seconds-to-Duration
	// conversion cannot overflow. Overflow matters: an operator passing
	// an epoch timestamp instead of an age would wrap the duration
	// negative, push the cutoff into the future, and delete the entire
	// queue, which is exactly what the positive-age requirement exists
	// to prevent.
	maxPurgeAgeSeconds = math.MaxInt64 / int64(time.Second)
)

// requireDeadLetterStore returns the delivery store's operator-facing
// dead-letter surface, or an Unavailable status while the store is not yet
// initialized (the RPC server starts before the database on some paths).
func (r *RPCServer) requireDeadLetterStore() (actor.DeadLetterStore, error) {
	dlStore, ok := r.server.deliveryStore.(actor.DeadLetterStore)
	if !ok {
		return nil, status.Errorf(codes.Unavailable, "dead-letter "+
			"store not ready")
	}

	return dlStore, nil
}

// deadLetterToProto converts a store dead letter to its RPC representation.
// The payload is attached only when includePayload is set, keeping list
// responses lean by default.
func deadLetterToProto(dl *actor.DeadLetter,
	includePayload bool) *waverpc.DeadLetter {

	msg := &waverpc.DeadLetter{
		Id:              dl.ID,
		Source:          dl.Source,
		ActorId:         dl.ActorID,
		MessageType:     dl.MessageType,
		FailureReason:   dl.FailureReason,
		Attempts:        int32(dl.Attempts),
		CreatedAt:       dl.CreatedAt.Unix(),
		Priority:        int32(dl.Priority),
		MaxAttempts:     int32(dl.MaxAttempts),
		PromiseId:       dl.PromiseID,
		CallbackActorId: dl.CallbackActorID,
		CorrelationId:   dl.CorrelationID,
		CorrelationKey:  dl.CorrelationKey,
	}

	if includePayload {
		msg.Payload = append([]byte(nil), dl.Payload...)
	}

	return msg
}

// ListDeadLetters returns dead-lettered actor messages for operator
// inspection, newest first.
func (r *RPCServer) ListDeadLetters(ctx context.Context,
	req *waverpc.ListDeadLettersRequest) (*waverpc.ListDeadLettersResponse,
	error) {

	dlStore, err := r.requireDeadLetterStore()
	if err != nil {
		return nil, err
	}

	limit := int(req.Limit)
	if limit <= 0 {
		limit = defaultDeadLetterListLimit
	}
	if limit > maxDeadLetterListLimit {
		limit = maxDeadLetterListLimit
	}
	if req.Offset < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "offset "+
			"must not be negative")
	}

	// The per-actor listing has no offset pagination; reject the
	// combination rather than silently ignoring the offset.
	if req.ActorId != "" && req.Offset > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "offset is "+
			"only supported on the global (unfiltered) listing")
	}

	var entries []actor.DeadLetter
	if req.ActorId != "" {
		entries, err = dlStore.ListDeadLetters(
			ctx, req.ActorId, limit,
		)
	} else {
		entries, err = dlStore.ListAllDeadLetters(
			ctx, limit, int(req.Offset),
		)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list dead "+
			"letters: %v", err)
	}

	totalCount, err := dlStore.CountDeadLetters(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count dead "+
			"letters: %v", err)
	}

	resp := &waverpc.ListDeadLettersResponse{
		DeadLetters: make([]*waverpc.DeadLetter, len(entries)),
		TotalCount:  totalCount,
	}
	for i := range entries {
		resp.DeadLetters[i] = deadLetterToProto(
			&entries[i], req.IncludePayload,
		)
	}

	return resp, nil
}

// GetDeadLetter returns a single dead letter by ID, including its payload.
func (r *RPCServer) GetDeadLetter(ctx context.Context,
	req *waverpc.GetDeadLetterRequest) (*waverpc.GetDeadLetterResponse,
	error) {

	dlStore, err := r.requireDeadLetterStore()
	if err != nil {
		return nil, err
	}

	if req.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "id is "+
			"required")
	}

	dl, err := dlStore.GetDeadLetter(ctx, req.Id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get dead letter: %v",
			err)
	}
	if dl == nil {
		return nil, status.Errorf(codes.NotFound, "dead letter %s "+
			"not found", req.Id)
	}

	return &waverpc.GetDeadLetterResponse{
		DeadLetter: deadLetterToProto(dl, true),
	}, nil
}

// RequeueDeadLetter re-enqueues a dead letter into its original actor
// mailbox under a fresh message ID and removes it from the dead-letter
// queue.
func (r *RPCServer) RequeueDeadLetter(ctx context.Context,
	req *waverpc.RequeueDeadLetterRequest) (
	*waverpc.RequeueDeadLetterResponse, error) {

	dlStore, err := r.requireDeadLetterStore()
	if err != nil {
		return nil, err
	}

	if req.Id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "id is "+
			"required")
	}

	newID, err := dlStore.RequeueDeadLetter(ctx, req.Id)
	switch {
	case errors.Is(err, actor.ErrDeadLetterNotFound):
		return nil, status.Errorf(codes.NotFound, "dead letter %s "+
			"not found", req.Id)

	case errors.Is(err, actor.ErrDeadLetterNotRequeueable):
		return nil, status.Errorf(codes.FailedPrecondition, "dead "+
			"letter %s is not requeueable: %v", req.Id, err)

	case err != nil:
		return nil, status.Errorf(codes.Internal, "requeue dead "+
			"letter: %v", err)
	}

	r.server.log.InfoS(ctx, "Dead letter requeued by operator",
		"dead_letter_id", req.Id,
		"new_message_id", newID,
	)

	return &waverpc.RequeueDeadLetterResponse{
		NewMessageId: newID,
	}, nil
}

// PurgeDeadLetters permanently deletes dead letters older than the given
// age.
func (r *RPCServer) PurgeDeadLetters(ctx context.Context,
	req *waverpc.PurgeDeadLettersRequest) (
	*waverpc.PurgeDeadLettersResponse, error) {

	dlStore, err := r.requireDeadLetterStore()
	if err != nil {
		return nil, err
	}

	// A positive age is required so an empty request can never
	// wholesale-delete the queue, and the age is bounded so the
	// seconds-to-Duration conversion below cannot overflow negative
	// (which would push the cutoff into the future and purge
	// everything).
	if req.OlderThanSeconds <= 0 ||
		req.OlderThanSeconds > maxPurgeAgeSeconds {
		return nil, status.Errorf(codes.InvalidArgument,
			"older_than_seconds must be positive and at most %d",
			maxPurgeAgeSeconds)
	}

	cutoff := r.server.clk.Now().Add(
		-time.Duration(req.OlderThanSeconds) * time.Second,
	)

	removed, err := dlStore.PurgeDeadLetters(ctx, cutoff)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "purge dead "+
			"letters: %v", err)
	}

	r.server.log.InfoS(ctx, "Dead letters purged by operator",
		"removed", removed,
		"cutoff", cutoff,
	)

	return &waverpc.PurgeDeadLettersResponse{
		Removed: removed,
	}, nil
}
