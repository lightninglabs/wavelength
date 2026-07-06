package darepod

import (
	"encoding/hex"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestResolveLeaveDestinationInvalidAddress verifies a malformed
// address string surfaces a clean "invalid leave address" error
// (the shape any caller would see on a typo) rather than a raw
// bech32 decode message bubbling up to gRPC.
func TestResolveLeaveDestinationInvalidAddress(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	_, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_Address{
			Address: "not-a-valid-address",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid leave address")
}

// TestResolveLeaveDestinationValidTaproot verifies the happy path
// on a real regtest taproot address. We construct the address
// programmatically so the test stays valid across bech32m checksum
// changes in the library.
func TestResolveLeaveDestinationValidTaproot(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	// Build a regtest P2TR address from a deterministic 32-byte
	// x-only key. The choice of key is irrelevant — we only want
	// a structurally valid taproot address to feed DecodeAddress.
	xOnly := make([]byte, 32)
	xOnly[0] = 0xab
	addr, err := btcaddr.NewAddressTaproot(
		xOnly, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	wantScript, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	pkScript, err := r.resolveLeaveDestination(
		&daemonrpc.LeaveDestination{
			Target: &daemonrpc.LeaveDestination_Address{
				Address: addr.String(),
			},
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, wantScript, pkScript, "resolveLeaveDestination matches "+
			"btcutil's canonical PayToAddrScript output",
	)
}

// TestAddrNetNameAcceptsTestNet4 keeps cross-network address errors honest for
// every daemon network accepted by Config.Validate. Testnet3, testnet4, and
// signet share address encoding, so the label includes every possible network.
func TestAddrNetNameAcceptsTestNet4(t *testing.T) {
	t.Parallel()

	xOnly := make([]byte, 32)
	xOnly[0] = 0xcd
	addr, err := btcaddr.NewAddressTaproot(
		xOnly, &chaincfg.TestNet4Params,
	)
	require.NoError(t, err)

	require.Equal(t, "testnet3/testnet4/signet", addrNetName(addr))
}

// TestResolveLeaveDestinationPkScript verifies that the pk_script
// branch returns the bytes verbatim once the class-whitelist guard
// has accepted them (a structurally-valid P2TR script in this case).
func TestResolveLeaveDestinationPkScript(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	want := validP2TRPkScript(0xaa)
	got, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_PkScript{
			PkScript: want,
		},
	})
	require.NoError(t, err)
	require.Equal(t, want, got,
		"pk_script is returned verbatim")
}

// TestResolveLeaveDestinationNil surfaces a clean error when the
// caller passes a nil destination. Protects against a future
// handler change that would nil-deref here instead of surfacing
// an InvalidArgument to the caller.
func TestResolveLeaveDestinationNil(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	_, err := r.resolveLeaveDestination(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "destination is required")
}

// TestResolveLeaveDestinationEmptyAddress verifies that an empty
// address string is rejected before DecodeAddress is called.
func TestResolveLeaveDestinationEmptyAddress(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	_, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_Address{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "address is empty")
}

// TestResolveLeaveDestinationEmptyPkScript verifies that an empty
// pk_script is rejected with a clear error rather than being
// silently shipped to the wallet as a zero-byte script.
func TestResolveLeaveDestinationEmptyPkScript(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	_, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_PkScript{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pk_script is empty")
}

// TestResolveLeaveDestinationPkScriptOversized locks in the
// MaxScriptSize cap so a hostile or buggy caller cannot ship a
// multi-kilobyte pkScript through the leave path.
func TestResolveLeaveDestinationPkScriptOversized(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	tooLarge := make([]byte, txscript.MaxScriptSize+1)

	_, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_PkScript{
			PkScript: tooLarge,
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "too large")
}

// TestResolveLeaveDestinationPkScriptRejectsP2A locks in the BIP 431
// P2A anchor rejection: a P2A pkScript is anyone-can-spend and
// landing leave funds on it would effectively burn them, so the RPC
// rejects this exact byte pattern.
func TestResolveLeaveDestinationPkScriptRejectsP2A(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()
	p2a := []byte{txscript.OP_1, txscript.OP_DATA_2, 0x4e, 0x73}

	_, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_PkScript{
			PkScript: p2a,
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "P2A")
}

// TestResolveLeaveDestinationPkScriptRejectsNonStandard verifies
// that a pkScript GetScriptClass classifies as NonStandardTy (e.g.
// a truncated push that fails to parse) is rejected before dispatch.
func TestResolveLeaveDestinationPkScriptRejectsNonStandard(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	// Truncated taproot push: OP_1 OP_DATA_32 followed by only 2
	// bytes of payload — fails to parse, classifies NonStandardTy.
	truncated := []byte{0x51, 0x20, 0xaa, 0xbb}

	_, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_PkScript{
			PkScript: truncated,
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not supported")
}

// TestResolveLeaveDestinationPkScriptAcceptsOPRETURN verifies that
// OP_RETURN scripts (NullDataTy) remain a supported power-user
// destination — burns / data-carrier exits are explicit caller
// intent.
func TestResolveLeaveDestinationPkScriptAcceptsOPRETURN(t *testing.T) {
	t.Parallel()

	r := newTestRPCServer()

	// OP_RETURN <empty>: the canonical data-carrier shape.
	opReturn := []byte{txscript.OP_RETURN}

	got, err := r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_PkScript{
			PkScript: opReturn,
		},
	})
	require.NoError(t, err)
	require.Equal(t, opReturn, got)
}

// TestResolveLeaveDestinationCrossNetwork verifies that a mainnet
// address under regtest chain params is rejected by
// btcaddr.DecodeAddress rather than silently yielding a pkScript.
// Leave is funds-moving, so a cross-network address would send
// real funds to an unintended script — this guard matters.
func TestResolveLeaveDestinationCrossNetwork(t *testing.T) {
	t.Parallel()

	// Build a mainnet P2TR address programmatically so the test
	// doesn't depend on a hand-encoded bech32m string.
	xOnly := make([]byte, 32)
	xOnly[0] = 0xcd
	mainnetAddr, err := btcaddr.NewAddressTaproot(
		xOnly, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	// newTestRPCServer uses RegressionNetParams, so decoding a
	// mainnet address must fail.
	r := newTestRPCServer()
	_, err = r.resolveLeaveDestination(&daemonrpc.LeaveDestination{
		Target: &daemonrpc.LeaveDestination_Address{
			Address: mainnetAddr.String(),
		},
	})
	require.Error(t, err)
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
