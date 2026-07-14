package indexer

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/internal/indexerlimits"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestEncodeProofTLVRoundTrip encodes a proof and decodes it via a TLV
// stream, verifying all nine fields survive the round trip.
func TestEncodeProofTLVRoundTrip(t *testing.T) {
	t.Parallel()

	const (
		msgType   = "receive_script_registration"
		serverID  = "server-abc"
		principal = "client:xyz"
		purpose   = "register_receive_script"
		issuedAt  = uint64(1700000000)
		expiresAt = uint64(1700000600)
	)
	pkScript := []byte{0x51, 0x20, 0x01, 0x02, 0x03}
	nonce := []byte{0xaa, 0xbb, 0xcc, 0xdd}

	encoded, err := encodeProofTLV(
		msgType, serverID, principal, purpose, pkScript, nonce,
		issuedAt, expiresAt,
	)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	// Decode the fields back out using a fresh TLV stream.
	var (
		gotType      []byte
		gotVersion   uint32
		gotServerID  []byte
		gotPrincipal []byte
		gotPkScript  []byte
		gotIssuedAt  uint64
		gotExpiresAt uint64
		gotNonce     []byte
		gotPurpose   []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &gotType, tlv.SizeVarBytes(&gotType),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &gotVersion,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &gotServerID,
			tlv.SizeVarBytes(&gotServerID), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &gotPrincipal,
			tlv.SizeVarBytes(&gotPrincipal), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &gotPkScript,
			tlv.SizeVarBytes(&gotPkScript), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &gotIssuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &gotExpiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &gotNonce,
			tlv.SizeVarBytes(&gotNonce), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &gotPurpose,
			tlv.SizeVarBytes(&gotPurpose), tlv.EVarBytes,
			tlv.DVarBytes,
		),
	)
	require.NoError(t, err)

	err = stream.Decode(bytes.NewReader(encoded))
	require.NoError(t, err)

	require.Equal(t, msgType, string(gotType))
	require.Equal(t, uint32(registrationMessageVersion), gotVersion)
	require.Equal(t, serverID, string(gotServerID))
	require.Equal(t, principal, string(gotPrincipal))
	require.Equal(t, pkScript, gotPkScript)
	require.Equal(t, issuedAt, gotIssuedAt)
	require.Equal(t, expiresAt, gotExpiresAt)
	require.Equal(t, nonce, gotNonce)
	require.Equal(t, purpose, string(gotPurpose))
}

// TestEncodeProofTLVDeterministic verifies that encoding the same
// inputs twice yields identical byte output.
func TestEncodeProofTLVDeterministic(t *testing.T) {
	t.Parallel()

	const (
		msgType  = "script_scope"
		serverID = "srv"
		prin     = "prin"
		purpose  = "list_vtxos_by_scripts"
		issued   = uint64(1700000000)
		expires  = uint64(1700000600)
	)

	pkScript := []byte{0x51, 0x20, 0x01}
	nonce := []byte{0xde, 0xad, 0xbe, 0xef}

	encode := func() []byte {
		b, err := encodeProofTLV(
			msgType, serverID, prin, purpose, pkScript, nonce,
			issued, expires,
		)
		require.NoError(t, err)

		return b
	}

	require.Equal(t, encode(), encode())
}

// TestEncodeProofTLVDistinctPurposes verifies that different purpose
// strings produce different encoded output.
func TestEncodeProofTLVDistinctPurposes(t *testing.T) {
	t.Parallel()

	common := func(purpose string) []byte {
		b, err := encodeProofTLV(
			"script_scope", "srv", "prin", purpose,
			[]byte{0x51, 0x20, 0x01},
			[]byte{0x00, 0x01, 0x02, 0x03}, 1700000000, 1700000600,
		)
		require.NoError(t, err)

		return b
	}

	a := common("list_vtxos_by_scripts")
	b := common("get_subtree_by_scripts")

	require.NotEqual(
		t, a, b, "different purposes must produce different TLV bytes",
	)
}

// TestSchnorrSigOverMessageSignVerify signs a message and verifies
// the signature using the corresponding public key.
func TestSchnorrSigOverMessageSignVerify(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	msg := []byte("test proof message content")

	signer := &PrivKeySchnorrSigner{Key: priv}
	sig64, err := schnorrSigOverMessage(
		t.Context(), msg, nil, signer,
	)
	require.NoError(t, err)
	require.Len(t, sig64, 64)

	// Verify the signature using the same tagged hash logic.
	msgHash := chainhash.TaggedHash(proofTag(), msg)
	sig, err := schnorr.ParseSignature(sig64)
	require.NoError(t, err)

	pub := priv.PubKey()
	require.True(
		t, sig.Verify(msgHash[:], pub),
		"signature must verify with correct key",
	)
}

// TestSchnorrSigOverMessageDeterministic verifies that signing the
// same message with the same key produces the same signature.
func TestSchnorrSigOverMessageDeterministic(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	msg := []byte("deterministic signing test")

	signer := &PrivKeySchnorrSigner{Key: priv}
	sig1, err := schnorrSigOverMessage(
		t.Context(), msg, nil, signer,
	)
	require.NoError(t, err)

	sig2, err := schnorrSigOverMessage(
		t.Context(), msg, nil, signer,
	)
	require.NoError(t, err)

	require.Equal(t, sig1, sig2)
}

// TestSchnorrSigOverMessageWrongKeyFails verifies that a signature
// does not verify under a different public key.
func TestSchnorrSigOverMessageWrongKeyFails(t *testing.T) {
	t.Parallel()

	priv1, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	priv2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	msg := []byte("wrong key test")

	signer1 := &PrivKeySchnorrSigner{Key: priv1}
	sig64, err := schnorrSigOverMessage(
		t.Context(), msg, nil, signer1,
	)
	require.NoError(t, err)

	// Parse and verify against the wrong key.
	msgHash := chainhash.TaggedHash(proofTag(), msg)
	sig, err := schnorr.ParseSignature(sig64)
	require.NoError(t, err)

	wrongPub := priv2.PubKey()
	require.False(
		t, sig.Verify(msgHash[:], wrongPub),
		"signature must not verify with wrong key",
	)
}

// TestSchnorrSigOverMessageUsesMessageSigner verifies that the helper prefers
// the tagged-message signing path when it is available.
func TestSchnorrSigOverMessageUsesMessageSigner(t *testing.T) {
	t.Parallel()

	signer := &testMessageSchnorrSigner{
		result: []byte("message-signed"),
	}
	msg := []byte("message signer path")
	pkScript := []byte{0x51, 0x20, 0x01}

	sig, err := schnorrSigOverMessage(
		t.Context(), msg, pkScript, signer,
	)
	require.NoError(t, err)
	require.Equal(t, []byte("message-signed"), sig)
	require.Equal(t, msg, signer.message)
	require.Equal(t, pkScript, signer.pkScript)
	require.Equal(t, proofTag(), signer.tag)
	require.False(t, signer.rawCalled)
}

// TestRegisterReceiveScriptTaprootUsesShortLivedProof verifies that the
// registration proof TTL remains short even when the registration retention
// window is much longer.
func TestRegisterReceiveScriptTaprootUsesShortLivedProof(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := append(
		[]byte{0x51, 0x20},
		privKey.PubKey().SerializeCompressed()[1:]...,
	)

	rpcClient := &recordingRPCClient{}
	client := New(
		rpcClient, &PrivKeySchnorrSigner{
			Key: privKey,
		}, "test-server", "client:test",
		fn.None[btclog.Logger](),
	)

	registrationExpiry := time.Now().Add(30 * 24 * time.Hour)
	_, err = client.RegisterReceiveScriptTaproot(
		t.Context(), pkScript, registrationExpiry, "oor receive",
	)
	require.NoError(t, err)

	req := rpcClient.lastRegisterReceiveScriptRequest(t)
	require.Equal(
		t,
		uint64(
			registrationExpiry.Unix(),
		),
		req.ExpiresAtUnixS,
	)

	decoded := decodeProofTLVBytes(
		t, req.GetTaprootSchnorr().GetMessage(),
	)
	require.Equal(t, purposeRegisterReceiveScript, decoded.purpose)
	require.Equal(t, uint64(registrationExpiry.Unix()), req.ExpiresAtUnixS)
	require.NotEqual(t, req.ExpiresAtUnixS, decoded.expiresAt)
	require.Equal(
		t, uint64(offlineReceiveProofTTL/time.Second),
		decoded.expiresAt-decoded.issuedAt,
	)
}

// TestBuildListVTXOsByScriptsTaprootRequestRejectsOversizedCursor verifies
// untrusted opaque pagination cursors are capped before being copied into the
// request body.
func TestBuildListVTXOsByScriptsTaprootRequestRejectsOversizedCursor(
	t *testing.T) {

	t.Parallel()

	client := New(
		&recordingRPCClient{}, nil, "test-server", "client:test",
		fn.None[btclog.Logger](),
	)

	_, err := client.BuildListVTXOsByScriptsTaprootRequest(
		t.Context(), nil,
		make([]byte, indexerlimits.MaxVTXOsByScriptsCursorBytes+1), 1,
		nil,
	)
	require.ErrorContains(t, err, "after cursor: vtxo cursor length")
}

type testMessageSchnorrSigner struct {
	result    []byte
	message   []byte
	pkScript  []byte
	tag       []byte
	rawCalled bool
}

// SignSchnorr records an unexpected raw-digest call.
func (s *testMessageSchnorrSigner) SignSchnorr(_ []byte, _ [32]byte) ([]byte,
	error) {

	s.rawCalled = true

	return nil, nil
}

// SignSchnorrMessage records the canonical inputs passed by the helper.
func (s *testMessageSchnorrSigner) SignSchnorrMessage(_ context.Context,
	pkScript []byte, message []byte, tag []byte) ([]byte, error) {

	s.message = append([]byte(nil), message...)
	s.pkScript = append([]byte(nil), pkScript...)
	s.tag = append([]byte(nil), tag...)

	return append([]byte(nil), s.result...), nil
}

// TestValidateTaprootPkScript uses a table-driven approach to verify
// that only valid P2TR scripts pass validation.
func TestValidateTaprootPkScript(t *testing.T) {
	t.Parallel()

	// Build a valid 34-byte P2TR script: OP_1 OP_DATA_32 <32 bytes>.
	validP2TR := make([]byte, 34)
	validP2TR[0] = 0x51 // OP_1 (witness version 1)
	validP2TR[1] = 0x20 // OP_DATA_32
	for i := 2; i < 34; i++ {
		validP2TR[i] = byte(i)
	}

	tests := []struct {
		name    string
		script  []byte
		wantErr bool
	}{
		{
			name:    "valid P2TR",
			script:  validP2TR,
			wantErr: false,
		},
		{
			name:    "empty script",
			script:  nil,
			wantErr: true,
		},
		{
			name: "P2PKH",
			script: []byte{
				0x76, 0xa9, 0x14, // OP_DUP OP_HASH160 20
				0x00, 0x01, 0x02, 0x03, 0x04,
				0x05, 0x06, 0x07, 0x08, 0x09,
				0x0a, 0x0b, 0x0c, 0x0d, 0x0e,
				0x0f, 0x10, 0x11, 0x12, 0x13,
				0x88, 0xac, // OP_EQUALVERIFY OP_CHECKSIG
			},
			wantErr: true,
		},
		{
			name: "P2WSH (witness v0, 32 bytes)",
			script: append(
				[]byte{0x00, 0x20}, make([]byte, 32)...,
			),
			wantErr: true,
		},
		{
			name: "too short",
			script: []byte{
				0x51,
				0x20,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateTaprootPkScript(tc.script)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestRandomNonceRejectsZeroAndNegative verifies that randomNonce
// returns an error for non-positive lengths.
func TestRandomNonceRejectsZeroAndNegative(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, -1, -100} {
		_, err := randomNonce(n)
		require.Error(t, err, "n=%d should fail", n)
	}
}

// TestRandomNoncePositive verifies that randomNonce returns a
// non-nil, non-zero slice of the correct length.
func TestRandomNoncePositive(t *testing.T) {
	t.Parallel()

	nonce, err := randomNonce(32)
	require.NoError(t, err)
	require.Len(t, nonce, 32)

	// Sanity: all-zero would indicate a broken RNG.
	allZero := make([]byte, 32)
	require.NotEqual(t, allZero, nonce)
}

// TestProofTagImmutable verifies that mutating one proofTag() return
// does not affect subsequent calls.
func TestProofTagImmutable(t *testing.T) {
	t.Parallel()

	tag1 := proofTag()
	original := make([]byte, len(tag1))
	copy(original, tag1)

	// Mutate the first return value.
	tag1[0] = 0xff

	tag2 := proofTag()
	require.Equal(
		t, original, tag2,
		"mutating one proofTag() return must not affect the next",
	)
}

type recordingRPCClient struct {
	mu      sync.Mutex
	lastReq proto.Message
}

// SendRPC records the last request and returns a static correlation pair.
func (r *recordingRPCClient) SendRPC(_ context.Context,
	_ mailboxrpc.ServiceMethod, req proto.Message,
	_ mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	r.mu.Lock()
	r.lastReq = proto.Clone(req)
	r.mu.Unlock()

	return mailboxrpc.SendResult{
		CorrelationID:  "corr-1",
		IdempotencyKey: "idemp-1",
	}, nil
}

// AwaitRPC completes successfully with the zero-value response.
func (r *recordingRPCClient) AwaitRPC(_ context.Context, _ string,
	_ proto.Message) error {

	return nil
}

// lastRegisterReceiveScriptRequest returns the last recorded registration
// request.
func (r *recordingRPCClient) lastRegisterReceiveScriptRequest(
	t *testing.T) *arkrpc.RegisterReceiveScriptRequest {

	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	req, ok := r.lastReq.(*arkrpc.RegisterReceiveScriptRequest)
	require.True(t, ok)

	return req
}
