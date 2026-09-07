package serverconn

import (
	"context"
	"math"
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestPullBatchCursorContract exercises the untrusted pull response boundary.
// Gaps are legal; regressions, overlapping rows and invented successors are
// not.
func TestPullBatchCursorContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cursor  uint64
		seqs    []uint64
		next    uint64
		invalid bool
	}{
		{
			name: "initial global gap",
			seqs: []uint64{
				4,
				9,
			},
			next: 10,
		},
		{
			name:   "interleaved recipients and rollback gaps",
			cursor: 10,
			seqs: []uint64{
				13,
				18,
				21,
			},
			next: 22,
		},
		{
			name:   "inclusive start",
			cursor: 10,
			seqs: []uint64{
				10,
				11,
			},
			next: 12,
		},
		{
			name:   "overlapping pull",
			cursor: 10,
			seqs: []uint64{
				9,
				10,
			},
			next:    11,
			invalid: true,
		},
		{
			name:   "duplicate sequence",
			cursor: 10,
			seqs: []uint64{
				10,
				10,
			},
			next:    11,
			invalid: true,
		},
		{
			name:   "unordered rows",
			cursor: 10,
			seqs: []uint64{
				12,
				11,
			},
			next:    13,
			invalid: true,
		},
		{
			name:   "inflated cursor",
			cursor: 10,
			seqs: []uint64{
				10,
				11,
			},
			next:    1000,
			invalid: true,
		},
		{
			name:   "regressed cursor",
			cursor: 10,
			seqs: []uint64{
				10,
				11,
			},
			next:    10,
			invalid: true,
		},
		{
			name: "zero sequence",
			seqs: []uint64{
				0,
			},
			next:    1,
			invalid: true,
		},
		{
			name: "overflow",
			seqs: []uint64{
				math.MaxUint64,
			},
			next:    0,
			invalid: true,
		},
		{
			name:   "legacy empty zero",
			cursor: 10,
		},
		{
			name:   "empty unchanged",
			cursor: 10,
			next:   10,
		},
		{
			name:    "empty inflation",
			cursor:  10,
			next:    1000,
			invalid: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envelopes := make([]*mailboxpb.Envelope, len(tc.seqs))
			for i, seq := range tc.seqs {
				envelopes[i] = &mailboxpb.Envelope{
					EventSeq: seq,
				}
			}
			edge := &mailboxClientStub{pullFn: func(
				_ context.Context, req *mailboxpb.PullRequest,
				_ ...grpc.CallOption) (*mailboxpb.PullResponse,
				error) {

				require.Equal(t, tc.cursor, req.Cursor)

				return &mailboxpb.PullResponse{
					Status: &mailboxpb.Status{
						Ok: true,
					},
					Envelopes:  envelopes,
					NextCursor: tc.next,
				}, nil
			}}
			conn := newErrorPathActor(edge, newMemCheckpointStore())
			got, next, err := conn.pullBatch(
				context.Background(), tc.cursor,
			)
			if tc.invalid {
				require.Error(t, err)
				require.Empty(t, got)
				require.Zero(t, next)

				return
			}
			require.NoError(t, err)
			require.Equal(t, envelopes, got)
			require.Equal(t, tc.next, next)
		})
	}
}
