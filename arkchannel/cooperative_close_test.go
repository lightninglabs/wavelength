package arkchannel

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestCooperativeCloseSettlesChannelVTXODirectly proves the immediate
// three-party policy preserves lnd balances and spends the VTXO rather than the
// unpublished channel point.
func TestCooperativeCloseSettlesChannelVTXODirectly(t *testing.T) {
	t.Parallel()

	terms, source, request, clientKey, hubKey, operatorKey :=
		testCooperativeCloseFixture(t, KindPromotion, 5_000)
	const (
		clientBalance = btcutil.Amount(63_000)
		hubBalance    = btcutil.Amount(37_000)
	)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, clientBalance, hubBalance, 7,
	)
	require.NoError(t, err)
	proposal := template.Proposal()
	require.NoError(t, proposal.Validate(terms, source, request))
	require.Equal(
		t, terms.Capacity, proposal.ClientBalance+proposal.HubBalance,
	)
	require.Equal(
		t, source.Amount-proposal.Fee,
		proposal.ClientOutput+proposal.HubOutput,
	)
	require.Equal(
		t, clientBalance+5_000-proposal.Fee, proposal.ClientOutput,
	)
	require.Equal(t, hubBalance, proposal.HubOutput)

	clientSig := signCooperativeClose(
		t, template, terms, PartyClient, clientKey,
	)
	require.NoError(
		t, template.VerifySignature(terms, PartyClient, clientSig),
	)
	hubSig := signCooperativeClose(
		t, template, terms, PartyHub, hubKey,
	)
	require.NoError(t, template.VerifySignature(terms, PartyHub, hubSig))
	wrongSig := signCooperativeClose(
		t, template, terms, PartyHub, hubKey,
	)
	require.ErrorContains(
		t, template.VerifySignature(terms, PartyClient, wrongSig),
		"invalid client cooperative close signature",
	)
	operatorDesc, err := template.OperatorSignDescriptor(
		terms, keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	)
	require.NoError(t, err)
	operatorSig, err := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	).SignOutputRaw(
		mustDecodeCooperativeClose(
			t, proposal.Transaction,
		), operatorDesc,
	)
	require.NoError(t, err)

	settlement, err := template.Complete(
		terms, source, request, clientSig, hubSig, operatorSig,
	)
	require.NoError(t, err)
	require.NoError(t, settlement.Validate(terms, source, request))
	tx := mustDecodeCooperativeClose(t, settlement.Transaction)
	require.Equal(t, source.OutPoint, tx.TxIn[0].PreviousOutPoint)
	require.NotEqual(
		t, testBacking(t, terms, source).ChannelPoint,
		tx.TxIn[0].PreviousOutPoint,
	)
	require.Equal(t, settlement.TxID, tx.TxHash())
	require.Len(t, tx.TxIn[0].Witness, 5)
}

// TestCooperativeCloseReturnsReserveToHubFunder verifies the fee reserve never
// changes the lnd balance allocation and follows the original OOR funder.
func TestCooperativeCloseReturnsReserveToHubFunder(t *testing.T) {
	t.Parallel()

	terms, source, request, _, _, _ := testCooperativeCloseFixture(
		t, KindReceiveIntent, 4_000,
	)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 25_000, 75_000, 11,
	)
	require.NoError(t, err)
	proposal := template.Proposal()
	require.EqualValues(t, 25_000, proposal.ClientOutput)
	require.Equal(
		t, btcutil.Amount(79_000)-proposal.Fee, proposal.HubOutput,
	)
}

// TestCooperativeCloseRejectsUnsafeSettlement verifies peers cannot sign a
// changed payout or consume ordinary channel liquidity for the target fee.
func TestCooperativeCloseRejectsUnsafeSettlement(t *testing.T) {
	t.Parallel()

	terms, source, request, _, _, _ := testCooperativeCloseFixture(
		t, KindReceiveIntent, 2_000,
	)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 40_000, 60_000, 4,
	)
	require.NoError(t, err)
	changed := template.Proposal()
	changed.ClientOutput++
	require.ErrorContains(
		t, changed.Validate(terms, source, request),
		"not canonical",
	)

	dustTemplate, err := NewCooperativeCloseTemplate(
		terms, source, request, 1, terms.Capacity-1, 4,
	)
	require.NoError(t, err)
	dustProposal := dustTemplate.Proposal()
	require.Zero(t, dustProposal.ClientOutput)
	require.Equal(t, source.Amount,
		dustProposal.HubOutput+dustProposal.Fee)

	tooSmall := resizeCooperativeCloseSource(t, source, terms.Capacity+1)
	_, err = NewCooperativeCloseTemplate(
		terms, tooSmall, request, 40_000, 60_000, 4,
	)
	require.ErrorContains(t, err, "exceeds reserve")
}

// TestCooperativeCloseRejectsWrongPolicyKey verifies role checking happens
// before a signature request reaches the wallet signer.
func TestCooperativeCloseRejectsWrongPolicyKey(t *testing.T) {
	t.Parallel()

	terms, source, request, _, _, _ := testCooperativeCloseFixture(
		t, KindPromotion, 2_000,
	)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 50_000, 50_000, 1,
	)
	require.NoError(t, err)
	wrongKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	_, err = template.SignDescriptor(
		terms, PartyClient, keychain.KeyDescriptor{
			PubKey: wrongKey.PubKey(),
		},
	)
	require.ErrorContains(t, err, "does not match policy role")
}

// testCooperativeCloseFixture creates terms and source with all immediate
// cooperative policy keys available to the test.
func testCooperativeCloseFixture(t *testing.T, kind Kind,
	reserve btcutil.Amount) (Terms, VTXOBinding, CooperativeCloseRequest,
	*btcec.PrivateKey, *btcec.PrivateKey, *btcec.PrivateKey) {

	t.Helper()
	terms := testTerms(t, kind)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hubKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	terms.VTXO.ClientArkKey = compressedKey(clientKey)
	terms.VTXO.HubArkKey = compressedKey(hubKey)
	terms.VTXO.ArkOperatorKey = compressedKey(operatorKey)
	source := resizeCooperativeCloseSource(
		t, testBinding(terms), terms.Capacity+reserve,
	)
	request := CooperativeCloseRequest{
		Initiator:            PartyClient,
		ClientDeliveryScript: testCooperativeDeliveryScript(1),
		HubDeliveryScript:    testCooperativeDeliveryScript(2),
		FeeRate:              chainfee.SatPerKWeight(1_000),
	}

	return terms, source, request, clientKey, hubKey, operatorKey
}

// resizeCooperativeCloseSource changes the prepared output amount and keeps its
// transaction ID and outpoint binding canonical.
func resizeCooperativeCloseSource(t *testing.T, source VTXOBinding,
	amount btcutil.Amount) VTXOBinding {

	t.Helper()
	tx := mustDecodeCooperativeClose(t, source.ArkTransaction)
	tx.TxOut[source.OutPoint.Index].Value = int64(amount)
	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))
	txid := tx.TxHash()
	source.OORSessionID = [32]byte(txid)
	source.OutPoint.Hash = txid
	source.Amount = amount
	source.ArkTransaction = raw.Bytes()

	return source
}

// testCooperativeDeliveryScript returns a standard P2WPKH output script.
func testCooperativeDeliveryScript(tag byte) []byte {
	return append([]byte{0x00, 0x14}, bytes.Repeat([]byte{tag}, 20)...)
}

// signCooperativeClose signs one endpoint role with its Ark policy key.
func signCooperativeClose(t *testing.T, template *CooperativeCloseTemplate,
	terms Terms, party Party, key *btcec.PrivateKey) input.Signature {

	t.Helper()
	desc, err := template.SignDescriptor(
		terms, party, keychain.KeyDescriptor{
			PubKey: key.PubKey(),
		},
	)
	require.NoError(t, err)
	sig, err := input.NewMockSigner(
		[]*btcec.PrivateKey{key}, nil,
	).SignOutputRaw(
		mustDecodeCooperativeClose(
			t, template.Proposal().Transaction,
		), desc,
	)
	require.NoError(t, err)

	return sig
}

// completeTestCooperativeClose signs all three immediate policy roles.
func completeTestCooperativeClose(t *testing.T,
	template *CooperativeCloseTemplate, terms Terms, source VTXOBinding,
	request CooperativeCloseRequest, clientKey, hubKey,
	operatorKey *btcec.PrivateKey) CooperativeClose {

	t.Helper()
	clientSig := signCooperativeClose(
		t, template, terms, PartyClient, clientKey,
	)
	hubSig := signCooperativeClose(
		t, template, terms, PartyHub, hubKey,
	)
	operatorDesc, err := template.OperatorSignDescriptor(
		terms, keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	)
	require.NoError(t, err)
	operatorSig, err := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	).SignOutputRaw(
		mustDecodeCooperativeClose(t, template.Proposal().Transaction),
		operatorDesc,
	)
	require.NoError(t, err)
	settlement, err := template.Complete(
		terms, source, request, clientSig, hubSig, operatorSig,
	)
	require.NoError(t, err)

	return settlement
}

// mustDecodeCooperativeClose decodes a transaction fixture.
func mustDecodeCooperativeClose(t *testing.T, raw []byte) *wire.MsgTx {
	t.Helper()
	tx, err := decodeCooperativeCloseTransaction(raw)
	require.NoError(t, err)

	return tx
}
