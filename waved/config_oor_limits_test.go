package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// validOORLimitsTestConfig returns a baseline daemon config that reaches OOR
// limit validation without failing unrelated required settings first.
func validOORLimitsTestConfig() *Config {
	cfg := DefaultConfig()
	cfg.Network = "regtest"
	cfg.Server.Host = "127.0.0.1:10010"
	cfg.Wallet.EsploraURL = "http://127.0.0.1:3000"

	return cfg
}

// TestConfigValidateRejectsInvalidOORLimits verifies daemon config validation
// rejects OOR safety caps that are zero, too small for standard scripts, or
// inconsistent with each other.
func TestConfigValidateRejectsInvalidOORLimits(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*OORLimitsConfig)
		wantErr string
	}{
		{
			name: "zero checkpoints",
			mutate: func(limits *OORLimitsConfig) {
				limits.MaxCheckpoints = 0
			},
			wantErr: "oor.limits.maxcheckpoints",
		},
		{
			name: "zero vtxo matches",
			mutate: func(limits *OORLimitsConfig) {
				limits.MaxVTXOMatches = 0
			},
			wantErr: "oor.limits.maxvtxomatches",
		},
		{
			name: "zero mailbox items",
			mutate: func(limits *OORLimitsConfig) {
				limits.MaxMailboxItems = 0
			},
			wantErr: "oor.limits.maxmailboxitems",
		},
		{
			name: "script cap below standard taproot script",
			mutate: func(limits *OORLimitsConfig) {
				limits.MaxMailboxScriptBytes =
					minOORMailboxScriptBytes - 1
			},
			wantErr: "oor.limits.maxmailboxscriptbytes",
		},
		{
			name: "mailbox items below checkpoints",
			mutate: func(limits *OORLimitsConfig) {
				limits.MaxCheckpoints = 3
				limits.MaxVTXOMatches = 2
				limits.MaxMailboxItems = 2
			},
			wantErr: "oor.limits.maxmailboxitems",
		},
		{
			name: "mailbox items below vtxo matches",
			mutate: func(limits *OORLimitsConfig) {
				limits.MaxCheckpoints = 2
				limits.MaxVTXOMatches = 3
				limits.MaxMailboxItems = 2
			},
			wantErr: "oor.limits.maxmailboxitems",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := validOORLimitsTestConfig()
			tc.mutate(cfg.OOR.Limits)

			err := cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestConfigValidateAcceptsDefaultOORLimits verifies the shipped OOR safety
// defaults pass daemon config validation.
func TestConfigValidateAcceptsDefaultOORLimits(t *testing.T) {
	t.Parallel()

	cfg := validOORLimitsTestConfig()
	require.NoError(t, cfg.Validate())
}
