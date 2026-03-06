package vhtlc

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	arkscript "github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

// testOpts returns a valid Opts for use in tests, using deterministic keys
// from testutils.CreateKey.
func testOpts(t *testing.T) Opts {
	t.Helper()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	server, _ := testutils.CreateKey(3)

	return Opts{
		Sender:   sender,
		Receiver: receiver,
		Server:   server,
		PreimageHash: Hash160(
			[]byte("test-preimage-32-bytes-exactly!!"),
		),
		RefundLocktime:                       500_000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                288,
		UnilateralRefundWithoutReceiverDelay: 1008,
	}
}

// TestNewPolicyValidation verifies that NewPolicy rejects invalid inputs.
func TestNewPolicyValidation(t *testing.T) {
	t.Parallel()

	valid := testOpts(t)

	tests := []struct {
		name    string
		mutate  func(*Opts)
		wantErr string
	}{
		{
			name:    "nil sender",
			mutate:  func(o *Opts) { o.Sender = nil },
			wantErr: "sender key is nil",
		},
		{
			name:    "nil receiver",
			mutate:  func(o *Opts) { o.Receiver = nil },
			wantErr: "receiver key is nil",
		},
		{
			name:    "nil server",
			mutate:  func(o *Opts) { o.Server = nil },
			wantErr: "server key is nil",
		},
		{
			name:    "short preimage hash",
			mutate:  func(o *Opts) { o.PreimageHash = make([]byte, 19) },
			wantErr: "preimage hash must be 20 bytes",
		},
		{
			name:    "long preimage hash",
			mutate:  func(o *Opts) { o.PreimageHash = make([]byte, 21) },
			wantErr: "preimage hash must be 20 bytes",
		},
		{
			name: "all exit delays zero",
			mutate: func(o *Opts) {
				o.UnilateralClaimDelay = 0
				o.UnilateralRefundDelay = 0
				o.UnilateralRefundWithoutReceiverDelay = 0
			},
			wantErr: "at least one CSV delay must be non-zero",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := valid
			tc.mutate(&opts)

			_, err := NewPolicy(opts)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestNewPolicyDeterminism verifies that the same Opts always produce the
// same output key and leaf scripts (byte-for-byte identical).
func TestNewPolicyDeterminism(t *testing.T) {
	t.Parallel()

	opts := testOpts(t)

	p1, err := NewPolicy(opts)
	require.NoError(t, err)

	p2, err := NewPolicy(opts)
	require.NoError(t, err)

	// Output keys must be identical.
	require.Equal(t,
		p1.OutputKey().SerializeCompressed(),
		p2.OutputKey().SerializeCompressed(),
		"output key must be deterministic",
	)

	// All six leaf scripts must be identical.
	require.Len(t, p1.Leaves, 6)
	require.Len(t, p2.Leaves, 6)

	for i := range p1.Leaves {
		require.Equal(t,
			p1.Leaves[i].Leaf.Script,
			p2.Leaves[i].Leaf.Script,
			"leaf %d script must be deterministic", i,
		)
	}
}

// TestNewPolicyLeafRoles verifies the canonical leaf ordering: all collab
// leaves appear before all exit leaves.
func TestNewPolicyLeafRoles(t *testing.T) {
	t.Parallel()

	opts := testOpts(t)
	p, err := NewPolicy(opts)
	require.NoError(t, err)

	require.Len(t, p.Leaves, 6, "vHTLC must have exactly 6 leaves")

	// The first three leaves must be collab, the last three exit.
	for i, leaf := range p.Leaves {
		if i < 3 {
			require.Equal(t, arkscript.LeafRoleCollab, leaf.Role,
				"leaf %d should be collab", i)
		} else {
			require.Equal(t, arkscript.LeafRoleExit, leaf.Role,
				"leaf %d should be exit", i)
		}
	}
}

// TestNewPolicySpendInfos verifies that all six named spend-info accessors
// return non-empty WitnessScript and ControlBlock bytes.
func TestNewPolicySpendInfos(t *testing.T) {
	t.Parallel()

	opts := testOpts(t)
	p, err := NewPolicy(opts)
	require.NoError(t, err)

	type namedAccessor struct {
		name string
		fn   func() (*arkscript.SpendInfo, error)
	}

	accessors := []namedAccessor{
		{"Claim", p.ClaimSpendInfo},
		{"Refund", p.RefundSpendInfo},
		{"RefundWithoutReceiver", p.RefundWithoutReceiverSpendInfo},
		{"UnilateralClaim", p.UnilateralClaimSpendInfo},
		{"UnilateralRefund", p.UnilateralRefundSpendInfo},
		{
			"UnilateralRefundWithoutReceiver",
			p.UnilateralRefundWithoutReceiverSpendInfo,
		},
	}

	for _, a := range accessors {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()

			info, err := a.fn()
			require.NoError(t, err)
			require.NotEmpty(t, info.WitnessScript,
				"WitnessScript must not be empty")
			require.NotEmpty(t, info.ControlBlock,
				"ControlBlock must not be empty")
		})
	}
}

// TestNewPolicyPkScript verifies that PkScript returns a valid 34-byte P2TR
// pkScript.
func TestNewPolicyPkScript(t *testing.T) {
	t.Parallel()

	opts := testOpts(t)
	p, err := NewPolicy(opts)
	require.NoError(t, err)

	pkScript, err := p.PkScript()
	require.NoError(t, err)

	// A P2TR pkScript is: OP_1 <32-byte-key> = 34 bytes.
	require.Len(t, pkScript, 34, "P2TR pkScript must be 34 bytes")
	require.Equal(t, byte(0x51), pkScript[0],
		"P2TR pkScript must start with OP_1")
}

// TestHash160 verifies that Hash160 matches btcutil.Hash160.
func TestHash160(t *testing.T) {
	t.Parallel()

	data := []byte("test preimage data")
	got := Hash160(data)
	want := btcutil.Hash160(data)

	require.Equal(t, want, got)
	require.Len(t, got, 20, "HASH160 must produce 20 bytes")
}

// TestExitDelaysZero verifies the ExitDelaysZero helper.
func TestExitDelaysZero(t *testing.T) {
	t.Parallel()

	opts := testOpts(t)

	// All non-zero: should not be zero.
	require.False(t, opts.ExitDelaysZero())

	// Only claim delay non-zero.
	opts2 := opts
	opts2.UnilateralRefundDelay = 0
	opts2.UnilateralRefundWithoutReceiverDelay = 0
	require.False(t, opts2.ExitDelaysZero())

	// All zero.
	opts3 := opts
	opts3.UnilateralClaimDelay = 0
	opts3.UnilateralRefundDelay = 0
	opts3.UnilateralRefundWithoutReceiverDelay = 0
	require.True(t, opts3.ExitDelaysZero())
}
