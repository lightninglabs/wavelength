package waved

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// sourceLifetimeBackend returns a live tip independently of serialized inputs.
type sourceLifetimeBackend struct {
	chainsource.ChainBackend
	height int32
	err    error
	calls  int
}

// BestBlock records each admission's fresh backend query.
func (b *sourceLifetimeBackend) BestBlock(context.Context) (int32,
	chainhash.Hash, error) {

	b.calls++

	return b.height, chainhash.Hash{}, b.err
}

// sourceLifetimeFixture holds saved metadata and truncated durable inputs.
type sourceLifetimeFixture struct {
	server   *Server
	terms    arkchannel.Terms
	backend  *sourceLifetimeBackend
	operator *btcec.PublicKey
	inputs   []oor.TransferInput
}

// fundingProofs supplies the already-held wallet lineage, not an unsigned
// substitute for the new policy output's eventual recovery package.
type fundingProofs map[wire.OutPoint]*recovery.Proof

// EnsureProof returns only a proof for the requested wallet input.
func (p fundingProofs) EnsureProof(_ context.Context, out wire.OutPoint) (
	*recovery.Proof, error) {

	if proof, ok := p[out]; ok {
		return proof, nil
	}

	return nil, fmt.Errorf("wallet lineage unavailable for %s", out)
}

// TestPreparedFundingRecoveryWindow includes the unsigned checkpoint and Ark
// transaction before commit and uses the channel CSV, not the input's CSV.
func TestPreparedFundingRecoveryWindow(t *testing.T) {
	t.Parallel()

	fixture := newSourceLifetimeFixture(t, 1000)
	desc := fixture.inputs[0].VTXO
	parent := linkedTx(wire.OutPoint{Hash: chainhash.Hash{9}})
	parent.TxOut[0] = wire.NewTxOut(int64(desc.Amount), desc.PkScript)
	desc.Outpoint = outpointOf(t, parent)
	proof, err := recovery.NewProof(
		desc.Outpoint, 900, &recovery.Node{
			Tx: parent,
		},
	)
	require.NoError(t, err)
	proofs := fundingProofs{desc.Outpoint: proof}
	session, _, err := oor.NewPreparedSessionWithIdempotencyKey(
		t.Context(), arkscript.CheckpointPolicy{
			OperatorKey: fixture.operator, CSVDelay: 10,
		}, fixture.inputs, []oortx.RecipientOutput{{
			PkScript: desc.PkScript, Value: desc.Amount,
			VTXOPolicyTemplate: desc.PolicyTemplate,
		}}, "funding-depth", oor.EnvConfig{},
	)
	require.NoError(t, err)
	t.Cleanup(session.FSM.Stop)
	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	prepared, ok := state.(*oor.Prepared)
	require.True(t, ok)
	target := wire.OutPoint{Hash: chainhash.Hash(session.ID)}
	projected, err := projectChannelFundingProof(
		t.Context(), proofs, target, 10, prepared,
	)
	require.NoError(t, err)
	require.Len(t, projected.Layers(), 3)
	require.NoError(t, validateArkChannelRecoveryWindow(projected, 14, 100))
	require.ErrorContains(
		t, validateArkChannelRecoveryWindow(projected, 13, 100),
		"needs 14",
	)
	_, err = projectChannelFundingProof(
		t.Context(), proofs, target, recovery.MaxCSVDelay+1, prepared,
	)
	require.ErrorIs(t, err, arkchannel.ErrSourceInadmissible)

	delete(proofs, desc.Outpoint)
	_, err = projectChannelFundingProof(
		t.Context(), proofs, target, 10, prepared,
	)
	require.ErrorContains(t, err, "wallet lineage unavailable")
	require.NotErrorIs(t, err, arkchannel.ErrSourceInadmissible)
}

// newSourceLifetimeFixture uses the production store and durable request codec
// so admission cannot accidentally rely on metadata absent after serialization.
func newSourceLifetimeFixture(t *testing.T,
	expiry int32) *sourceLifetimeFixture {

	t.Helper()
	store, _, _, _, _ := newSendOORTestStores(t)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	desc, _ := newSendOORTestVTXO(t, operator.PubKey(), 61, 100_000)
	desc.BatchExpiry = expiry
	require.NoError(t, store.SaveVTXO(t.Context(), desc))
	inputs, err := BuildTransferInputs(
		t.Context(), store, []wire.OutPoint{desc.Outpoint},
	)
	require.NoError(t, err)
	backend := &sourceLifetimeBackend{height: 859}

	return &sourceLifetimeFixture{
		server: &Server{
			vtxoStore:    store,
			chainBackend: backend,
		},
		terms: arkchannel.Terms{
			VTXO: arkchannel.VTXOTerms{
				FunderDelay: 100,
			},
		},
		backend: backend, operator: operator.PubKey(),
		inputs: durableSourceLifetimeInputs(
			t, operator.PubKey(), inputs,
		),
	}
}

// durableSourceLifetimeInputs reproduces the signing-only descriptor seen by
// prepared commit after the ordinary durable actor message serialization.
func durableSourceLifetimeInputs(t *testing.T, operator *btcec.PublicKey,
	inputs []oor.TransferInput) []oor.TransferInput {

	t.Helper()
	request := &oor.StartTransferRequest{
		Policy: arkscript.CheckpointPolicy{
			OperatorKey: operator, CSVDelay: 10,
		},
		Inputs: inputs, PrepareOnly: true,
	}
	var raw bytes.Buffer
	require.NoError(t, request.Encode(&raw))
	var decoded oor.StartTransferRequest
	require.NoError(t, decoded.Decode(&raw))
	require.Len(t, decoded.Inputs, len(inputs))
	for _, input := range decoded.Inputs {
		require.Zero(t, input.VTXO.BatchExpiry)
	}

	return decoded.Inputs
}

// TestArkChannelSourceLifetimeRestoredSnapshot checks the canonical outgoing
// signing snapshot codec, not just the initial durable request. The original
// input has expiry, but a decoded snapshot must reload it from the VTXO store.
func TestArkChannelSourceLifetimeRestoredSnapshot(t *testing.T) {
	t.Parallel()

	fixture := newSourceLifetimeFixture(t, 1000)
	outpoint := fixture.inputs[0].VTXO.Outpoint
	inputs, err := BuildTransferInputs(
		t.Context(), fixture.server.vtxoStore,
		[]wire.OutPoint{outpoint},
	)
	require.NoError(t, err)
	desc := inputs[0].VTXO
	require.Equal(t, int32(1000), desc.BatchExpiry)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: fixture.operator, CSVDelay: 10,
	}
	session, _, err := oor.NewPreparedSessionWithIdempotencyKey(
		t.Context(), policy, inputs, []oortx.RecipientOutput{{
			PkScript: desc.PkScript, Value: desc.Amount,
			VTXOPolicyTemplate: desc.PolicyTemplate,
		}}, "source-lifetime-snapshot", oor.EnvConfig{},
	)
	require.NoError(t, err)
	t.Cleanup(session.FSM.Stop)
	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	outgoing, ok := state.(oor.State)
	require.True(t, ok)
	snapshot, err := oor.NewOutgoingSnapshot(session.ID, outgoing)
	require.NoError(t, err)
	request := &oor.RestoreSessionRequest{Snapshot: snapshot}
	var raw bytes.Buffer
	require.NoError(t, request.Encode(&raw))
	var decoded oor.RestoreSessionRequest
	require.NoError(t, decoded.Decode(&raw))
	require.Empty(t, decoded.Snapshot.TransferInputs)
	restored, err := oor.OutgoingStateFromSnapshot(decoded.Snapshot)
	require.NoError(t, err)
	prepared, ok := restored.(*oor.Prepared)
	require.True(t, ok)
	require.Len(t, prepared.TransferInputs, 1)
	durable := prepared.TransferInputs[0].VTXO
	require.Equal(t, outpoint, durable.Outpoint)
	require.Zero(t, durable.BatchExpiry)
	require.NoError(
		t,
		fixture.server.validateArkChannelSourceLifetime(
			t.Context(), fixture.terms, prepared.TransferInputs,
		),
	)
}

// TestArkChannelSourceLifetimeUsesSavedExpiry admits truncated durable inputs
// only when the saved expiry is safe, ignoring both absent and spoofed
// metadata.
func TestArkChannelSourceLifetimeUsesSavedExpiry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		savedExpiry   int32
		claimedExpiry int32
		height        int32
		wantErr       string
	}{
		{
			name: "durable expiry omitted", savedExpiry: 1000,
			height: 859,
		},
		{
			name: "stale input metadata", savedExpiry: 1000,
			claimedExpiry: 1, height: 859,
		},
		{
			name: "deadline", savedExpiry: 1000,
			claimedExpiry: math.MaxInt32, height: 860,
			wantErr: "lifetime exhausted",
		},
		{
			name: "expired", savedExpiry: 1000,
			claimedExpiry: math.MaxInt32, height: 1000,
			wantErr: "lifetime exhausted",
		},
		{
			name:          "missing saved expiry",
			claimedExpiry: math.MaxInt32, height: 1,
			wantErr: "batch expiry must be positive",
		},
		{
			name: "invalid saved expiry", savedExpiry: -1,
			claimedExpiry: math.MaxInt32, height: 1,
			wantErr: "batch expiry must be positive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newSourceLifetimeFixture(t, test.savedExpiry)
			fixture.backend.height = test.height
			input := fixture.inputs[0].VTXO
			input.BatchExpiry = test.claimedExpiry
			err := fixture.server.validateArkChannelSourceLifetime(
				t.Context(), fixture.terms, fixture.inputs,
			)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, fixture.backend.calls)
		})
	}
}

// TestArkChannelSourceLifetimeUsesExactInputs checks every selected outpoint,
// including different outputs of the same transaction, in either input order.
func TestArkChannelSourceLifetimeUsesExactInputs(t *testing.T) {
	t.Parallel()

	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()

			fixture := newSourceLifetimeFixture(t, 1000)
			first := fixture.inputs[0].VTXO.Outpoint
			second, _ := newSendOORTestVTXO(
				t, fixture.operator, 62, 100_000,
			)
			second.Outpoint.Hash = first.Hash
			second.BatchExpiry = 900
			store := fixture.server.vtxoStore
			require.NoError(t, store.SaveVTXO(t.Context(), second))
			selected := []wire.OutPoint{first, second.Outpoint}
			if reverse {
				slices.Reverse(selected)
			}
			inputs, err := BuildTransferInputs(
				t.Context(), store, selected,
			)
			require.NoError(t, err)
			inputs = durableSourceLifetimeInputs(
				t, fixture.operator, inputs,
			)
			fixture.backend.height = 760
			err = fixture.server.validateArkChannelSourceLifetime(
				t.Context(), fixture.terms, inputs,
			)
			require.ErrorContains(t, err, "batch expiry 900")
			fixture.backend.height--
			require.NoError(
				t,
				fixture.server.validateArkChannelSourceLifetime(
					t.Context(), fixture.terms, inputs,
				),
			)
			require.Equal(t, 2, fixture.backend.calls)
		})
	}
}

// TestArkChannelSourceLifetimeFailsClosed never substitutes serialized expiry
// when the authoritative row, store, descriptor, or live chain tip is missing.
func TestArkChannelSourceLifetimeFailsClosed(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{
		"missing row", "missing store", "missing backend",
		"backend error", "negative height", "zero allowance",
		"no inputs", "nil descriptor",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			fixture := newSourceLifetimeFixture(t, 1000)
			fixture.inputs[0].VTXO.BatchExpiry = math.MaxInt32
			switch scenario {
			case "missing row":
				fixture.inputs[0].VTXO.Outpoint.Index++

			case "missing store":
				fixture.server.vtxoStore = nil

			case "missing backend":
				fixture.server.chainBackend = nil

			case "backend error":
				fixture.backend.err = fmt.Errorf("backend " +
					"unavailable")

			case "negative height":
				fixture.backend.height = -1

			case "zero allowance":
				fixture.terms.VTXO.FunderDelay = 0

			case "no inputs":
				fixture.inputs = nil

			case "nil descriptor":
				fixture.inputs[0].VTXO = nil
			}
			err := fixture.server.validateArkChannelSourceLifetime(
				t.Context(), fixture.terms, fixture.inputs,
			)
			require.Error(t, err)
			if scenario == "missing row" {
				require.ErrorIs(t, err, vtxo.ErrVTXONotFound)
			}
			if scenario == "backend error" {
				require.ErrorIs(t, err, fixture.backend.err)
			}
		})
	}
}
