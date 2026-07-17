package waved

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

type testCustomInputStore struct {
	desc    *vtxo.Descriptor
	lookups []wire.OutPoint
}

func (s *testCustomInputStore) SaveVTXO(context.Context,
	*vtxo.Descriptor) error {

	return fmt.Errorf("unexpected SaveVTXO call")
}

func (s *testCustomInputStore) GetVTXO(_ context.Context,
	outpoint wire.OutPoint) (*vtxo.Descriptor, error) {

	s.lookups = append(s.lookups, outpoint)

	if s.desc == nil {
		return nil, fmt.Errorf("vtxo not found")
	}

	return s.desc, nil
}

func (s *testCustomInputStore) ListLiveVTXOs(context.Context) (
	[]*vtxo.Descriptor, error) {

	return nil, fmt.Errorf("unexpected ListLiveVTXOs call")
}

func (s *testCustomInputStore) ListVTXOsByStatus(context.Context,
	vtxo.VTXOStatus) ([]*vtxo.Descriptor, error) {

	return nil, fmt.Errorf("unexpected ListVTXOsByStatus call")
}

func (s *testCustomInputStore) ListSelectionCandidatesByStatus(context.Context,
	vtxo.VTXOStatus) ([]vtxo.SelectedVTXO, error) {

	return nil, fmt.Errorf("unexpected ListSelectionCandidatesByStatus " +
		"call")
}

func (s *testCustomInputStore) UpdateVTXOStatus(context.Context, wire.OutPoint,
	vtxo.VTXOStatus) error {

	return fmt.Errorf("unexpected UpdateVTXOStatus call")
}

func (s *testCustomInputStore) UpdateVTXOStatusReleasingReservation(
	context.Context, wire.OutPoint, vtxo.VTXOStatus) error {

	return fmt.Errorf("unexpected UpdateVTXOStatusReleasingReservation " +
		"call")
}

func (s *testCustomInputStore) MarkForfeiting(context.Context, wire.OutPoint,
	string, *wire.MsgTx) error {

	return fmt.Errorf("unexpected MarkForfeiting call")
}

func (s *testCustomInputStore) GetForfeitTx(context.Context, wire.OutPoint) (
	*wire.MsgTx, error) {

	return nil, fmt.Errorf("unexpected GetForfeitTx call")
}

func (s *testCustomInputStore) MarkForfeited(context.Context, wire.OutPoint,
	chainhash.Hash, chainhash.Hash) error {

	return fmt.Errorf("unexpected MarkForfeited call")
}

func (s *testCustomInputStore) DeleteVTXO(context.Context,
	wire.OutPoint) error {

	return fmt.Errorf("unexpected DeleteVTXO call")
}

func testVHTLCPolicyFixture(t *testing.T) (*arkscript.VHTLCPolicy,
	lntypes.Preimage, *btcec.PrivateKey, *btcec.PrivateKey,
	*btcec.PrivateKey) {

	t.Helper()

	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var preimage lntypes.Preimage
	copy(preimage[:], bytes.Repeat([]byte{0x42}, len(preimage)))

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               senderPriv.PubKey(),
		Receiver:                             receiverPriv.PubKey(),
		Server:                               serverPriv.PubKey(),
		PreimageHash:                         preimage.Hash(),
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 10,
		UnilateralRefundDelay:                20,
		UnilateralRefundWithoutReceiverDelay: 30,
	})
	require.NoError(t, err)

	return policy, preimage, senderPriv, receiverPriv, serverPriv
}

func testWalletOpsOutpoint(seed byte) wire.OutPoint {
	var hashBytes [32]byte
	hashBytes[0] = seed

	return wire.OutPoint{
		Hash:  chainhash.Hash(hashBytes),
		Index: uint32(seed),
	}
}

// TestBuildCustomTransferInputsExternalVHTLCClaim verifies that callers can
// provide an external vHTLC output plus explicit claim spend path without
// consulting the local VTXO store.
func TestBuildCustomTransferInputsExternalVHTLCClaim(t *testing.T) {
	t.Parallel()

	policy, preimage, _, receiverPriv, serverPriv :=
		testVHTLCPolicyFixture(t)
	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	claimPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)

	spendPath, err := claimPath.Encode()
	require.NoError(t, err)

	clientKey := keychain.KeyDescriptor{
		PubKey: receiverPriv.PubKey(),
		KeyLocator: keychain.KeyLocator{
			Family: 1,
			Index:  2,
		},
	}

	store := &testCustomInputStore{}
	outpoint := testWalletOpsOutpoint(1)

	inputs, err := BuildCustomTransferInputs(
		t.Context(), store, []*waverpc.CustomOORInput{{
			Outpoint:           outpoint.String(),
			VtxoPolicyTemplate: policyTemplate,
			SpendPath:          spendPath,
			AmountSat:          42_000,
			PkScript:           pkScript,
		}}, clientKey, serverPriv.PubKey(), 144,
	)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Empty(t, store.lookups)

	input := inputs[0]
	require.Equal(t, outpoint, input.VTXO.Outpoint)
	require.Equal(t, btcutil.Amount(42_000), input.VTXO.Amount)
	require.Equal(t, pkScript, input.VTXO.PkScript)
	require.Equal(t, policyTemplate, input.VTXOPolicyTemplate)
	require.True(t, input.VTXO.ClientKey.PubKey.IsEqual(clientKey.PubKey))
	require.True(t, input.VTXO.OperatorKey.IsEqual(serverPriv.PubKey()))
	require.NoError(t, input.Validate())

	effective, err := input.EffectiveSpendPath()
	require.NoError(t, err)

	effectiveRaw, err := effective.Encode()
	require.NoError(t, err)
	require.Equal(t, spendPath, effectiveRaw)
}

// TestBuildCustomTransferInputsStoreLookupVHTLCClaim verifies that custom
// inputs can still use the local VTXO store when the caller only supplies
// semantic policy and spend-path metadata.
func TestBuildCustomTransferInputsStoreLookupVHTLCClaim(t *testing.T) {
	t.Parallel()

	policy, preimage, _, receiverPriv, serverPriv :=
		testVHTLCPolicyFixture(t)
	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	claimPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)

	spendPath, err := claimPath.Encode()
	require.NoError(t, err)

	outpoint := testWalletOpsOutpoint(2)
	clientKey := keychain.KeyDescriptor{
		PubKey: receiverPriv.PubKey(),
		KeyLocator: keychain.KeyLocator{
			Family: 3,
			Index:  4,
		},
	}

	store := &testCustomInputStore{
		desc: &vtxo.Descriptor{
			Outpoint:       outpoint,
			Amount:         btcutil.Amount(55_000),
			PolicyTemplate: policyTemplate,
			PkScript:       pkScript,
			ClientKey:      clientKey,
			OperatorKey:    serverPriv.PubKey(),
			TapScript: &waddrmgr.Tapscript{
				Type: waddrmgr.TapscriptTypeFullTree,
			},
		},
	}

	inputs, err := BuildCustomTransferInputs(
		t.Context(), store, []*waverpc.CustomOORInput{{
			Outpoint:           outpoint.String(),
			VtxoPolicyTemplate: policyTemplate,
			SpendPath:          spendPath,
		}}, clientKey, serverPriv.PubKey(), 144,
	)
	require.NoError(t, err)
	require.Len(t, inputs, 1)
	require.Equal(t, []wire.OutPoint{outpoint}, store.lookups)

	input := inputs[0]
	require.Equal(t, btcutil.Amount(55_000), input.VTXO.Amount)
	require.Equal(t, pkScript, input.VTXO.PkScript)
	require.Equal(t, policyTemplate, input.VTXOPolicyTemplate)
	require.NoError(t, input.Validate())

	effective, err := input.EffectiveSpendPath()
	require.NoError(t, err)

	effectiveRaw, err := effective.Encode()
	require.NoError(t, err)
	require.Equal(t, spendPath, effectiveRaw)
}

// TestReserveCustomInputsRejectsDoubleUse verifies that the in-memory
// custom-input reservation map refuses to hand out the same outpoint to
// two concurrent SendOOR calls, closing the M-1 race window.
func TestReserveCustomInputsRejectsDoubleUse(t *testing.T) {
	t.Parallel()

	srv := &RPCServer{
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}

	op := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("m1-test-vtxo")),
		Index: 0,
	}

	// First caller claims the outpoint.
	release, err := srv.reserveCustomInputs([]wire.OutPoint{op})
	require.NoError(t, err)
	require.NotNil(t, release)

	// Concurrent second caller must see the reservation and fail
	// rather than silently succeed.
	_, err = srv.reserveCustomInputs([]wire.OutPoint{op})
	require.ErrorContains(t, err, "already reserved")

	// After release, the outpoint is re-usable.
	release()

	release2, err := srv.reserveCustomInputs([]wire.OutPoint{op})
	require.NoError(t, err)
	release2()
}

// TestSpendPathsMatchIgnoresConditionWitness verifies semantic path matching
// does not treat runtime witness items as part of the authenticated branch
// identity.
func TestSpendPathsMatchIgnoresConditionWitness(t *testing.T) {
	t.Parallel()

	base := &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{
				txscript.OP_TRUE,
			},
			ControlBlock: bytes.Repeat([]byte{0x01}, 33),
		},
		RequiredSequence: 144,
		RequiredLockTime: 42,
		Conditions: [][]byte{
			[]byte("first"),
		},
	}

	other := &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: bytes.Clone(base.WitnessScript),
			ControlBlock:  bytes.Clone(base.ControlBlock),
		},
		RequiredSequence: base.RequiredSequence,
		RequiredLockTime: base.RequiredLockTime,
		Conditions: [][]byte{
			[]byte("different"),
			[]byte("witness"),
		},
	}

	require.True(t, spendPathsMatch(base, other))
}

// TestReserveCustomInputsAtomicOnCollision verifies that when a batch
// reservation includes any already-reserved outpoint, none of the other
// outpoints in the batch are claimed — the reservation is all-or-nothing.
// Without this property a partial reservation could leak into the map and
// block subsequent unrelated callers.
func TestReserveCustomInputsAtomicOnCollision(t *testing.T) {
	t.Parallel()

	srv := &RPCServer{
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}

	op1 := wire.OutPoint{Hash: chainhash.HashH([]byte("op1")), Index: 0}
	op2 := wire.OutPoint{Hash: chainhash.HashH([]byte("op2")), Index: 0}
	op3 := wire.OutPoint{Hash: chainhash.HashH([]byte("op3")), Index: 0}

	// First caller claims op2 alone.
	release, err := srv.reserveCustomInputs([]wire.OutPoint{op2})
	require.NoError(t, err)
	defer release()

	// Second caller attempts [op1, op2, op3]. op2 collides and the
	// whole batch must fail; op1 and op3 must NOT end up locked.
	_, err = srv.reserveCustomInputs(
		[]wire.OutPoint{op1, op2, op3},
	)
	require.Error(t, err)

	// op1 and op3 should be available to a fresh caller.
	release2, err := srv.reserveCustomInputs(
		[]wire.OutPoint{op1, op3},
	)
	require.NoError(t, err)
	release2()
}

// TestBuildCustomTransferInputsUsesLocalOperatorPolicyLeaf verifies that a
// policy-backed custom spend preserves an exact semantic policy leaf when the
// checkpoint owner path remains signable by the local client and operator.
func TestBuildCustomTransferInputsUsesLocalOperatorPolicyLeaf(t *testing.T) {
	t.Parallel()

	policy, preimage, _, receiverPriv, serverPriv :=
		testVHTLCPolicyFixture(t)

	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	claimPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)

	spendPath, err := claimPath.Encode()
	require.NoError(t, err)

	outpoint := testWalletOpsOutpoint(3)
	clientKey := keychain.KeyDescriptor{
		PubKey: receiverPriv.PubKey(),
		KeyLocator: keychain.KeyLocator{
			Family: 5,
			Index:  6,
		},
	}

	inputs, err := BuildCustomTransferInputs(
		t.Context(), &testCustomInputStore{},
		[]*waverpc.CustomOORInput{{
			Outpoint:           outpoint.String(),
			VtxoPolicyTemplate: policyTemplate,
			SpendPath:          spendPath,
			AmountSat:          42_000,
			PkScript:           pkScript,
		}}, clientKey, serverPriv.PubKey(), 144,
	)
	require.NoError(t, err)
	require.Len(t, inputs, 1)

	input := inputs[0]
	require.Equal(t, claimPath.WitnessScript, input.OwnerLeafScript)
	require.NotEmpty(t, input.OwnerLeafPolicy)

	ownerLeaf, err := arkscript.DecodeLeafTemplate(input.OwnerLeafPolicy)
	require.NoError(t, err)

	ownerLeafScript, err := ownerLeaf.Script()
	require.NoError(t, err)
	require.Equal(t, claimPath.WitnessScript, ownerLeafScript)
}

// TestBuildCustomTransferInputsDefaultsMultiPartyRefundOwnerLeaf verifies that
// a custom spend which needs an extra participant signature uses a standard
// local/operator checkpoint owner leaf after the custom input spend itself.
func TestBuildCustomTransferInputsDefaultsMultiPartyRefundOwnerLeaf(
	t *testing.T) {

	t.Parallel()

	policy, _, senderPriv, _, serverPriv := testVHTLCPolicyFixture(t)

	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	refundPath, err := policy.RefundPath()
	require.NoError(t, err)

	spendPath, err := refundPath.Encode()
	require.NoError(t, err)

	outpoint := testWalletOpsOutpoint(4)
	clientKey := keychain.KeyDescriptor{
		PubKey: senderPriv.PubKey(),
		KeyLocator: keychain.KeyLocator{
			Family: 7,
			Index:  8,
		},
	}

	inputs, err := BuildCustomTransferInputs(
		t.Context(), &testCustomInputStore{},
		[]*waverpc.CustomOORInput{{
			Outpoint:           outpoint.String(),
			VtxoPolicyTemplate: policyTemplate,
			SpendPath:          spendPath,
			AmountSat:          42_000,
			PkScript:           pkScript,
		}}, clientKey, serverPriv.PubKey(), 144,
	)
	require.NoError(t, err)
	require.Len(t, inputs, 1)

	input := inputs[0]
	require.Empty(t, input.OwnerLeafScript)
	require.Empty(t, input.OwnerLeafPolicy)
	require.NoError(t, input.Validate())

	defaultLeaf, err := arkscript.MultiSigCollabTapLeaf(
		senderPriv.PubKey(), serverPriv.PubKey(),
	)
	require.NoError(t, err)
	require.Equal(t, defaultLeaf.Script, input.OwnerLeafScript)
	require.NotEqual(t, refundPath.WitnessScript, input.OwnerLeafScript)
}

// TestFindSettlementOwnerLeafWithConditions verifies that a caller's
// spend path is resolved correctly even when it carries runtime
// condition witness items. The spend-path identity is defined by the
// authenticated branch (witness script, control block, sequence,
// locktime) only; condition witnesses are transient and must not
// perturb leaf selection.
//
// This pins the contract that motivated stripping Conditions from
// spendPathsMatch: without that change, the defensive second-stage
// branch of findSettlementOwnerLeaf would silently fail for any
// future policy whose auth leaf is not directly enumerated in
// template.Leaves and requires a preimage-style condition witness.
func TestFindSettlementOwnerLeafWithConditions(t *testing.T) {
	t.Parallel()

	policy, preimage, _, receiverPriv, serverPriv :=
		testVHTLCPolicyFixture(t)

	claimPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)
	require.NotEmpty(
		t, claimPath.Conditions,
		"vHTLC claim path should carry a preimage condition",
	)

	raw, err := claimPath.Encode()
	require.NoError(t, err)

	ownerLeaf, ownerLeafPolicy, err := findSettlementOwnerLeaf(
		policy.Template, receiverPriv.PubKey(), serverPriv.PubKey(),
		raw,
	)
	require.NoError(t, err)
	require.Equal(t, claimPath.WitnessScript, ownerLeaf)
	require.NotEmpty(t, ownerLeafPolicy)
}
