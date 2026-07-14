package serverconn

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mailboxClientStub is a configurable MailboxServiceClient test double.
type mailboxClientStub struct {
	sendFn func(
		ctx context.Context, in *mailboxpb.SendRequest,
		opts ...grpc.CallOption,
	) (*mailboxpb.SendResponse, error)

	pullFn func(
		ctx context.Context, in *mailboxpb.PullRequest,
		opts ...grpc.CallOption,
	) (*mailboxpb.PullResponse, error)

	ackFn func(
		ctx context.Context, in *mailboxpb.AckUpToRequest,
		opts ...grpc.CallOption,
	) (*mailboxpb.AckUpToResponse, error)
}

// Send executes the configured send function.
func (s *mailboxClientStub) Send(ctx context.Context, in *mailboxpb.SendRequest,
	opts ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	if s.sendFn != nil {
		return s.sendFn(ctx, in, opts...)
	}

	return &mailboxpb.SendResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// Pull executes the configured pull function.
func (s *mailboxClientStub) Pull(ctx context.Context, in *mailboxpb.PullRequest,
	opts ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	if s.pullFn != nil {
		return s.pullFn(ctx, in, opts...)
	}

	return &mailboxpb.PullResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// AckUpTo executes the configured ack function.
func (s *mailboxClientStub) AckUpTo(ctx context.Context,
	in *mailboxpb.AckUpToRequest, opts ...grpc.CallOption) (
	*mailboxpb.AckUpToResponse, error) {

	if s.ackFn != nil {
		return s.ackFn(ctx, in, opts...)
	}

	return &mailboxpb.AckUpToResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// checkpointLoadStore allows overriding LoadCheckpoint behavior for tests.
type checkpointLoadStore struct {
	*memCheckpointStore

	loadErr        error
	loadCheckpoint *actor.Checkpoint
}

// LoadCheckpoint returns an injected error/checkpoint when configured.
func (s *checkpointLoadStore) LoadCheckpoint(ctx context.Context,
	actorID string) (*actor.Checkpoint, error) {

	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.loadCheckpoint != nil {
		return s.loadCheckpoint, nil
	}

	return s.memCheckpointStore.LoadCheckpoint(ctx, actorID)
}

// checkpointSaveStore allows overriding SaveCheckpoint behavior for tests.
type checkpointSaveStore struct {
	*memCheckpointStore

	saveErr error
}

// SaveCheckpoint returns an injected error when configured.
func (s *checkpointSaveStore) SaveCheckpoint(
	ctx context.Context, params actor.CheckpointParams,
) error {

	if s.saveErr != nil {
		return s.saveErr
	}

	return s.memCheckpointStore.SaveCheckpoint(ctx, params)
}

// newErrorPathActor builds a connector actor with defaults and test overrides.
func newErrorPathActor(
	edge mailboxpb.MailboxServiceClient,
	store actor.DeliveryStore,
) *ServerConnectionActor {

	cfg := DefaultConnectorConfig()
	cfg.Edge = edge
	cfg.Store = store
	cfg.LocalMailboxID = "client-1"
	cfg.RemoteMailboxID = "server-1"
	cfg.ArkProtocolVersion = 1

	return NewServerConnectionActor(cfg)
}

// TestPullBatch_StatusFailure verifies pullBatch wraps non-OK status responses.
func TestPullBatch_StatusFailure(t *testing.T) {
	t.Parallel()

	edge := &mailboxClientStub{
		pullFn: func(ctx context.Context, in *mailboxpb.PullRequest,
			opts ...grpc.CallOption) (*mailboxpb.PullResponse,
			error) {

			return &mailboxpb.PullResponse{
				Status: &mailboxpb.Status{
					Ok:      false,
					Code:    "TEMPORARY",
					Message: "pull failed",
				},
			}, nil
		},
	}

	actor := newErrorPathActor(edge, newMemCheckpointStore())

	_, _, err := actor.pullBatch(t.Context(), 0)
	require.Error(t, err)

	var stErr *mailboxconn.StatusError
	require.ErrorAs(t, err, &stErr)
	require.Equal(t, "Pull", stErr.Op)
	require.Contains(t, stErr.Error(), "TEMPORARY")
}

// TestAckRemote_StatusFailure verifies ackRemote wraps non-OK status responses.
func TestAckRemote_StatusFailure(t *testing.T) {
	t.Parallel()

	edge := &mailboxClientStub{
		ackFn: func(ctx context.Context, in *mailboxpb.AckUpToRequest,
			opts ...grpc.CallOption) (*mailboxpb.AckUpToResponse,
			error) {

			return &mailboxpb.AckUpToResponse{
				Status: &mailboxpb.Status{
					Ok:      false,
					Code:    "INTERNAL",
					Message: "ack failed",
				},
			}, nil
		},
	}

	actor := newErrorPathActor(edge, newMemCheckpointStore())

	err := actor.ackRemote(t.Context(), 1)
	require.Error(t, err)

	var stErr *mailboxconn.StatusError
	require.ErrorAs(t, err, &stErr)
	require.Equal(t, "AckUpTo", stErr.Op)
	require.Contains(t, stErr.Error(), "ack failed")
}

// TestIsIngressShutdownErr verifies expected shutdown cancellation is
// distinguished from retryable ingress failures.
func TestIsIngressShutdownErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cancel bool
		err    error
		want   bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name:   "local context canceled",
			cancel: true,
			err:    context.Canceled,
			want:   true,
		},
		{
			name: "grpc canceled",
			err:  status.Error(codes.Canceled, "context canceled"),
			want: false,
		},
		{
			name:   "ctx canceled after transport error",
			cancel: true,
			err:    errors.New("transport closed"),
			want:   true,
		},
		{
			name: "retryable transport error",
			err:  errors.New("temporary transport failure"),
			want: false,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if test.cancel {
				cancel()
			}

			require.Equal(
				t, test.want,
				isIngressShutdownErr(ctx, test.err),
			)
		})
	}
}

// TestLoadCheckpoint_Errors verifies loadCheckpoint surfaces store/decode
// failures.
func TestLoadCheckpoint_Errors(t *testing.T) {
	t.Parallel()

	edge := &mailboxClientStub{}

	loadErrActor := newErrorPathActor(
		edge, &checkpointLoadStore{
			memCheckpointStore: newMemCheckpointStore(),
			loadErr:            fmt.Errorf("load failed"),
		},
	)

	_, err := loadErrActor.loadCheckpoint(t.Context())
	require.ErrorContains(t, err, "load failed")

	decodeErrActor := newErrorPathActor(
		edge, &checkpointLoadStore{
			memCheckpointStore: newMemCheckpointStore(),
			loadCheckpoint: &actor.Checkpoint{
				ActorID:   "serverconn-client-1",
				StateType: ackStateType,
				StateData: []byte{0xff, 0x00, 0x01},
			},
		},
	)

	_, err = decodeErrActor.loadCheckpoint(t.Context())
	require.Error(t, err)
}

// TestSaveCheckpoint_Error verifies saveCheckpoint surfaces store save errors.
func TestSaveCheckpoint_Error(t *testing.T) {
	t.Parallel()

	actor := newErrorPathActor(
		&mailboxClientStub{},
		&checkpointSaveStore{
			memCheckpointStore: newMemCheckpointStore(),
			saveErr:            fmt.Errorf("save failed"),
		},
	)

	err := actor.saveCheckpoint(t.Context(), AckState{})
	require.ErrorContains(t, err, "save failed")
}

// TestStatusError_ErrorString verifies statusError string formatting paths.
func TestStatusError_ErrorString(t *testing.T) {
	t.Parallel()

	errNil := (&mailboxconn.StatusError{
		Op: "AckUpTo",
	}).Error()
	require.Contains(t, errNil, "nil status")

	errStatus := (&mailboxconn.StatusError{
		Op: "Pull",
		Status: &mailboxpb.Status{
			Ok:      false,
			Code:    "EIO",
			Message: "io failure",
		},
	}).Error()
	require.Contains(t, errStatus, "io failure")
	require.Contains(t, errStatus, "EIO")
}
