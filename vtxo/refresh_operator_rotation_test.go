package vtxo

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/round"
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

// TestRefreshEmissionUsesOperatorPlaceholder pins the contract that the
// auto-refresh emission builds the NEW VTXO output's policy template with the
// operator-key placeholder rather than any concrete operator key. The server
// binds its current key at admission, which removes the refresh-after-rotation
// problem at the root: the new output never commits to the input VTXO's stored
// (or any) concrete operator key on the client side.
func TestRefreshEmissionUsesOperatorPlaceholder(t *testing.T) {
	t.Parallel()

	t.Run("rebuilds with operator placeholder", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()

		manager := newMockManagerRef(t)
		actor := newRefreshTestActor(h, vtxo, manager)

		refreshReq := drainRefreshEmission(t, h, actor, manager, vtxo)
		params := decodeStandardParams(t, refreshReq.PolicyTemplate)

		require.True(
			t, xOnlyEqual(
				params.OperatorKey,
				&arkscript.OperatorKeyPlaceholder,
			),
			"emitted template must commit to the operator "+
				"placeholder, not a concrete key",
		)
		require.False(
			t, xOnlyEqual(params.OperatorKey, vtxo.OperatorKey),
			"emitted template must no longer carry the "+
				"descriptor's stored operator key",
		)
	})

	t.Run("rejects custom shape", func(t *testing.T) {
		t.Parallel()

		h := newVTXOTestHarness(t)
		vtxo := h.newTestDescriptor()
		vtxo.PolicyTemplate = []byte{
			0x00,
		}

		manager := newMockManagerRef(t)
		actor := newRefreshTestActor(h, vtxo, manager)

		_, err := actor.refreshOutputTemplate(h.ctx, vtxo)
		require.True(
			t, errors.Is(err, ErrRefreshOperatorKeyUnsupported),
		)
	})
}

// newRefreshTestActor builds a minimal VTXOActor in LiveState wired to the
// shared test harness.
func newRefreshTestActor(h *vtxoTestHarness, vtxo *Descriptor,
	manager *mockManagerRef) *VTXOActor {

	return &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        vtxo,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     manager,
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
