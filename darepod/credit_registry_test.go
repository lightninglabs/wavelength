package darepod

import (
	"testing"

	"github.com/lightninglabs/darepo-client/credit"
	"github.com/stretchr/testify/require"
)

// TestCreditMaxAwaitingPollsCoercesZero asserts the production credit registry
// never runs with the unbounded awaiting wait that lets a credit-backed send
// hang forever (darepo-client#880): an unset (zero) config value is coerced to
// the fail-fast default, while an explicit value passes through as an operator
// override.
func TestCreditMaxAwaitingPollsCoercesZero(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  CreditConfig
		want uint32
	}{
		{
			name: "zero coerces to fail-fast default",
			cfg: CreditConfig{
				MaxAwaitingPolls: 0,
			},
			want: credit.DefaultMaxAwaitingPolls,
		},
		{
			name: "explicit override passes through",
			cfg: CreditConfig{
				MaxAwaitingPolls: 42,
			},
			want: 42,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, tc.want, tc.cfg.MaxAwaitingPollsOrDefault(),
			)
		})
	}

	// The default must be a bounded, positive cap: a zero default would
	// silently re-introduce the unbounded hang this guard exists to close.
	require.Positive(t, credit.DefaultMaxAwaitingPolls)
}
