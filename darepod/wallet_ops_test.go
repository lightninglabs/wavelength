package darepod

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

type testCustomInputStore struct {
	desc    *vtxo.Descriptor
	lookups []wire.OutPoint
}

func (s *testCustomInputStore) SaveVTXO(
	context.Context, *vtxo.Descriptor) error {

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

func (s *testCustomInputStore) ListLiveVTXOs(
	context.Context) ([]*vtxo.Descriptor,
	error) {

	return nil, fmt.Errorf("unexpected ListLiveVTXOs call")
}

func (s *testCustomInputStore) ListVTXOsByStatus(context.Context,
	vtxo.VTXOStatus) ([]*vtxo.Descriptor, error) {

	return nil, fmt.Errorf("unexpected ListVTXOsByStatus call")
}

func (s *testCustomInputStore) UpdateVTXOStatus(context.Context,
	wire.OutPoint, vtxo.VTXOStatus) error {

	return fmt.Errorf("unexpected UpdateVTXOStatus call")
}

func (s *testCustomInputStore) MarkForfeiting(context.Context, wire.OutPoint,
	string, *wire.MsgTx) error {

	return fmt.Errorf("unexpected MarkForfeiting call")
}

func (s *testCustomInputStore) GetForfeitTx(context.Context,
	wire.OutPoint) (*wire.MsgTx, error) {

	return nil, fmt.Errorf("unexpected GetForfeitTx call")
}

func (s *testCustomInputStore) MarkForfeited(context.Context,
	wire.OutPoint, chainhash.Hash) error {

	return fmt.Errorf("unexpected MarkForfeited call")
}

func (s *testCustomInputStore) DeleteVTXO(context.Context,
	wire.OutPoint) error {

	return fmt.Errorf("unexpected DeleteVTXO call")
}

func testVHTLCPolicyFixture(t *testing.T) (
	*arkscript.VHTLCPolicy, lntypes.Preimage, *btcec.PrivateKey,
	*btcec.PrivateKey, *btcec.PrivateKey,
) {

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
		t.Context(), store, []*daemonrpc.CustomOORInput{{
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
		t.Context(), store, []*daemonrpc.CustomOORInput{{
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
