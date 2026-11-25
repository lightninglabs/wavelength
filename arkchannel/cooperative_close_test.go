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

// TestCooperativeCloseDeliveryPolicyAgreement requires both quoted destinations
// to bind the same session, including when one wallet is still on old terms.
func TestCooperativeCloseDeliveryPolicyAgreement(t *testing.T) {
	t.Parallel()
	terms, _, request, _, _, prior := testCooperativeCloseFixture(
		t, KindPromotion, 5_000,
	)
	next, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	client, err := CooperativeCloseDeliveryOwner(
		request.ClientDeliveryScript,
	)
	require.NoError(t, err)
	hub, err := CooperativeCloseDeliveryOwner(request.HubDeliveryScript)
	require.NoError(t, err)
	delay := terms.VTXO.MinExitDelay
	encode := arkscript.EncodeStandardVTXOTemplate
	request.ClientDeliveryScript, err = encode(
		client, next.PubKey(), delay,
	)
	require.NoError(t, err)
	require.ErrorContains(t, request.Validate(), "policies differ")
	for _, tc := range []struct {
		key   *btcec.PublicKey
		delay uint32
		match bool
	}{
		{
			prior.PubKey(),
			delay,
			false,
		},
		{
			next.PubKey(),
			delay + 1,
			false,
		},
		{
			next.PubKey(),
			delay,
			true,
		},
	} {
		request.HubDeliveryScript, err = encode(
			hub, tc.key, tc.delay,
		)
		require.NoError(t, err)
		if tc.match {
			require.NoError(t, request.Validate())
		} else {
			require.ErrorContains(
				t, request.Validate(),
				"policies differ",
			)
		}
	}
}

// TestCooperativeCloseBuildsThreePartyOOR proves the no-delay 3-of-3 policy
// authorizes an ordinary OOR transfer that preserves full-capacity role
// balances and never spends the unpublished channel point.
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

	hubArkSig := signCooperativeCloseArk(t, template, terms, hubKey)
	settlement, err := template.Complete(
		terms, source, request, hubSig, hubArkSig,
	)
	require.NoError(t, err)
	require.NoError(t, settlement.Validate(terms, source, request))
	checkpointSig, payoutSig, err := settlement.Signatures()
	require.NoError(t, err)
	require.Equal(t, hubSig.Serialize(), checkpointSig.Serialize())
	require.Equal(t, hubArkSig.Serialize(), payoutSig.Serialize())
	require.NotEqual(t, source.OutPoint.Hash, settlement.TxID)
	clientOutpoint, err := settlement.ReplacementOutPoint(
		terms, source, request, PartyClient,
	)
	require.NoError(t, err)
	hubOutpoint, err := settlement.ReplacementOutPoint(
		terms, source, request, PartyHub,
	)
	require.NoError(t, err)
	require.Equal(t, settlement.TxID, clientOutpoint.Hash)
	require.Equal(t, settlement.TxID, hubOutpoint.Hash)
	require.NotEqual(t, clientOutpoint, hubOutpoint)
	_, err = settlement.ReplacementOutPoint(
		terms, source, request, Party(99),
	)
	require.ErrorContains(t, err, "unknown cooperative close party")

	spec, err := template.OORSpec()
	require.NoError(t, err)
	require.Len(t, spec.Recipients, 2)
	path, err := arkscript.DecodeSpendPath(spec.SpendPath)
	require.NoError(t, err)
	require.Equal(t, wire.MaxTxInSequenceNum, path.RequiredSequence)
}

// TestCooperativeCloseAuthorizationBindsFinalPayout proves a checkpoint
// authorization cannot be reused to redirect the hub's agreed allocation.
func TestCooperativeCloseAuthorizationBindsFinalPayout(t *testing.T) {
	t.Parallel()

	terms, source, request, _, hubKey, _ := testCooperativeCloseFixture(
		t, KindPromotion, 5_000,
	)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 60_000, 40_000, 7,
	)
	require.NoError(t, err)
	checkpointSig := signCooperativeClose(
		t, template, terms, PartyHub, hubKey,
	)
	arkSig := signCooperativeCloseArk(t, template, terms, hubKey)
	settlement, err := template.Complete(
		terms, source, request, checkpointSig, arkSig,
	)
	require.NoError(t, err)
	redirected, err := NewCooperativeCloseTemplate(
		terms, source, request, terms.Capacity, 0, 7,
	)
	require.NoError(t, err)
	// The checkpoint alone still verifies: it does not commit recipients.
	require.NoError(
		t, redirected.VerifySignature(terms, PartyHub, checkpointSig),
	)
	require.Error(t, redirected.VerifyArkSignature(terms, PartyHub, arkSig))
	settlement.Proposal = redirected.Proposal()
	settlement.TxID = redirected.ArkTransaction().TxHash()
	require.ErrorContains(
		t, settlement.Validate(terms, source, request),
		"payout authorization",
	)

	// Reject old checkpoint-only artifacts rather than silently treating
	// missing payout authorization as consent to spend.
	settlement.Proposal = template.Proposal()
	settlement.TxID = template.ArkTransaction().TxHash()
	settlement.Transaction = checkpointSig.Serialize()
	require.Error(t, settlement.Validate(terms, source, request))
}

// TestCooperativeCloseReturnsReserveToClientFunder verifies the backing reserve
// never changes the full-capacity role allocation and follows the funder.
func TestCooperativeCloseReturnsReserveToClientFunder(t *testing.T) {
	t.Parallel()

	terms, source, request, _, _, _ := testCooperativeCloseFixture(
		t, KindPromotion, 4_000,
	)
	template, err := NewCooperativeCloseTemplate(
		terms, source, request, 25_000, 75_000, 11,
	)
	require.NoError(t, err)
	proposal := template.Proposal()
	require.EqualValues(t, 29_000, proposal.ClientOutput)
	require.Equal(t, btcutil.Amount(75_000), proposal.HubOutput)
}

// TestCooperativeCloseRequiresFullCapacityRoleBalances proves core neither
// infers nor assigns a balance shortfall supplied by the runtime.
func TestCooperativeCloseRequiresFullCapacityRoleBalances(t *testing.T) {
	t.Parallel()

	terms, source, request, _, _, _ := testCooperativeCloseFixture(
		t, KindPromotion, 2_000,
	)
	_, err := NewCooperativeCloseTemplate(
		terms, source, request, 39_000, 60_000, 4,
	)
	require.ErrorContains(t, err, "do not match capacity")
}

// TestCooperativeCloseRejectsUnsafeSettlement verifies peers cannot alter a
// replacement amount or omit the backing reserve.
func TestCooperativeCloseRejectsUnsafeSettlement(t *testing.T) {
	t.Parallel()

	terms, source, request, _, _, _ := testCooperativeCloseFixture(
		t, KindPromotion, 2_000,
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
		terms, source, request, terms.Capacity-1, 1, 4,
	)
	require.NoError(t, err)
	smallProposal := smallTemplate.Proposal()
	require.EqualValues(t, 1, smallProposal.HubOutput)
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
	settlement, err := template.Complete(
		terms, source, request, hubSig,
		signCooperativeCloseArk(t, template, terms, hubKey),
	)
	require.NoError(t, err)

	return settlement
}

// signCooperativeCloseArk authorizes the exact payout transaction.
func signCooperativeCloseArk(t *testing.T, template *CooperativeCloseTemplate,
	terms Terms, key *btcec.PrivateKey) input.Signature {

	t.Helper()
	desc, err := template.ArkSignDescriptor(
		terms, PartyHub, keychain.KeyDescriptor{
			PubKey: key.PubKey(),
		},
	)
	require.NoError(t, err)
	sig, err := input.NewMockSigner(
		[]*btcec.PrivateKey{key}, nil,
	).SignOutputRaw(template.ArkTransaction(), desc)
	require.NoError(t, err)

	return sig
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
