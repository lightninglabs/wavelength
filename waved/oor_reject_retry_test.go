package waved

import (
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/oor"
	"github.com/stretchr/testify/require"
)

// TestOORRejectRetry verifies that the two transient rejections
// (input-not-spendable and user-balance-exceeded) are driven through the OOR
// FSM as retryable (so the submit is re-driven — bounded by the FSM's
// cumulative retry-window cap — until the transient condition clears and the
// transfer recovers), while every other typed rejection stays terminal. This is
// the server-reject-to-client-retry link behind the darepo-client#938 fix:
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
