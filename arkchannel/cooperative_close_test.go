package arkchannel

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestCooperativeCloseBuildsThreePartyOOR proves the no-delay 3-of-3 policy
// authorizes an ordinary OOR transfer that preserves lnd balances and never
// spends the unpublished channel point.
func TestCooperativeCloseBuildsThreePartyOOR(t *testing.T) {
	t.Parallel()

	terms, source, request, clientKey, hubKey, _ :=
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
		t, source.Amount, proposal.ClientOutput+proposal.HubOutput,
	)
	require.Equal(t, clientBalance+5_000, proposal.ClientOutput)
	require.Equal(t, hubBalance, proposal.HubOutput)

	checkpoint, err := psbtutil.Parse(proposal.Transaction)
	require.NoError(t, err)
	require.Equal(
		t, source.OutPoint,
		checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint,
	)
	require.NotEqual(
		t, testBacking(t, terms, source).ChannelPoint,
		checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint,
	)
	require.Equal(
		t, wire.MaxTxInSequenceNum,
		checkpoint.UnsignedTx.TxIn[0].Sequence,
	)

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
	require.ErrorContains(
		t, template.VerifySignature(terms, PartyClient, hubSig),
		"invalid client cooperative close signature",
	)

	settlement, err := template.Complete(terms, source, request, hubSig)
	require.NoError(t, err)
	require.NoError(t, settlement.Validate(terms, source, request))
	require.Equal(t, hubSig.Serialize(), settlement.Transaction)
	require.NotEqual(t, source.OutPoint.Hash, settlement.TxID)

	spec, err := template.OORSpec()
	require.NoError(t, err)
	require.Len(t, spec.Recipients, 2)
	path, err := arkscript.DecodeSpendPath(spec.SpendPath)
	require.NoError(t, err)
	require.Equal(t, wire.MaxTxInSequenceNum, path.RequiredSequence)
}

// TestCooperativeCloseReturnsReserveToHubFunder verifies the backing reserve
// never changes the lnd balance allocation and follows the original funder.
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
	require.Equal(t, btcutil.Amount(79_000), proposal.HubOutput)
}

// TestCooperativeCloseRejectsUnsafeSettlement verifies peers cannot alter a
// replacement amount or omit the backing reserve.
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

	smallTemplate, err := NewCooperativeCloseTemplate(
		terms, source, request, 1, terms.Capacity-1, 4,
	)
	require.NoError(t, err)
	smallProposal := smallTemplate.Proposal()
	require.EqualValues(t, 1, smallProposal.ClientOutput)
	require.Equal(
		t, source.Amount,
		smallProposal.ClientOutput+smallProposal.HubOutput,
	)

	tooSmall := resizeCooperativeCloseSource(t, source, terms.Capacity)
	_, err = NewCooperativeCloseTemplate(
		terms, tooSmall, request, 40_000, 60_000, 4,
	)
	require.ErrorContains(t, err, "no backing reserve")
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
		ClientDeliveryScript: testCooperativeOwnerKey(1),
		HubDeliveryScript:    testCooperativeOwnerKey(2),
	}

	return terms, source, request, clientKey, hubKey, operatorKey
}

// resizeCooperativeCloseSource changes the prepared output amount and keeps its
// transaction ID and outpoint binding canonical.
func resizeCooperativeCloseSource(t *testing.T, source VTXOBinding,
	amount btcutil.Amount) VTXOBinding {

	t.Helper()
	tx := mustDecodeTransaction(t, source.ArkTransaction)
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

// testCooperativeOwnerKey returns a deterministic compressed account key.
func testCooperativeOwnerKey(tag byte) []byte {
	_, key := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{tag}, 32))

	return key.SerializeCompressed()
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
		mustDecodeCheckpoint(t, template.Proposal().Transaction), desc,
	)
	require.NoError(t, err)

	return sig
}

// completeTestCooperativeClose records the hub's 3-of-3 OOR authorization.
// The unused keys remain in the helper signature because state tests share the
// full policy fixture.
func completeTestCooperativeClose(t *testing.T,
	template *CooperativeCloseTemplate, terms Terms, source VTXOBinding,
	request CooperativeCloseRequest, _, hubKey,
	_ *btcec.PrivateKey) CooperativeClose {

	t.Helper()
	hubSig := signCooperativeClose(
		t, template, terms, PartyHub, hubKey,
	)
	settlement, err := template.Complete(terms, source, request, hubSig)
	require.NoError(t, err)

	return settlement
}

// mustDecodeTransaction decodes a raw transaction fixture.
func mustDecodeTransaction(t *testing.T, raw []byte) *wire.MsgTx {
	t.Helper()
	tx := wire.NewMsgTx(2)
	require.NoError(t, tx.Deserialize(bytes.NewReader(raw)))

	return tx
}

// mustDecodeCheckpoint returns an unsigned checkpoint transaction.
func mustDecodeCheckpoint(t *testing.T, raw []byte) *wire.MsgTx {
	t.Helper()
	packet, err := psbtutil.Parse(raw)
	require.NoError(t, err)

	return packet.UnsignedTx
}
