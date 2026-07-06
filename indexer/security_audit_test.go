package indexer

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestH1_CrossPurposeProofReplay demonstrates that a proof generated for
// purposeListVTXOsByScripts can be trivially repackaged as a ScriptScope
// in a ListVTXOEventsByScripts request. The signed TLV message is
// purpose-bound, but the outer proto envelope has no additional binding,
// meaning a lazy server that skips the purpose check would accept it.
func TestH1_CrossPurposeProofReplay(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20}
	pkScript = append(
		pkScript, privKey.PubKey().SerializeCompressed()[1:]...,
	)

	serverID := "test-server"
	principal := "client:test"

	// Build a proof for "list_vtxos_by_scripts".
	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	msgBytes, err := encodeProofTLV(
		scriptScopeMessageType, serverID, principal,
		purposeListVTXOsByScripts, pkScript, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	require.NoError(t, err)

	signer := &PrivKeySchnorrSigner{Key: privKey}
	sig64, err := schnorrSigOverMessage(
		t.Context(), msgBytes, pkScript, signer,
	)
	require.NoError(t, err)

	// The attacker now takes the (msgBytes, sig64) pair -- which was
	// intended for ListVTXOsByScripts -- and packages it into a
	// ListVTXOEventsByScripts request. The signature is mathematically
	// valid because it was made by the same key.
	replayedScope := &arkrpc.ScriptScope{
		PkScript: pkScript,
		Proof: &arkrpc.ScriptScope_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgBytes,
				Sig64:   sig64,
			},
		},
	}

	// Decode the TLV and verify the purpose field says
	// "list_vtxos_by_scripts" NOT "list_vtxo_events_by_scripts".
	decoded := decodeProofTLVBytes(t, msgBytes)
	require.Equal(
		t, purposeListVTXOsByScripts, decoded.purpose,
		"proof purpose should be for VTXOs, not events",
	)

	// The envelope is perfectly valid protobuf. A server that only
	// checks signature validity without comparing the TLV purpose
	// field to the actual RPC method would accept this.
	require.NotNil(t, replayedScope)
	require.Len(t, replayedScope.GetTaprootSchnorr().Sig64, 64)

	t.Logf(
		"Proof for %q can be embedded in a different RPC request "+
			"proto envelope", decoded.purpose,
	)
}

// TestH2_RegistrationExpiryMismatch demonstrates that the proto-level
// ExpiresAtUnixS and the TLV-signed expiresAt intentionally diverge for
// registrations with long retention, and that tampering the outer field widens
// that divergence further.
func TestH2_RegistrationExpiryMismatch(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20}
	pkScript = append(
		pkScript, privKey.PubKey().SerializeCompressed()[1:]...,
	)

	serverID := "test-server"
	principal := "client:test"

	// The caller wants the registration to remain registered for 1 hour.
	callerExpiry := time.Now().Add(1 * time.Hour)

	// The real client signs the TLV proof with a short replay window while
	// the outer request keeps the longer registration-retention deadline.
	proofExpiry := time.Now().Add(offlineReceiveProofTTL)
	//
	// Now consider: an attacker intercepts the wire proto and modifies
	// the outer ExpiresAtUnixS to a much later time.
	now := time.Now()
	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	msgBytes, err := encodeProofTLV(
		registrationMessageType, serverID, principal,
		purposeRegisterReceiveScript, pkScript, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			proofExpiry.Unix(),
		),
	)
	require.NoError(t, err)

	signer := &PrivKeySchnorrSigner{Key: privKey}
	sig64, err := schnorrSigOverMessage(
		t.Context(), msgBytes, pkScript, signer,
	)
	require.NoError(t, err)

	// Build the registration request as the honest client would.
	honestReq := &arkrpc.RegisterReceiveScriptRequest{
		PkScript:       pkScript,
		ExpiresAtUnixS: uint64(callerExpiry.Unix()),
		Proof: &arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgBytes,
				Sig64:   sig64,
			},
		},
	}

	// Attacker modifies only the outer field. Use proto.Clone to
	// avoid copying the internal sync.Mutex in MessageState.
	cloned := proto.Clone(honestReq)
	tamperedReq, ok := cloned.(*arkrpc.RegisterReceiveScriptRequest)
	require.True(t, ok)
	tamperedReq.ExpiresAtUnixS = uint64(
		callerExpiry.Add(365 * 24 * time.Hour).Unix(),
	)

	// The signature is still valid because it covers the TLV payload
	// only. The request-level ExpiresAtUnixS is now 1 year later.
	tlvExpiry := decodeProofTLVBytes(t, msgBytes).expiresAt

	require.NotEqual(
		t, tamperedReq.ExpiresAtUnixS, tlvExpiry,
		"tampered proto ExpiresAtUnixS diverges from signed TLV",
	)

	t.Logf(
		"Proto ExpiresAtUnixS=%d, TLV-signed expiresAt=%d (delta=%ds)",
		tamperedReq.ExpiresAtUnixS, tlvExpiry,
		tamperedReq.ExpiresAtUnixS-tlvExpiry,
	)
}

// TestUnregisterRequiresProofOfControl verifies that
// UnregisterReceiveScriptRequest carries a proof oneof and that the
// client populates it with a valid TaprootSchnorrProof.
func TestUnregisterRequiresProofOfControl(t *testing.T) {
	t.Parallel()

	// Verify the proto now has a proof oneof.
	req := &arkrpc.UnregisterReceiveScriptRequest{}
	proofOneof := req.ProtoReflect().
		Descriptor().Oneofs().ByName("proof")
	require.NotNil(
		t, proofOneof,
		"UnregisterReceiveScriptRequest must have proof oneof",
	)

	// Verify the oneof has the expected fields.
	require.Equal(
		t, 2, proofOneof.Fields().Len(),
		"proof oneof should have taproot_schnorr and bip322",
	)

	taprootField := proofOneof.Fields().ByName("taproot_schnorr")
	require.NotNil(t, taprootField)

	bip322Field := proofOneof.Fields().ByName("bip322")
	require.NotNil(t, bip322Field)

	t.Log(
		"UnregisterReceiveScriptRequest has proof oneof with " +
			"taproot_schnorr and bip322",
	)
}

// TestM1_ProofReplayWithinTTL demonstrates that the same
// (message, sig64) pair can be submitted multiple times within
// the TTL window.
func TestM1_ProofReplayWithinTTL(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20}
	pkScript = append(
		pkScript, privKey.PubKey().SerializeCompressed()[1:]...,
	)

	serverID := "test-server"
	principal := "client:test"

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	msgBytes, err := encodeProofTLV(
		scriptScopeMessageType, serverID, principal,
		purposeListVTXOsByScripts, pkScript, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	require.NoError(t, err)

	signer := &PrivKeySchnorrSigner{Key: privKey}
	sig64, err := schnorrSigOverMessage(
		t.Context(), msgBytes, pkScript, signer,
	)
	require.NoError(t, err)

	// Replay the exact same proof 100 times. Each is
	// cryptographically valid. Without server-side nonce tracking,
	// all 100 would be accepted.
	for i := 0; i < 100; i++ {
		msgHash := chainhash.TaggedHash(proofTag(), msgBytes)
		sig, err := schnorr.ParseSignature(sig64)
		require.NoError(t, err)
		require.True(t, sig.Verify(
			msgHash[:], privKey.PubKey(),
		))
	}

	t.Logf(
		"Same proof replayed 100x, all signatures verify (TTL "+
			"window = %v)", offlineReceiveProofTTL,
	)
}

// TestM2_UnboundedScriptCount demonstrates there is no cap on the number
// of ScriptScope entries that can be packed into a single request.
func TestM2_UnboundedScriptCount(t *testing.T) {
	t.Parallel()

	// In a real attack, we would generate N distinct keys and
	// sign N proofs. Here we demonstrate the structural issue.
	const attackScriptCount = 10_000

	scopes := make([]*arkrpc.ScriptScope, attackScriptCount)
	for i := range scopes {
		scopes[i] = &arkrpc.ScriptScope{
			PkScript: []byte{
				0x51,
				0x20,
				byte(i >> 8),
				byte(i),
			},
			Proof: &arkrpc.ScriptScope_TaprootSchnorr{
				TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
					Message: bytes.Repeat(
						[]byte{0xaa}, 200,
					),
					Sig64: bytes.Repeat([]byte{0xbb}, 64),
				},
			},
		}
	}

	req := &arkrpc.ListVTXOsByScriptsRequest{
		Scripts: scopes,
		Limit:   100,
	}

	require.Len(
		t, req.Scripts, attackScriptCount,
		"no cap prevents 10k scripts in one request",
	)

	t.Logf(
		"Single request with %d scripts (each requiring schnorr "+
			"verify on server)", attackScriptCount,
	)
}

// TestM3_SecondResolutionTimestamp demonstrates that proofs generated
// within the same second produce identical issuedAt/expiresAt values.
func TestM3_SecondResolutionTimestamp(t *testing.T) {
	t.Parallel()

	serverID := "s"
	principal := "p"
	pkScript := []byte{0x51, 0x20, 0x01}
	purpose := purposeListVTXOsByScripts

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce1, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)
	nonce2, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	// Same second, different nonces.
	msg1, err := encodeProofTLV(
		scriptScopeMessageType, serverID, principal, purpose, pkScript,
		nonce1,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	require.NoError(t, err)

	msg2, err := encodeProofTLV(
		scriptScopeMessageType, serverID, principal, purpose, pkScript,
		nonce2,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	require.NoError(t, err)

	// The messages differ only in the nonce field. issuedAt and
	// expiresAt are identical because of second-granularity.
	require.NotEqual(t, msg1, msg2, "nonces differ so messages differ")

	ts1 := decodeProofTLVBytes(t, msg1).issuedAt
	ts2 := decodeProofTLVBytes(t, msg2).issuedAt
	require.Equal(
		t, ts1, ts2, "timestamps are identical within same second",
	)

	t.Logf(
		"Two proofs share issuedAt=%d (second-resolution reduces "+
			"discrimination)", ts1,
	)
}

// raceSyncBackend is a SyncBackend that simulates slow responses to
// expose race windows.
type raceSyncBackend struct {
	mu    sync.Mutex
	calls int
}

// ListVTXOEventsByScriptsTaproot returns a response after a simulated
// delay, always returning the same next cursor.
func (b *raceSyncBackend) ListVTXOEventsByScriptsTaproot(_ context.Context,
	_ []TaprootScriptScope, afterEventID uint64, _ uint32,
	_ ...mailboxrpc.RPCOptions) (*arkrpc.ListVTXOEventsByScriptsResponse,
	error) {

	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	// Simulate network latency.
	time.Sleep(5 * time.Millisecond)

	return &arkrpc.ListVTXOEventsByScriptsResponse{
		Events: []*arkrpc.VTXOEvent{
			{
				EventId: afterEventID + 1,
			},
			{
				EventId: afterEventID + 2,
			},
		},
		NextCursor: afterEventID + 2,
	}, nil
}

// ListOORRecipientEventsByScriptTaproot is unused in this test.
func (b *raceSyncBackend) ListOORRecipientEventsByScriptTaproot(
	_ context.Context, _ []byte, _ uint64, _ uint32,
	_ ...mailboxrpc.RPCOptions) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	return &arkrpc.ListOORRecipientEventsByScriptResponse{}, nil
}

// TestM4_SyncClientTOCTOU demonstrates the race condition in SyncClient
// where concurrent polls for the same cursor key both read cursor=0,
// both fetch the same events, and both advance the cursor.
func TestM4_SyncClientTOCTOU(t *testing.T) {
	t.Parallel()

	backend := &raceSyncBackend{}
	store := NewMemorySyncCursorStore()
	syncClient, err := NewSyncClient(
		backend, store, fn.None[btclog.Logger](),
	)
	require.NoError(t, err)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]*VTXOSyncResult, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			result, syncErr := syncClient.SyncVTXOEventsTaproot(
				t.Context(),
				"same-key", nil, 100,
			)
			if syncErr == nil {
				// Ack immediately for the race test.
				_ = result.Ack()
				results[idx] = result
			}
		}(i)
	}

	wg.Wait()

	// Count how many goroutines fetched events starting from
	// cursor=0 (i.e., got the first page of events).
	var fromZero int
	for _, result := range results {
		if result != nil &&
			len(result.Response.Events) > 0 &&
			result.Response.Events[0].EventId == 1 {

			fromZero++
		}
	}

	// In a correct implementation, only ONE goroutine should
	// fetch from cursor=0. But without locking across the full
	// load-fetch-save cycle, multiple will.
	t.Logf(
		"%d/%d goroutines fetched from cursor=0 (expected 1 in "+
			"ideal case)", fromZero, goroutines,
	)

	if fromZero > 1 {
		t.Logf(
			"%d concurrent polls read stale cursor, causing "+
				"duplicate event processing", fromZero,
		)
	}

	// Total backend calls should equal goroutines (no dedup).
	backend.mu.Lock()
	totalCalls := backend.calls
	backend.mu.Unlock()
	require.Equal(
		t, goroutines, totalCalls, "all goroutines made backend calls",
	)
}

// TestL1_ArbitraryPkScriptAccepted demonstrates that encodeProofTLV
// happily encodes any byte slice as a pkScript -- including non-P2TR
// scripts, empty scripts, and oversized payloads.
func TestL1_ArbitraryPkScriptAccepted(t *testing.T) {
	t.Parallel()

	serverID := "s"
	principal := "p"
	purpose := purposeListVTXOsByScripts
	now := uint64(time.Now().Unix())
	expires := now + 600

	tests := []struct {
		name     string
		pkScript []byte
	}{
		{
			name:     "empty script",
			pkScript: []byte{},
		},
		{
			name: "P2PKH script",
			pkScript: append(
				[]byte{0x76, 0xa9, 0x14},
				bytes.Repeat([]byte{0x01}, 20)...,
			),
		},
		{
			name:     "oversized script (1MB)",
			pkScript: bytes.Repeat([]byte{0x51}, 1024*1024),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nonce, err := randomNonce(registrationNonceBytes)
			require.NoError(t, err)

			msg, err := encodeProofTLV(
				scriptScopeMessageType, serverID, principal,
				purpose, tc.pkScript, nonce, now, expires,
			)
			require.NoError(t, err,
				"should encode without error")
			require.NotEmpty(t, msg)

			t.Logf(
				"Accepted %s (len=%d) in TLV proof message",
				tc.name, len(tc.pkScript),
			)
		})
	}
}

// TestRegistrationProofHasPurpose verifies that the registration proof
// TLV format includes a purpose field, matching the structure of
// script-scope proofs.
func TestRegistrationProofHasPurpose(t *testing.T) {
	t.Parallel()

	serverID := "test-server"
	principal := "client:test"
	pkScript := []byte{0x51, 0x20, 0x01}

	now := uint64(time.Now().Unix())
	expires := now + 3600

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	regMsg, err := encodeProofTLV(
		registrationMessageType, serverID, principal,
		purposeRegisterReceiveScript, pkScript, nonce, now, expires,
	)
	require.NoError(t, err)

	scopeMsg, err := encodeProofTLV(
		scriptScopeMessageType, serverID, principal,
		purposeListVTXOsByScripts, pkScript, nonce, now, expires,
	)
	require.NoError(t, err)

	// Both messages now include a purpose field. Verify the
	// registration proof carries its expected purpose.
	regPurpose := extractPurposeFromTLVSafe(regMsg)
	require.Equal(
		t, purposeRegisterReceiveScript, regPurpose,
		"registration proof must have purpose field",
	)

	scopePurpose := extractPurposeFromTLVSafe(scopeMsg)
	require.Equal(t, purposeListVTXOsByScripts, scopePurpose)

	// The two purposes must differ to prevent cross-purpose
	// replay.
	require.NotEqual(
		t, regPurpose, scopePurpose,
		"registration and scope proofs must have distinct purposes",
	)

	t.Logf(
		"Registration proof has purpose=%q; scope proof has purpose=%q",
		regPurpose, scopePurpose,
	)
}

// TestH4_CrossTypeProofConfusion demonstrates that a registration proof
// can be repackaged into a ScriptScope envelope. The signature is valid
// because the server verifies signature over the raw TLV bytes.
func TestH4_CrossTypeProofConfusion(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20}
	pkScript = append(
		pkScript, privKey.PubKey().SerializeCompressed()[1:]...,
	)

	serverID := "test-server"
	principal := "client:test"

	now := time.Now()
	expires := now.Add(1 * time.Hour)

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	// Build a registration proof (type="receive_script_registration").
	regMsg, err := encodeProofTLV(
		registrationMessageType, serverID, principal,
		purposeRegisterReceiveScript, pkScript, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			expires.Unix(),
		),
	)
	require.NoError(t, err)

	signer := &PrivKeySchnorrSigner{Key: privKey}
	regSig, err := schnorrSigOverMessage(
		t.Context(), regMsg, pkScript, signer,
	)
	require.NoError(t, err)

	// Package the registration proof into a ScriptScope envelope
	// (which is expected to carry a script_scope type proof).
	confusedScope := &arkrpc.ScriptScope{
		PkScript: pkScript,
		Proof: &arkrpc.ScriptScope_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: regMsg,
				Sig64:   regSig,
			},
		},
	}

	// The signature is valid over these bytes.
	msgHash := chainhash.TaggedHash(proofTag(), regMsg)
	sig, err := schnorr.ParseSignature(regSig)
	require.NoError(t, err)
	require.True(t, sig.Verify(msgHash[:], privKey.PubKey()))

	// Extract the type -- it says "receive_script_registration",
	// not "script_scope".
	decoded := decodeProofTLVBytes(t, regMsg)
	require.Equal(t, registrationMessageType, decoded.proofType)

	require.NotNil(t, confusedScope)

	t.Logf(
		"Registration proof (type=%q) repackaged into ScriptScope "+
			"envelope with valid signature", decoded.proofType,
	)
}

// TestM5_PkScriptEnvelopeMismatch demonstrates that the proto-level
// pk_script can diverge from the TLV-signed pk_script.
func TestM5_PkScriptEnvelopeMismatch(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// The attacker controls this key and can sign proofs for
	// ownedScript.
	ownedScript := []byte{0x51, 0x20}
	ownedScript = append(
		ownedScript, privKey.PubKey().SerializeCompressed()[1:]...,
	)

	// The attacker wants to query VTXOs for victimScript.
	victimScript := []byte{0x51, 0x20, 0xde, 0xad, 0xbe, 0xef}

	serverID := "test-server"
	principal := "client:test"

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	// Sign a proof for ownedScript.
	msgBytes, err := encodeProofTLV(
		scriptScopeMessageType, serverID, principal,
		purposeListVTXOsByScripts, ownedScript, nonce,
		uint64(
			now.Unix(),
		),
		uint64(
			expiresAt.Unix(),
		),
	)
	require.NoError(t, err)

	signer := &PrivKeySchnorrSigner{Key: privKey}
	sig64, err := schnorrSigOverMessage(
		t.Context(), msgBytes, ownedScript, signer,
	)
	require.NoError(t, err)

	// Construct a ScriptScope with victimScript in the envelope
	// but the proof signed for ownedScript.
	mismatchedScope := &arkrpc.ScriptScope{
		PkScript: victimScript, // Different from TLV payload!
		Proof: &arkrpc.ScriptScope_TaprootSchnorr{
			TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
				Message: msgBytes,
				Sig64:   sig64,
			},
		},
	}

	// Signature is still valid (it was signed over ownedScript).
	msgHash := chainhash.TaggedHash(proofTag(), msgBytes)
	sig, err := schnorr.ParseSignature(sig64)
	require.NoError(t, err)
	require.True(t, sig.Verify(msgHash[:], privKey.PubKey()))

	decoded := decodeProofTLVBytes(t, msgBytes)
	require.Equal(
		t, ownedScript, decoded.pkScript, "TLV payload has ownedScript",
	)
	require.NotEqual(
		t, victimScript, decoded.pkScript,
		"TLV payload does NOT have victimScript",
	)
	require.Equal(
		t, victimScript, mismatchedScope.PkScript,
		"proto envelope has victimScript",
	)

	t.Logf(
		"Proto pk_script=%x but TLV-signed pk_script=%x",
		hex.EncodeToString(victimScript),
		hex.EncodeToString(ownedScript),
	)
}

// decodedProof holds all fields decoded from a proof TLV message.
type decodedProof struct {
	proofType   string
	version     uint32
	serverID    string
	principal   string
	pkScript    []byte
	ownerPubKey []byte
	issuedAt    uint64
	expiresAt   uint64
	nonce       []byte
	purpose     string

	// hasPurpose indicates whether the purpose field was present
	// in the TLV stream.
	hasPurpose bool
}

// decodeProofTLVBytes decodes all fields from a TLV-encoded proof
// message into a decodedProof struct. This is the single test helper
// for extracting any field from a proof message.
func decodeProofTLVBytes(t *testing.T, msg []byte) decodedProof {
	t.Helper()

	var (
		proofType      []byte
		version        uint32
		serverIDBytes  []byte
		principalBytes []byte
		pkScript       []byte
		ownerPubKey    []byte
		issuedAt       uint64
		expiresAt      uint64
		nonce          []byte
		purposeBytes   []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce, tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purposeBytes,
			tlv.SizeVarBytes(&purposeBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeOwnerPubKey, &ownerPubKey,
			tlv.SizeVarBytes(&ownerPubKey), tlv.EVarBytes,
			tlv.DVarBytes,
		),
	)
	require.NoError(t, err)

	r := bytes.NewReader(msg)
	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	require.NoError(t, err)

	_, hasPurpose := parsedTypes[proofTLVTypePurpose]

	return decodedProof{
		proofType:   string(proofType),
		version:     version,
		serverID:    string(serverIDBytes),
		principal:   string(principalBytes),
		pkScript:    pkScript,
		ownerPubKey: ownerPubKey,
		issuedAt:    issuedAt,
		expiresAt:   expiresAt,
		nonce:       nonce,
		purpose:     string(purposeBytes),
		hasPurpose:  hasPurpose,
	}
}

// extractPurposeFromTLVSafe attempts to decode the purpose field,
// returning empty string if not present. Unlike decodeProofTLVBytes
// this does not require a *testing.T and returns "" on any error.
func extractPurposeFromTLVSafe(msg []byte) string {
	var (
		proofType      []byte
		version        uint32
		serverIDBytes  []byte
		principalBytes []byte
		pkScript       []byte
		ownerPubKey    []byte
		issuedAt       uint64
		expiresAt      uint64
		nonce          []byte
		purposeBytes   []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce, tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purposeBytes,
			tlv.SizeVarBytes(&purposeBytes), tlv.EVarBytes,
			tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeOwnerPubKey, &ownerPubKey,
			tlv.SizeVarBytes(&ownerPubKey), tlv.EVarBytes,
			tlv.DVarBytes,
		),
	)
	if err != nil {
		return ""
	}

	r := bytes.NewReader(msg)
	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return ""
	}

	if _, ok := parsedTypes[proofTLVTypePurpose]; !ok {
		return ""
	}

	return string(purposeBytes)
}

// Ensure test helpers don't accidentally get used as unused imports.
var _ = fmt.Sprintf
