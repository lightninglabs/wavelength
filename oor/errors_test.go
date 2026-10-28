package oor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/oorpb"
	"github.com/stretchr/testify/require"
)

// TestClassifySubmitError verifies each typed operator rejection code maps to
// its client-facing typed error, that the user-balance rejection is distinct
// from the output-policy one (they have opposite retry semantics), and that
// untyped errors pass through unchanged.
func TestClassifySubmitError(t *testing.T) {
	t.Parallel()

	// Short alias for the long enum, so the deeply-nested subtest that uses
	// it stays within the 80-column limit.
	notSpendableCode := oorpb.OORRejectCode_OOR_REJECT_INPUT_NOT_SPENDABLE

	t.Run("user balance maps to typed error", func(t *testing.T) {
		t.Parallel()

		rejected := &oorpb.SubmitRejectedError{
			Code:   oorpb.OORRejectCode_OOR_REJECT_USER_BALANCE,
			Reason: "user balance exceeds maximum",
		}

		got := ClassifySubmitError(rejected)
		require.ErrorIs(t, got, &ErrUserBalanceExceeded{})
		require.Contains(t, got.Error(), "user balance exceeds maximum")
	})

	t.Run("user balance distinct from output policy", func(t *testing.T) {
		t.Parallel()

		balance := ClassifySubmitError(&oorpb.SubmitRejectedError{
			Code: oorpb.OORRejectCode_OOR_REJECT_USER_BALANCE,
		})
		policy := ClassifySubmitError(&oorpb.SubmitRejectedError{
			Code: oorpb.OORRejectCode_OOR_REJECT_OUTPUT_POLICY,
		})

		// A balance rejection must not be mistaken for a (terminal)
		// output-policy rejection, since only the latter requires
		// restructuring the outputs.
		require.NotErrorIs(t, balance, &ErrOutputPolicyViolation{})
		require.NotErrorIs(t, policy, &ErrUserBalanceExceeded{})
	})

	t.Run("output policy still maps", func(t *testing.T) {
		t.Parallel()

		got := ClassifySubmitError(&oorpb.SubmitRejectedError{
			Code:   oorpb.OORRejectCode_OOR_REJECT_OUTPUT_POLICY,
			Reason: "output 0 exceeds the per-VTXO maximum",
		})
		require.ErrorIs(t, got, &ErrOutputPolicyViolation{})
	})

	t.Run("input not spendable maps and carries height",
		func(t *testing.T) {
			t.Parallel()

			got := ClassifySubmitError(&oorpb.SubmitRejectedError{
				Code:             notSpendableCode,
				Reason:           "vtxo x not spendable",
				ServerBestHeight: 313034,
			})
			require.ErrorIs(t, got, &ErrInputNotSpendable{})

			// It must not collapse into the terminal output-policy
			// error: input-not-spendable is transient and
			// retryable.
			require.NotErrorIs(t, got, &ErrOutputPolicyViolation{})

			// The operator's best height rides through so the
			// caller can check whether the operator is behind on
			// chain sync.
			var notSpendable *ErrInputNotSpendable
			require.True(t, errors.As(got, &notSpendable))
			require.Equal(
				t, uint32(313034),
				notSpendable.ServerBestHeight,
			)
		})

	t.Run("operator internal maps to typed error", func(t *testing.T) {
		t.Parallel()

		got := ClassifySubmitError(&oorpb.SubmitRejectedError{
			Code:   oorpb.OORRejectCode_OOR_REJECT_INTERNAL,
			Reason: "backing store temporarily unavailable",
		})
		require.ErrorIs(t, got, &ErrOperatorInternal{})
		require.Contains(
			t, got.Error(),
			"backing store temporarily unavailable",
		)

		// An internal-fault rejection must not collapse into any of
		// the other typed rejections: unlike output-policy it is
		// retryable, and unlike input-not-spendable and user-balance
		// the transient condition is operator-internal.
		require.NotErrorIs(t, got, &ErrOutputPolicyViolation{})
		require.NotErrorIs(t, got, &ErrInputNotSpendable{})
		require.NotErrorIs(t, got, &ErrUserBalanceExceeded{})
	})

	t.Run("unspecified has no typed mapping", func(t *testing.T) {
		t.Parallel()

		// UNSPECIFIED deliberately stays unmapped: it conflates
		// faults the operator did not classify, so the client must
		// keep treating it as terminal rather than guessing at
		// retryability. The classifier passes it through unchanged.
		rejected := &oorpb.SubmitRejectedError{
			Code:   oorpb.OORRejectCode_OOR_REJECT_UNSPECIFIED,
			Reason: "unknown failure",
		}

		got := ClassifySubmitError(rejected)
		require.Same(t, rejected, got)
	})

	t.Run("wrapped rejection is unwrapped", func(t *testing.T) {
		t.Parallel()

		// errors.As must reach the typed rejection even when it is
		// wrapped, so the daemon's outer context does not hide the
		// code.
		wrapped := fmt.Errorf("submit failed: %w",
			&oorpb.SubmitRejectedError{
				Code: oorpb.
					OORRejectCode_OOR_REJECT_USER_BALANCE,
			})

		require.ErrorIs(
			t, ClassifySubmitError(wrapped),
			&ErrUserBalanceExceeded{},
		)
	})

	t.Run("nil and untyped pass through", func(t *testing.T) {
		t.Parallel()

		require.NoError(t, ClassifySubmitError(nil))

		plain := errors.New("connection reset")
		require.Equal(t, plain, ClassifySubmitError(plain))
	})
}
