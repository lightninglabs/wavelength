package waved

import (
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/oor"
	"github.com/stretchr/testify/require"
)

// TestOORRejectRetry verifies that only the transient input-not-spendable
// rejection is driven through the OOR FSM as retryable (so the submit is
// re-driven until the operator's chain view catches up and the transfer
// recovers), while every other typed rejection stays terminal. This is the
// server-reject-to-client-retry link behind the darepo-client#938 fix: paired
// with oor.TestHandleOutboxError (retryable OutboxError re-drives instead of
// failing), it shows an input-not-spendable rejection recovers rather than
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
			wantDelay:     oorInputNotSpendableRetryDelay,
		},
		{
			name: "output policy is terminal",
			err: &oor.ErrOutputPolicyViolation{
				Reason: "x",
			},
			wantRetryable: false,
		},
		{
			name: "user balance is terminal",
			err: &oor.ErrUserBalanceExceeded{
				Reason: "x",
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
