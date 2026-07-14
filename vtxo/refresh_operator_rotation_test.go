package vtxo

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/round"
	"github.com/stretchr/testify/require"
)

// xOnlyEqual compares two secp256k1 keys by their Schnorr (x-only)
// serialization, which is the form Ark policy templates commit to.
// Comparing full pubkeys with IsEqual would spuriously fail when the
// y-coordinate parity differs.
func xOnlyEqual(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}

	return bytes.Equal(
		schnorr.SerializePubKey(a), schnorr.SerializePubKey(b),
	)
}

// TestRefreshEmissionUsesJoinTimeOperatorKey pins the contract that the
// auto-refresh emission must build the NEW VTXO output's policy template
// against the operator key the client resolves at join time (via a fresh
// GetInfo round-trip), not against the input VTXO's stored key. VTXOs
// commit to their operator key for life; the new output is itself a fresh
// VTXO, so its operator key is chosen at join time and is stable on that
// new VTXO forever.
//
// The test exercises four behaviours through subtests:
//
//   - With a fetch callback that returns K2, the emitted template
//     commits to K2 and not the descriptor's K1.
//   - With no fetch callback wired, the emission falls back to the
//     descriptor's stored bytes (harness path / non-standard policies).
//   - When the fetch callback errors, refreshOutputTemplate propagates
//     the error so the actor surfaces a failed refresh instead of
//     silently emitting against a stale key.
//   - When the fetch callback returns a nil key, the same propagation
//     rule applies.
func TestRefreshEmissionUsesJoinTimeOperatorKey(t *testing.T) {
	t.Parallel()

	t.Run("rebuilds against fetched key", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()

		_, k2 := generateTestKeyPair(t)
		require.False(
			t, xOnlyEqual(vtxo.OperatorKey, k2),
			"sanity: fetched key must differ from descriptor's "+
				"stored key",
		)

		manager := newMockManagerRef(t)
		actor := newRefreshTestActor(
			h, vtxo, manager,
			func(_ context.Context) (*btcec.PublicKey, error) {
				return k2, nil
			},
		)

		refreshReq := drainRefreshEmission(t, h, actor, manager, vtxo)
		params := decodeStandardParams(t, refreshReq.PolicyTemplate)

		require.True(
			t, xOnlyEqual(params.OperatorKey, k2),
			"emitted template must commit to the operator key "+
				"fetched at join time",
		)
		require.False(
			t, xOnlyEqual(params.OperatorKey, vtxo.OperatorKey),
			"emitted template must no longer carry the "+
				"descriptor's stale K1",
		)
	})

	t.Run("falls back to stored template when fetch unset",
		func(t *testing.T) {
			t.Parallel()

			h := newVTXOTestHarness(t)
			vtxo := h.newTestDescriptor()

			manager := newMockManagerRef(t)
			actor := newRefreshTestActor(h, vtxo, manager, nil)

			refreshReq := drainRefreshEmission(
				t, h, actor, manager, vtxo,
			)
			params := decodeStandardParams(
				t, refreshReq.PolicyTemplate,
			)

			require.True(
				t, xOnlyEqual(
					params.OperatorKey, vtxo.OperatorKey,
				),
				"with no fetch wired the actor must keep "+
					"the descriptor's stored template "+
					"(legacy fallback)",
			)
		})

	t.Run("propagates fetch error", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()

		fetchErr := errors.New("operator unreachable")
		manager := newMockManagerRef(t)
		actor := newRefreshTestActor(
			h, vtxo, manager,
			func(_ context.Context) (*btcec.PublicKey, error) {
				return nil, fetchErr
			},
		)

		_, err := actor.refreshOutputTemplate(h.ctx, vtxo)
		require.ErrorIs(
			t, err, fetchErr, "fetch error must propagate so "+
				"the refresh fails instead of silently "+
				"emitting against a stale key",
		)
	})

	t.Run("rejects nil fetched key", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()

		manager := newMockManagerRef(t)
		actor := newRefreshTestActor(
			h, vtxo, manager,
			func(_ context.Context) (*btcec.PublicKey, error) {
				return nil, nil
			},
		)

		_, err := actor.refreshOutputTemplate(h.ctx, vtxo)
		require.Error(
			t, err, "a fetch callback that returns a nil key "+
				"must fail the refresh, not silently fall back",
		)
	})
}

// fetchOperatorKeyFn is the local alias for the FetchOperatorKey callback
// signature; it keeps the newRefreshTestActor signature within the 80-col
// line budget.
type fetchOperatorKeyFn = func(context.Context) (*btcec.PublicKey, error)

// newRefreshTestActor builds a minimal VTXOActor in LiveState wired to the
// shared test harness. fetchOperatorKey may be nil to exercise the
// legacy fallback path.
func newRefreshTestActor(h *vtxoTestHarness, vtxo *Descriptor,
	manager *mockManagerRef,
	fetchOperatorKey fetchOperatorKeyFn) *VTXOActor {

	return &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:             vtxo,
			Store:            h.store,
			Wallet:           h.wallet,
			ChainParams:      &chaincfg.RegressionNetParams,
			Manager:          manager,
			FetchOperatorKey: fetchOperatorKey,
		},
		state: &LiveState{
			VTXO: vtxo,
		},
		env: h.env,
	}
}

// drainRefreshEmission feeds a single ForfeitRequest through the actor's
// processOutbox and returns the resulting RefreshVTXORequest the manager
// received.
func drainRefreshEmission(t *testing.T, h *vtxoTestHarness, actor *VTXOActor,
	manager *mockManagerRef, vtxo *Descriptor) *round.RefreshVTXORequest {

	t.Helper()

	require.NoError(
		t,
		actor.processOutbox(
			h.ctx, []VTXOOutMsg{
				&ForfeitRequest{VTXOOutpoint: vtxo.Outpoint},
			},
		),
	)

	msgs := manager.getMessages()
	require.Len(t, msgs, 1, "expected exactly one relayed message")

	relayMsg, ok := msgs[0].(*RelayToRoundMsg)
	require.True(t, ok, "expected RelayToRoundMsg, got %T", msgs[0])

	refreshReq, ok := relayMsg.Payload.(*round.RefreshVTXORequest)
	require.True(
		t, ok, "expected RefreshVTXORequest, got %T", relayMsg.Payload,
	)

	return refreshReq
}

// decodeStandardParams decodes a serialized standard VTXO policy template
// into its (owner, operator, exit-delay) triple.
func decodeStandardParams(t *testing.T,
	raw []byte) *arkscript.StandardVTXOParams {

	t.Helper()

	template, err := arkscript.DecodePolicyTemplate(raw)
	require.NoError(t, err, "decode policy template")

	params, err := arkscript.DecodeStandardVTXOParams(template)
	require.NoError(t, err, "decode standard VTXO params")

	return params
}
