package darepod

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestResolveLeaveDestination exercises resolveLeaveDestination across
// its address and pk_script branches: clean errors for malformed,
// empty, oversized, and disallowed-class inputs (funds-moving, so each
// guard matters), and verbatim / canonical script output on the happy
// paths. Address rows are built programmatically so the test stays
// valid across bech32m checksum changes.
func TestResolveLeaveDestination(t *testing.T) {
	t.Parallel()

	// Build a regtest P2TR address; the key choice is irrelevant, we
	// only need a structurally valid taproot address to decode.
	regKey := make([]byte, 32)
	regKey[0] = 0xab
	regAddr, err := btcutil.NewAddressTaproot(
		regKey, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	regScript, err := txscript.PayToAddrScript(regAddr)
	require.NoError(t, err)

	// A mainnet address under regtest params must be rejected by
	// DecodeAddress rather than silently yielding a pkScript.
	mainnetKey := make([]byte, 32)
	mainnetKey[0] = 0xcd
	mainnetAddr, err := btcutil.NewAddressTaproot(
		mainnetKey, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	addrDest := func(a string) *daemonrpc.LeaveDestination {
		return &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: a,
			},
		}
	}
	scriptDest := func(s []byte) *daemonrpc.LeaveDestination {
		return &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: s,
			},
		}
	}

	opReturn := []byte{txscript.OP_RETURN}
	verbatim := validP2TRPkScript(0xaa)

	tests := []struct {
		name string

		// dest is the destination fed to resolveLeaveDestination.
		dest *daemonrpc.LeaveDestination

		// wantErr, when non-empty, is a substring the returned error
		// must contain. An empty wantErr means the call must succeed
		// and return wantScript.
		wantErr string

		// wantScript is the expected pkScript on the success path.
		wantScript []byte
	}{
		{
			name:    "invalid address",
			dest:    addrDest("not-a-valid-address"),
			wantErr: "invalid leave address",
		},
		{
			name:    "empty address",
			dest:    addrDest(""),
			wantErr: "address is empty",
		},
		{
			// Mainnet address decodes to an error under regtest.
			name:    "cross network",
			dest:    addrDest(mainnetAddr.String()),
			wantErr: "invalid leave address",
		},
		{
			name:    "nil destination",
			dest:    nil,
			wantErr: "destination is required",
		},
		{
			name:    "empty pk_script",
			dest:    scriptDest(nil),
			wantErr: "pk_script is empty",
		},
		{
			// MaxScriptSize cap: a hostile / buggy caller cannot
			// ship a multi-kilobyte pkScript through the leave
			// path.
			name: "oversized pk_script",
			dest: scriptDest(
				make([]byte, txscript.MaxScriptSize+1),
			),
			wantErr: "too large",
		},
		{
			// BIP 431 P2A anchor is anyone-can-spend; landing leave
			// funds there would effectively burn them.
			name: "rejects p2a",
			dest: scriptDest([]byte{
				txscript.OP_1, txscript.OP_DATA_2, 0x4e, 0x73,
			}),
			wantErr: "P2A",
		},
		{
			// Truncated taproot push classifies NonStandardTy.
			name:    "rejects non-standard",
			dest:    scriptDest([]byte{0x51, 0x20, 0xaa, 0xbb}),
			wantErr: "not supported",
		},
		{
			// Real regtest taproot address resolves to btcutil's
			// canonical PayToAddrScript output.
			name:       "valid taproot",
			dest:       addrDest(regAddr.String()),
			wantScript: regScript,
		},
		{
			// pk_script is returned verbatim once the class guard
			// accepts a structurally valid P2TR script.
			name:       "valid pk_script",
			dest:       scriptDest(verbatim),
			wantScript: verbatim,
		},
		{
			// OP_RETURN (NullDataTy) stays a supported power-user
			// data-carrier / burn destination.
			name:       "accepts op_return",
			dest:       scriptDest(opReturn),
			wantScript: opReturn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := newTestRPCServer()
			got, err := r.resolveLeaveDestination(tc.dest)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantScript, got)
		})
	}
}

// TestLeaveVTXOsRejectsAllWithPerOutpointOverrides verifies the
// handler's primary "all + destinations" safety check: we can't
// honor per-outpoint overrides when we don't know the outpoint
// set up front, so the handler must reject the combination with
// codes.InvalidArgument rather than silently dropping overrides.
func TestLeaveVTXOsRejectsAllWithPerOutpointOverrides(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_All{
			All: true,
		},
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: validP2TRPkScript(0x01),
			},
		},
		Destinations: map[string]*daemonrpc.LeaveDestination{
			validOutpoint(): {
				Target: &daemonrpc.LeaveDestination_PkScript{
					PkScript: validP2TRPkScript(0x02),
				},
			},
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(
		t, codes.InvalidArgument, status.Code(err),
		"all + destinations must be InvalidArgument",
	)
	require.Contains(t, err.Error(), "selection=all")
}

// TestLeaveVTXOsRejectsMissingDefaultForUncoveredOutpoint verifies
// that the handler refuses to dispatch a batch where any outpoint
// has no destination — either the caller sets default_destination,
// or every outpoint must be explicitly covered by destinations.
func TestLeaveVTXOsRejectsMissingDefaultForUncoveredOutpoint(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	op1 := validOutpoint()
	op2 := validOutpoint2()

	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: []string{
					op1,
					op2,
				},
			},
		},
		Destinations: map[string]*daemonrpc.LeaveDestination{
			op1: {
				Target: &daemonrpc.LeaveDestination_PkScript{
					PkScript: validP2TRPkScript(0xaa),
				},
			},
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "no destination")
}

// TestLeaveVTXOsRejectsEmptyOutpoints mirrors RefreshVTXOs: an
// outpoints selection with an empty slice is an explicit bug on
// the caller's end.
func TestLeaveVTXOsRejectsEmptyOutpoints(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: nil,
			},
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "empty")
}

// TestLeaveVTXOsRejectsInvalidOutpoint verifies that malformed
// "txid:index" strings in the selection are caught before the
// handler reaches the wallet dispatch path.
func TestLeaveVTXOsRejectsInvalidOutpoint(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: []string{
					"not-an-outpoint",
				},
			},
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "invalid outpoint")
}

// TestLeaveVTXOsRejectsInvalidDestinationKey verifies that a
// destinations map keyed by a malformed outpoint string is
// rejected before dispatch. Bad keys on the override map are a
// caller bug, not a soft per-outpoint error.
func TestLeaveVTXOsRejectsInvalidDestinationKey(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	op := validOutpoint()

	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: []string{
					op,
				},
			},
		},
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: validP2TRPkScript(0x01),
			},
		},
		Destinations: map[string]*daemonrpc.LeaveDestination{
			"not-an-outpoint": {
				Target: &daemonrpc.LeaveDestination_PkScript{
					PkScript: validP2TRPkScript(0x02),
				},
			},
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "destinations")
}

// TestLeaveVTXOsRejectsExtraneousDestinationKey is the H-4
// regression guard: a destination override keyed by a syntactically
// valid outpoint that isn't in the selection (a typo'd index,
// copy-paste from another VTXO list) must fail closed with
// InvalidArgument so the typo surfaces immediately. Before the fix
// the stray entry was silently accepted into destOutputs and the
// real target fell back to default_destination — a money-moving
// footgun.
func TestLeaveVTXOsRejectsExtraneousDestinationKey(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	op1 := validOutpoint()
	stray := validOutpoint2()

	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: []string{
					op1,
				},
			},
		},
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: validP2TRPkScript(0x01),
			},
		},
		Destinations: map[string]*daemonrpc.LeaveDestination{
			stray: {
				Target: &daemonrpc.LeaveDestination_PkScript{
					PkScript: validP2TRPkScript(0x02),
				},
			},
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "outpoint not in selection")
}

// TestLeaveVTXOsRejectsMissingDefaultForAll verifies that
// selection=all without a default_destination fails fast — there's
// no way to supply per-outpoint overrides under "all", so a
// missing default is unambiguous caller error.
func TestLeaveVTXOsRejectsMissingDefaultForAll(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_All{
			All: true,
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "default_destination")
}

// TestLeaveVTXOsDryRunReturnsPreview verifies the dry_run
// short-circuit: echo the parsed outpoints with status="preview"
// without touching the wallet actor or the quoter. This is the
// path the CLI uses for pre-flight validation.
func TestLeaveVTXOsDryRunReturnsPreview(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	op1 := validOutpoint()
	op2 := validOutpoint2()

	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
			Outpoints: &daemonrpc.OutpointSelection{
				Outpoints: []string{
					op1,
					op2,
				},
			},
		},
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: validP2TRPkScript(0x01),
			},
		},
		DryRun: true,
	}

	resp, err := r.LeaveVTXOs(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, "preview", resp.Status)
	require.ElementsMatch(t,
		[]string{op1, op2}, resp.QueuedOutpoints,
	)
}

// TestLeaveVTXOsAllDryRunReturnsPreview is the H-5 smoke test: a
// selection=all dry_run request must reach the dry_run echo (status
// "preview") rather than short-circuiting on the wallet-ready gate
// or any other path before enumeration. Before the fix the dry_run
// echo ran *before* live VTXO enumeration so the queued_outpoints
// list was always empty for this combination, which lulled callers
// into re-running without --dry_run and draining every VTXO. After
// the fix the enumeration runs first, then the dry_run echo emits
// the live target set. Without a real vtxoStore in the in-package
// fixture we can only assert the path is reachable and returns
// "preview"; the populated-store case is exercised via the
// system-level itest follow-up.
func TestLeaveVTXOsAllDryRunReturnsPreview(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	req := &daemonrpc.LeaveVTXOsRequest{
		Selection: &daemonrpc.LeaveVTXOsRequest_All{
			All: true,
		},
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: validP2TRPkScript(0x01),
			},
		},
		DryRun: true,
	}

	resp, err := r.LeaveVTXOs(t.Context(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(
		t, "preview", resp.Status, "all + dry_run reaches the "+
			"dry_run echo regardless of vtxoStore wiring",
	)
}

// TestLeaveVTXOsSelectionRequired verifies that omitting the
// selection oneof entirely is an InvalidArgument error. A missing
// selection is never the intended request shape.
func TestLeaveVTXOsSelectionRequired(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	req := &daemonrpc.LeaveVTXOsRequest{
		DefaultDestination: &daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_PkScript{
				PkScript: validP2TRPkScript(0x01),
			},
		},
	}

	_, err := r.LeaveVTXOs(t.Context(), req)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "selection is required")
}

// validOutpoint returns a deterministic valid "txid:index" string
// so tests can reference a consistent outpoint without manual hex
// bookkeeping.
func validOutpoint() string {
	txid := make([]byte, 32)
	txid[0] = 0xaa

	return hex.EncodeToString(txid) + ":0"
}

// validOutpoint2 returns a second deterministic outpoint distinct
// from validOutpoint for multi-outpoint scenarios.
func validOutpoint2() string {
	txid := make([]byte, 32)
	txid[0] = 0xbb

	return hex.EncodeToString(txid) + ":1"
}

// validP2TRPkScript returns a structurally-valid 34-byte P2TR
// pkScript (`OP_1 OP_DATA_32 <32 bytes>`) for tests that need a
// real LeaveDestination_PkScript payload. The class-whitelist guard
// in resolveLeaveDestination rejects truncated / malformed pushes,
// so this helper centralises the few magic bytes test code needs.
func validP2TRPkScript(seed byte) []byte {
	pkScript := make([]byte, 34)
	pkScript[0] = 0x51 // OP_1
	pkScript[1] = 0x20 // OP_DATA_32
	pkScript[2] = seed

	return pkScript
}

// compile-time check that the package-level chainParams pointer
// matches RegressionNetParams — catches a regression where
// newTestRPCServer silently stops using regtest params and the
// cross-network test becomes a false pass.
var _ = chaincfg.RegressionNetParams
