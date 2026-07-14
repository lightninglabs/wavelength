package conn

import (
	"fmt"
	"testing"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
)

// TestStatusErrorClassification is a table-driven test proving that every
// permanent version code is classified as permanent while transient transport
// and internal failures are not, and that the full structured status payload
// (supported versions) remains available on the typed error.
func TestStatusErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		code        string
		permanent   bool
		mailboxVers []uint32
		arkVers     []uint32
	}{
		{
			name:      "mailbox version unsupported",
			code:      StatusMailboxVersionUnsupported,
			permanent: true,
			mailboxVers: []uint32{
				1,
			},
		},
		{
			name:      "ark version unsupported",
			code:      StatusArkVersionUnsupported,
			permanent: true,
			arkVers: []uint32{
				1,
				2,
			},
		},
		{
			name:      "ark version mismatch",
			code:      StatusArkVersionMismatch,
			permanent: true,
			arkVers: []uint32{
				1,
			},
		},
		{
			name:      "upgrade required",
			code:      StatusUpgradeRequired,
			permanent: true,
			arkVers: []uint32{
				2,
			},
		},
		{
			name:      "transient unavailable",
			code:      "UNAVAILABLE",
			permanent: false,
		},
		{
			name:      "internal failure",
			code:      "INTERNAL",
			permanent: false,
		},
		{
			name:      "empty code",
			code:      "",
			permanent: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			status := &mailboxpb.Status{
				Ok:                       false,
				Code:                     tc.code,
				Message:                  "boom",
				SupportedMailboxVersions: tc.mailboxVers,
				SupportedArkVersions:     tc.arkVers,
			}

			statusErr := NewStatusError("Send", status)

			// Classification matches the permanent-code table.
			require.Equal(
				t, tc.permanent, statusErr.IsPermanentVersion(),
			)
			require.Equal(
				t, tc.permanent,
				IsPermanentVersionError(statusErr),
			)

			// The code and full payload remain available.
			require.Equal(t, tc.code, statusErr.Code())
			require.Equal(
				t, tc.mailboxVers,
				statusErr.SupportedMailboxVersions(),
			)
			require.Equal(
				t, tc.arkVers, statusErr.SupportedArkVersions(),
			)

			// The error string preserves operation, message, and
			// code for diagnosability.
			require.Contains(t, statusErr.Error(), "Send")
			require.Contains(t, statusErr.Error(), "boom")
			if tc.code != "" {
				require.Contains(
					t, statusErr.Error(), tc.code,
				)
			}
		})
	}
}

// TestStatusErrorNilStatus proves the typed error degrades gracefully when no
// status is present rather than panicking.
func TestStatusErrorNilStatus(t *testing.T) {
	t.Parallel()

	statusErr := NewStatusError("AckUpTo", nil)

	require.False(t, statusErr.IsPermanentVersion())
	require.Empty(t, statusErr.Code())
	require.Nil(t, statusErr.SupportedMailboxVersions())
	require.Nil(t, statusErr.SupportedArkVersions())
	require.Contains(t, statusErr.Error(), "nil status")
}

// TestIsPermanentVersionErrorWrapped proves the free helper unwraps a wrapped
// StatusError, so durable senders can classify deeply nested errors.
func TestIsPermanentVersionErrorWrapped(t *testing.T) {
	t.Parallel()

	permErr := NewStatusError("Send", &mailboxpb.Status{
		Ok:   false,
		Code: StatusUpgradeRequired,
	})
	wrapped := fmt.Errorf("send failed: %w", permErr)
	require.True(t, IsPermanentVersionError(wrapped))

	transientErr := NewStatusError("Send", &mailboxpb.Status{
		Ok:   false,
		Code: "UNAVAILABLE",
	})
	require.False(
		t,
		IsPermanentVersionError(
			fmt.Errorf("send failed: %w", transientErr),
		),
	)

	// A non-StatusError is never a permanent version error.
	require.False(
		t,
		IsPermanentVersionError(
			fmt.Errorf("some other error"),
		),
	)
}
