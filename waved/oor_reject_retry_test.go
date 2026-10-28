package waved

import (
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/stretchr/testify/require"
)

// TestOORRejectRetry verifies that the transient rejections
// (input-not-spendable, user-balance-exceeded, and operator-internal) are
// driven through the OOR FSM as retryable (so the submit is re-driven —
// bounded by the FSM's cumulative retry-window cap — until the transient
// condition clears and the transfer recovers), while every other rejection,
// unspecified included, stays terminal. This is the
// server-reject-to-client-retry link behind the darepo-client#938 fix:
// paired with oor.TestHandleOutboxError (retryable OutboxError re-drives
// instead of failing), it shows a transient rejection recovers rather than
// wedging.
func TestOORRejectRetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		wantRetryable bool
		wantDelay     time.Duration
	}{
		{
			name: "input not spendable is retried",
			err: &oor.ErrInputNotSpendable{
				Reason:           "vtxo x not spendable",
				ServerBestHeight: 100,
			},
			wantRetryable: true,
			wantDelay:     oorTransientRejectRetryDelay,
		},
		{
			name: "output policy is terminal",
			err: &oor.ErrOutputPolicyViolation{
				Reason: "x",
			},
			wantRetryable: false,
		},
		{
			name: "user balance is retried",
			err: &oor.ErrUserBalanceExceeded{
				Reason: "x",
			},
			wantRetryable: true,
			wantDelay:     oorTransientRejectRetryDelay,
		},
		{
			name: "operator internal is retried",
			err: &oor.ErrOperatorInternal{
				Reason: "backing store temporarily unavailable",
			},
			wantRetryable: true,
			wantDelay:     oorTransientRejectRetryDelay,
		},
		{
			// Classification leaves UNSPECIFIED unmapped, so the
			// reject lane sees the raw proto rejection; it must
			// stay terminal rather than be retried on a guess.
			name: "unspecified rejection is terminal",
			err: &oorpb.SubmitRejectedError{
				Code: oorpb.
					OORRejectCode_OOR_REJECT_UNSPECIFIED,
				Reason: "unknown failure",
			},
			wantRetryable: false,
		},
		{
			name: "lineage too large is terminal",
			err: &oor.ErrLineageTooLarge{
				Reason: "x",
			},
			wantRetryable: false,
		},
		{
			name:          "generic error is terminal",
			err:           errors.New("boom"),
			wantRetryable: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			retryable, delay := oorRejectRetry(tc.err)
			require.Equal(t, tc.wantRetryable, retryable)
			require.Equal(t, tc.wantDelay, delay)
		})
	}
}
