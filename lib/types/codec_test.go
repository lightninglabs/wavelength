package types

import (
	"bytes"
	"io"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestJoinRoundAuthMessageDeterministic asserts canonical encoding is
// stable for equivalent request contents.
func TestJoinRoundAuthMessageDeterministic(t *testing.T) {
	t.Parallel()

	req := testJoinRoundAuthRequest(t)

	first, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)

	second, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)

	require.Equal(t, first, second)
}

// TestJoinRoundAuthMessageBindsForfeitSpendPaths asserts custom forfeit spend
// paths are part of the canonical signed message.
func TestJoinRoundAuthMessageBindsForfeitSpendPaths(t *testing.T) {
	t.Parallel()

	req := testJoinRoundAuthRequest(t)
	baseline, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)

	withoutAuthSpend := copyJoinRoundAuthRequestWithForfeit(req, func(
		req *ForfeitRequest) {

		req.AuthSpend = nil
	})
	authChanged, err := JoinRoundAuthMessage(withoutAuthSpend)
	require.NoError(t, err)
	require.NotEqual(t, baseline, authChanged)

	withoutForfeitSpend := copyJoinRoundAuthRequestWithForfeit(req, func(
		req *ForfeitRequest) {

		req.ForfeitSpend = nil
	})
	forfeitChanged, err := JoinRoundAuthMessage(withoutForfeitSpend)
	require.NoError(t, err)
	require.NotEqual(t, baseline, forfeitChanged)

	withDifferentForfeitSpend := copyJoinRoundAuthRequestWithForfeit(req,
		func(req *ForfeitRequest) {
			path := *req.ForfeitSpend
			path.RequiredSequence--
			req.ForfeitSpend = &path
		},
	)
	differentForfeit, err := JoinRoundAuthMessage(withDifferentForfeitSpend)
	require.NoError(t, err)
	require.NotEqual(t, baseline, differentForfeit)
}

// TestJoinRoundAuthMessageBindsClaimInputs verifies the outer BIP-322 payload
// commits to the complete independently signed claim envelope.
func TestJoinRoundAuthMessageBindsClaimInputs(t *testing.T) {
	t.Parallel()

	req := testJoinRoundAuthRequest(t)
	participant := testJoinAuthPubKey(t)
	replacement := testJoinAuthPubKey(t)
	claim := &VTXOClaimInput{
		SourceOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x33,
			},
			Index: 9,
		},
		ParticipantPubKey: participant,
		ReplacementSigningKey: keychain.KeyDescriptor{
			PubKey: replacement,
		},
		ValidFrom:  100,
		ValidUntil: 200,
		Signature:  bytes.Repeat([]byte{0x44}, VTXOClaimSignatureSize),
	}
	claim.Nonce[0] = 1
	req.ClaimInputs = []*VTXOClaimInput{claim}

	baseline, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)
	decoded, err := DecodeJoinRoundAuthMessage(baseline)
	require.NoError(t, err)
	requireJoinRoundAuthRequestEqual(t, req, decoded)

	changedReq := *req
	changedClaim := *claim
	changedClaim.Signature = bytes.Clone(claim.Signature)
	changedClaim.Signature[0] ^= 1
	changedReq.ClaimInputs = []*VTXOClaimInput{&changedClaim}
	changed, err := JoinRoundAuthMessage(&changedReq)
	require.NoError(t, err)
	require.NotEqual(t, baseline, changed)
}

// TestDecodeJoinRoundAuthMessageRoundTrip asserts decode reconstructs join
// requests from canonical message bytes.
func TestDecodeJoinRoundAuthMessageRoundTrip(t *testing.T) {
	t.Parallel()

	req := testJoinRoundAuthRequest(t)

	raw, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)

	decodedReq, err := DecodeJoinRoundAuthMessage(raw)
	require.NoError(t, err)

	requireJoinRoundAuthRequestEqual(t, req, decodedReq)
}

// TestJoinRoundAuthMessageVersionedTLV asserts the canonical payload is a
// versioned TLV envelope with the expected top-level records.
func TestJoinRoundAuthMessageVersionedTLV(t *testing.T) {
	t.Parallel()

	req := testJoinRoundAuthRequest(t)

	raw, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)

	var (
		version     uint64
		domain      []byte
		identifier  []byte
		boardingRaw []byte
		vtxoRaw     []byte
		forfeitRaw  []byte
		leaveRaw    []byte
		claimRaw    []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageDomainRecordType, &domain,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageIDRecordType, &identifier,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageBoardRecordType, &boardingRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVTXORecordType, &vtxoRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageForfeitRecordType, &forfeitRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageLeaveRecordType, &leaveRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageClaimRecordType, &claimRaw,
		),
	)
	require.NoError(t, err)

	reader := bytes.NewReader(raw)
	parsedTypes, err := stream.DecodeWithParsedTypes(reader)
	require.NoError(t, err)
	require.Zero(t, reader.Len())

	require.Equal(t, joinRoundAuthMessageVersion, version)
	require.Equal(t, joinRoundAuthDomainTag, string(domain))
	require.Equal(
		t, req.Identifier.SerializeCompressed(), identifier,
	)
	require.Contains(
		t, parsedTypes, joinRoundAuthMessageIDRecordType,
	)

	require.Equal(
		t, uint64(1), decodeJoinAuthBlobListCount(t, boardingRaw),
	)
	require.Equal(
		t, uint64(1),
		decodeJoinAuthBlobListCount(t, vtxoRaw),
	)
	require.Equal(
		t, uint64(1), decodeJoinAuthBlobListCount(t, forfeitRaw),
	)
	require.Equal(
		t, uint64(1),
		decodeJoinAuthBlobListCount(t, leaveRaw),
	)
	require.Equal(
		t, uint64(0),
		decodeJoinAuthBlobListCount(t, claimRaw),
	)
}

// TestJoinRoundAuthMessageRequiresIdentifier asserts identifier is mandatory
// in canonical join-auth messages.
func TestJoinRoundAuthMessageRequiresIdentifier(t *testing.T) {
	t.Parallel()

	req := testJoinRoundAuthRequest(t)
	req.Identifier = nil

	_, err := JoinRoundAuthMessage(req)
	require.ErrorContains(
		t, err, "join round request identifier must be provided",
	)
}

// TestDecodeJoinRoundAuthMessageRequiresIdentifierRecord asserts decode
// rejects envelopes that omit the identifier record.
func TestDecodeJoinRoundAuthMessageRequiresIdentifierRecord(t *testing.T) {
	t.Parallel()

	version := joinRoundAuthMessageVersion
	domain := []byte(joinRoundAuthDomainTag)

	boardingRaw := testJoinAuthBlobListCountOnly(t, 0)
	vtxoRaw := testJoinAuthBlobListCountOnly(t, 0)
	forfeitRaw := testJoinAuthBlobListCountOnly(t, 0)
	leaveRaw := testJoinAuthBlobListCountOnly(t, 0)
	claimRaw := testJoinAuthBlobListCountOnly(t, 0)

	raw, err := encodeJoinAuthTLV([]tlv.Record{
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageDomainRecordType, &domain,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageBoardRecordType, &boardingRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVTXORecordType, &vtxoRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageForfeitRecordType, &forfeitRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageLeaveRecordType, &leaveRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageClaimRecordType, &claimRaw,
		),
	})
	require.NoError(t, err)

	_, err = DecodeJoinRoundAuthMessage(raw)
	require.ErrorContains(
		t, err, "join auth identifier record must be present",
	)
}

// TestDecodeJoinRoundAuthMessageRequestCountLimit asserts decode rejects
// request-list counts above the configured maximum.
func TestDecodeJoinRoundAuthMessageRequestCountLimit(t *testing.T) {
	t.Parallel()

	version := joinRoundAuthMessageVersion
	domain := []byte(joinRoundAuthDomainTag)
	identifier := testJoinAuthPubKey(t).SerializeCompressed()

	boardingRaw := testJoinAuthBlobListCountOnly(
		t, joinRoundAuthMaxRequestCount+1,
	)
	vtxoRaw := testJoinAuthBlobListCountOnly(t, 0)
	forfeitRaw := testJoinAuthBlobListCountOnly(t, 0)
	leaveRaw := testJoinAuthBlobListCountOnly(t, 0)
	claimRaw := testJoinAuthBlobListCountOnly(t, 0)

	raw, err := encodeJoinAuthTLV([]tlv.Record{
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageDomainRecordType, &domain,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageIDRecordType, &identifier,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageBoardRecordType, &boardingRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVTXORecordType, &vtxoRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageForfeitRecordType, &forfeitRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageLeaveRecordType, &leaveRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageClaimRecordType, &claimRaw,
		),
	})
	require.NoError(t, err)

	_, err = DecodeJoinRoundAuthMessage(raw)
	require.ErrorContains(t, err, "blob list count")
	require.ErrorContains(t, err, "exceeds max")
}

// TestDecodeJoinRoundAuthMessageScriptSizeLimit asserts decode rejects scripts
// above the configured maximum.
func TestDecodeJoinRoundAuthMessageScriptSizeLimit(t *testing.T) {
	t.Parallel()

	req := testJoinRoundAuthRequest(t)
	req.VTXOReqs[0].PolicyTemplate = bytes.Repeat(
		[]byte{0x51}, joinRoundAuthMaxScriptSize+1,
	)

	raw, err := JoinRoundAuthMessage(req)
	require.NoError(t, err)

	_, err = DecodeJoinRoundAuthMessage(raw)
	require.ErrorContains(t, err, "vtxo policy size")
	require.ErrorContains(t, err, "exceeds max")
}

// decodeJoinAuthBlobListCount parses a blob list payload and returns its
// item count while checking it is not malformed.
func decodeJoinAuthBlobListCount(t *testing.T, raw []byte) uint64 {
	t.Helper()

	var scratch [8]byte
	reader := bytes.NewReader(raw)

	count, err := tlv.ReadVarInt(reader, &scratch)
	require.NoError(t, err)

	for i := uint64(0); i < count; i++ {
		size, err := tlv.ReadVarInt(reader, &scratch)
		require.NoError(t, err)

		blob := make([]byte, size)
		_, err = io.ReadFull(reader, blob)
		require.NoError(t, err)
	}

	require.Zero(t, reader.Len())

	return count
}

// requireJoinRoundAuthRequestEqual asserts two join requests match.
func requireJoinRoundAuthRequestEqual(t *testing.T, expected *JoinRoundRequest,
	actual *JoinRoundRequest) {

	t.Helper()

	require.NotNil(t, expected)
	require.NotNil(t, actual)

	if expected.Identifier == nil {
		require.Nil(t, actual.Identifier)
	} else {
		require.NotNil(t, actual.Identifier)
		require.Equal(
			t, expected.Identifier.SerializeCompressed(),
			actual.Identifier.SerializeCompressed(),
		)
	}

	require.Len(
		t, actual.BoardingReqs, len(expected.BoardingReqs),
	)
	for i := 0; i < len(expected.BoardingReqs); i++ {
		expectedReq := expected.BoardingReqs[i]
		actualReq := actual.BoardingReqs[i]

		require.NotNil(t, expectedReq)
		require.NotNil(t, actualReq)
		require.NotNil(t, expectedReq.Outpoint)
		require.NotNil(t, actualReq.Outpoint)

		require.Equal(t, *expectedReq.Outpoint, *actualReq.Outpoint)
		require.Equal(
			t, expectedReq.PolicyTemplate, actualReq.PolicyTemplate,
		)
	}

	require.Len(t, actual.VTXOReqs, len(expected.VTXOReqs))
	for i := 0; i < len(expected.VTXOReqs); i++ {
		expectedReq := expected.VTXOReqs[i]
		actualReq := actual.VTXOReqs[i]

		require.NotNil(t, expectedReq)
		require.NotNil(t, actualReq)

		require.Equal(t, expectedReq.Amount, actualReq.Amount)
		require.Equal(t, expectedReq.FixedAmount, actualReq.FixedAmount)
		require.Equal(
			t, expectedReq.PolicyTemplate, actualReq.PolicyTemplate,
		)
		require.Equal(
			t, expectedReq.SigningKey.PubKey.SerializeCompressed(),
			actualReq.SigningKey.PubKey.SerializeCompressed(),
		)
	}

	require.Len(
		t, actual.ForfeitReqs, len(expected.ForfeitReqs),
	)
	for i := 0; i < len(expected.ForfeitReqs); i++ {
		expectedReq := expected.ForfeitReqs[i]
		actualReq := actual.ForfeitReqs[i]

		require.NotNil(t, expectedReq)
		require.NotNil(t, actualReq)
		require.NotNil(t, expectedReq.VTXOOutpoint)
		require.NotNil(t, actualReq.VTXOOutpoint)
		require.Equal(
			t, *expectedReq.VTXOOutpoint, *actualReq.VTXOOutpoint,
		)
		requireSpendPathEqual(
			t, expectedReq.AuthSpend, actualReq.AuthSpend,
		)
		requireSpendPathEqual(
			t, expectedReq.ForfeitSpend, actualReq.ForfeitSpend,
		)
	}

	require.Len(t, actual.LeaveReqs, len(expected.LeaveReqs))
	for i := 0; i < len(expected.LeaveReqs); i++ {
		expectedReq := expected.LeaveReqs[i]
		actualReq := actual.LeaveReqs[i]

		require.NotNil(t, expectedReq)
		require.NotNil(t, actualReq)
		require.NotNil(t, expectedReq.Output)
		require.NotNil(t, actualReq.Output)

		require.Equal(
			t, expectedReq.Output.Value, actualReq.Output.Value,
		)
		require.Equal(
			t, expectedReq.Output.PkScript,
			actualReq.Output.PkScript,
		)
	}

	require.Len(t, actual.ClaimInputs, len(expected.ClaimInputs))
	for i := range expected.ClaimInputs {
		expectedClaim := expected.ClaimInputs[i]
		actualClaim := actual.ClaimInputs[i]
		require.NotNil(t, expectedClaim)
		require.NotNil(t, actualClaim)
		require.Equal(
			t, expectedClaim.SourceOutpoint,
			actualClaim.SourceOutpoint,
		)
		require.Equal(
			t,
			expectedClaim.ParticipantPubKey.SerializeCompressed(),
			actualClaim.ParticipantPubKey.SerializeCompressed(),
		)
		require.Equal(
			t, expectedClaim.ReplacementSigningKey.PubKey.
				SerializeCompressed(),
			actualClaim.ReplacementSigningKey.PubKey.
				SerializeCompressed(),
		)
		require.Equal(t, expectedClaim.Nonce, actualClaim.Nonce)
		require.Equal(t, expectedClaim.ValidFrom, actualClaim.ValidFrom)
		require.Equal(
			t, expectedClaim.ValidUntil, actualClaim.ValidUntil,
		)
		require.Equal(t, expectedClaim.Signature, actualClaim.Signature)
	}
}

// requireSpendPathEqual asserts two spend paths have the same canonical binary
// encoding.
func requireSpendPathEqual(t *testing.T,
	expected, actual *arkscript.SpendPath) {

	t.Helper()

	if expected == nil {
		require.Nil(t, actual)

		return
	}

	require.NotNil(t, actual)

	expectedBytes, err := expected.Encode()
	require.NoError(t, err)

	actualBytes, err := actual.Encode()
	require.NoError(t, err)
	require.Equal(t, expectedBytes, actualBytes)
}

// copyJoinRoundAuthRequestWithForfeit returns a shallow request copy with a
// copied first forfeit request mutated by update.
func copyJoinRoundAuthRequestWithForfeit(req *JoinRoundRequest,
	update func(*ForfeitRequest)) *JoinRoundRequest {

	copied := *req
	forfeit := *req.ForfeitReqs[0]
	update(&forfeit)
	copied.ForfeitReqs = []*ForfeitRequest{
		&forfeit,
	}

	return &copied
}

// testJoinAuthBlobListCountOnly serializes only the list count varint for
// decode limit tests.
func testJoinAuthBlobListCountOnly(t *testing.T, count uint64) []byte {
	t.Helper()

	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	err := tlv.WriteVarInt(&buf, count, &scratch)
	require.NoError(t, err)

	return buf.Bytes()
}

// testJoinRoundAuthRequest builds a minimal request fixture for join-auth
// message encoding tests.
func testJoinRoundAuthRequest(t *testing.T) *JoinRoundRequest {
	t.Helper()

	clientKey := testJoinAuthPubKey(t)
	operatorKey := testJoinAuthPubKey(t)
	signingKey := testJoinAuthPubKey(t)
	identifier := testJoinAuthPubKey(t)

	boardingOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x11,
		},
		Index: 3,
	}
	forfeitOutpoint := wire.OutPoint{
		Hash: chainhash.Hash{
			0x22,
		},
		Index: 8,
	}

	req := &JoinRoundRequest{
		BoardingReqs: []*BoardingRequest{
			{
				Outpoint:    &boardingOutpoint,
				ClientKey:   clientKey,
				OperatorKey: operatorKey,
				ExitDelay:   144,
			},
		},
		VTXOReqs: []*VTXORequest{
			{
				Amount:      25_000,
				Expiry:      288,
				ClientKey:   clientKey,
				OperatorKey: operatorKey,
				SigningKey: keychain.KeyDescriptor{
					PubKey: signingKey,
				},
				FixedAmount: true,
			},
		},
		ForfeitReqs: []*ForfeitRequest{
			{
				VTXOOutpoint: &forfeitOutpoint,
			},
		},
		LeaveReqs: []*LeaveRequest{
			{
				Output: &wire.TxOut{
					Value: 12_345,
					PkScript: []byte{
						0x00,
						0x14,
						0xaa,
					},
				},
			},
		},
		Identifier: identifier,
	}

	var err error
	req.BoardingReqs[0].PolicyTemplate, err =
		arkscript.EncodeStandardVTXOTemplate(
			clientKey, operatorKey, req.BoardingReqs[0].ExitDelay,
		)
	require.NoError(t, err)

	req.VTXOReqs[0].PolicyTemplate, err =
		arkscript.EncodeStandardVTXOTemplate(
			clientKey, operatorKey, req.VTXOReqs[0].Expiry,
		)
	require.NoError(t, err)

	req.VTXOReqs[0].PkScript, err = req.VTXOReqs[0].EffectivePkScript()
	require.NoError(t, err)

	policy, err := arkscript.NewVTXOPolicy(clientKey, operatorKey, 144)
	require.NoError(t, err)

	authSpend, err := policy.CollabSpendInfo()
	require.NoError(t, err)

	forfeitSpend, err := policy.ExitSpendInfo()
	require.NoError(t, err)

	req.ForfeitReqs[0].AuthSpend = &arkscript.SpendPath{
		SpendInfo:        authSpend,
		RequiredSequence: wire.MaxTxInSequenceNum,
	}
	req.ForfeitReqs[0].ForfeitSpend = &arkscript.SpendPath{
		SpendInfo:        forfeitSpend,
		RequiredSequence: policy.ExitDelay,
	}

	return req
}

// testJoinAuthPubKey derives a random public key for message-encoding
// tests.
func testJoinAuthPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	privateKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return privateKey.PubKey()
}
