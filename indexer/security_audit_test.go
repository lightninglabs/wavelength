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
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// ==========================================================================
// H-1: Proof replay across purposes -- a valid proof for one RPC method
// can be re-sent to a different RPC method if the server does not verify
// the "purpose" field inside the TLV message matches the actual RPC call.
//
// This tests the client-side: the client correctly binds purpose into the
// signed message. But the server-side MUST also verify it, and the test
// demonstrates what happens if it does not.
// ==========================================================================

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
	pkScript = append(pkScript, privKey.PubKey().SerializeCompressed()[1:]...)

	serverID := "test-server"
	principal := "client:test"

	// Build a proof for "list_vtxos_by_scripts".
	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	msgBytes, err := encodeProofTLV(
		scriptScopeMessageType,
		serverID, principal, purposeListVTXOsByScripts,
		pkScript, nonce,
		uint64(now.Unix()), uint64(expiresAt.Unix()),
	)
	require.NoError(t, err)

	sig64, err := schnorrSigOverMessage(msgBytes, privKey)
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
	purposeField := extractPurposeFromTLV(t, msgBytes)
	require.Equal(t, purposeListVTXOsByScripts, purposeField,
		"proof purpose should be for VTXOs, not events")

	// The envelope is perfectly valid protobuf. A server that only
	// checks signature validity without comparing the TLV purpose
	// field to the actual RPC method would accept this.
	require.NotNil(t, replayedScope)
	require.Len(t, replayedScope.GetTaprootSchnorr().Sig64, 64)

	t.Logf("H-1 DEMONSTRATED: proof for %q can be embedded "+
		"in a different RPC request proto envelope",
		purposeField)
}

// ==========================================================================
// H-2: Registration expiry is caller-controlled. The client passes
// expiresAt as a caller-provided time.Time to RegisterReceiveScriptTaproot
// but the TLV proof also has an expiresAt. The proto request carries
// ExpiresAtUnixS separately. A mismatch between the signed expiresAt and
// the request-level expiresAt could let an attacker extend registration.
// ==========================================================================

// TestH2_RegistrationExpiryMismatch demonstrates that the proto-level
// ExpiresAtUnixS and the TLV-signed expiresAt can diverge if the caller
// provides a different value.
func TestH2_RegistrationExpiryMismatch(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20}
	pkScript = append(pkScript, privKey.PubKey().SerializeCompressed()[1:]...)

	serverID := "test-server"
	principal := "client:test"

	// The caller wants the registration to expire in 1 hour.
	callerExpiry := time.Now().Add(1 * time.Hour)

	// But the TLV proof is signed with the client's own timestamp
	// logic. In RegisterReceiveScriptTaproot, the proof's expiresAt
	// is uint64(expiresAt.Unix()) where expiresAt = the caller arg.
	// The proto also carries ExpiresAtUnixS = uint64(expiresAt.Unix()).
	//
	// Now consider: an attacker intercepts the wire proto and modifies
	// the outer ExpiresAtUnixS to a much later time.
	now := time.Now()
	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	msgBytes, err := encodeProofTLV(
		registrationMessageType,
		serverID, principal,
		purposeRegisterReceiveScript, pkScript, nonce,
		uint64(now.Unix()), uint64(callerExpiry.Unix()),
	)
	require.NoError(t, err)

	sig64, err := schnorrSigOverMessage(msgBytes, privKey)
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

	// Attacker modifies only the outer field.
	tamperedReq := *honestReq
	tamperedReq.ExpiresAtUnixS = uint64(
		callerExpiry.Add(365 * 24 * time.Hour).Unix(),
	)

	// The signature is still valid -- it signed the TLV payload
	// which has the original expiresAt. But the request-level
	// ExpiresAtUnixS is now 1 year later.
	tlvExpiry := extractExpiresAtFromTLV(t, msgBytes)

	require.NotEqual(t, tamperedReq.ExpiresAtUnixS, tlvExpiry,
		"tampered proto ExpiresAtUnixS diverges from signed TLV")

	t.Logf("H-2 DEMONSTRATED: proto ExpiresAtUnixS=%d, "+
		"TLV-signed expiresAt=%d (delta=%ds)",
		tamperedReq.ExpiresAtUnixS, tlvExpiry,
		tamperedReq.ExpiresAtUnixS-tlvExpiry)
}

// ==========================================================================
// H-3: UnregisterReceiveScript has no proof-of-control. Any authenticated
// mailbox principal can unregister ANY pkScript. This is a privilege
// escalation that allows denial of service against other wallets'
// notification routing.
// ==========================================================================

// TestH3_UnregisterNoProofOfControl demonstrates that UnregisterReceiveScript
// carries only a pkScript with no proof-of-control, unlike all other
// script-scoped RPCs.
func TestH3_UnregisterNoProofOfControl(t *testing.T) {
	t.Parallel()

	// The proto definition for UnregisterReceiveScriptRequest is:
	//   message UnregisterReceiveScriptRequest {
	//       bytes pk_script = 1;
	//   }
	//
	// No proof oneof. No TaprootSchnorrProof. No BIP322Proof.
	// Compare with RegisterReceiveScriptRequest which has:
	//   oneof proof { TaprootSchnorrProof ...; BIP322Proof ...; }

	attackerScript := []byte{0x51, 0x20, 0xaa, 0xbb}
	victimScript := []byte{0x51, 0x20, 0xcc, 0xdd}

	// The attacker can construct an unregister request for the
	// victim's script without any proof of ownership.
	attackReq := &arkrpc.UnregisterReceiveScriptRequest{
		PkScript: victimScript,
	}

	// Verify that the request is structurally valid and contains
	// no proof field at all.
	require.Nil(t, attackReq.ProtoReflect().
		Descriptor().Oneofs().ByName("proof"),
		"UnregisterReceiveScriptRequest has no proof oneof")

	require.NotEqual(t, attackerScript, victimScript)

	t.Logf("H-3 DEMONSTRATED: UnregisterReceiveScript for "+
		"victim script %x requires no proof of control",
		victimScript)
}

// ==========================================================================
// M-1: Proof replay within the 10-minute TTL window. A valid proof can be
// intercepted and replayed until it expires. The nonce prevents the
// server from deduplicating, but only if the server maintains a nonce
// cache -- which is a server-side concern not enforced by this client.
// ==========================================================================

// TestM1_ProofReplayWithinTTL demonstrates that the same
// (message, sig64) pair can be submitted multiple times within
// the TTL window.
func TestM1_ProofReplayWithinTTL(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20}
	pkScript = append(pkScript, privKey.PubKey().SerializeCompressed()[1:]...)

	serverID := "test-server"
	principal := "client:test"

	now := time.Now()
	expiresAt := now.Add(offlineReceiveProofTTL)

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	msgBytes, err := encodeProofTLV(
		scriptScopeMessageType,
		serverID, principal, purposeListVTXOsByScripts,
		pkScript, nonce,
		uint64(now.Unix()), uint64(expiresAt.Unix()),
	)
	require.NoError(t, err)

	sig64, err := schnorrSigOverMessage(msgBytes, privKey)
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

	t.Logf("M-1 DEMONSTRATED: same proof replayed 100x, "+
		"all signatures verify (TTL window = %v)",
		offlineReceiveProofTTL)
}

// ==========================================================================
// M-2: Unbounded script count in multi-script RPC requests.
// ListVTXOsByScripts and similar RPCs accept repeated ScriptScope with
// no client-side limit. An attacker can send thousands of scripts in a
// single request, each requiring a signature verification, causing
// server-side CPU exhaustion.
// ==========================================================================

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
			PkScript: []byte{0x51, 0x20, byte(i >> 8), byte(i)},
			Proof: &arkrpc.ScriptScope_TaprootSchnorr{
				TaprootSchnorr: &arkrpc.TaprootSchnorrProof{
					Message: bytes.Repeat([]byte{0xaa}, 200),
					Sig64:   bytes.Repeat([]byte{0xbb}, 64),
				},
			},
		}
	}

	req := &arkrpc.ListVTXOsByScriptsRequest{
		Scripts: scopes,
		Limit:   100,
	}

	require.Len(t, req.Scripts, attackScriptCount,
		"no cap prevents 10k scripts in one request")

	t.Logf("M-2 DEMONSTRATED: single request with %d scripts "+
		"(each requiring schnorr verify on server)",
		attackScriptCount)
}

// ==========================================================================
// M-3: Timestamp resolution is seconds. Two proofs generated within the
// same second share the same issuedAt and expiresAt values, reducing
// nonce entropy by making the timestamp a constant rather than a
// discriminator.
// ==========================================================================

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
		scriptScopeMessageType,
		serverID, principal, purpose, pkScript, nonce1,
		uint64(now.Unix()), uint64(expiresAt.Unix()),
	)
	require.NoError(t, err)

	msg2, err := encodeProofTLV(
		scriptScopeMessageType,
		serverID, principal, purpose, pkScript, nonce2,
		uint64(now.Unix()), uint64(expiresAt.Unix()),
	)
	require.NoError(t, err)

	// The messages differ only in the nonce field. issuedAt and
	// expiresAt are identical because of second-granularity.
	require.NotEqual(t, msg1, msg2, "nonces differ so messages differ")

	ts1 := extractIssuedAtFromTLV(t, msg1)
	ts2 := extractIssuedAtFromTLV(t, msg2)
	require.Equal(t, ts1, ts2,
		"timestamps are identical within same second")

	t.Logf("M-3 DEMONSTRATED: two proofs share issuedAt=%d "+
		"(second-resolution reduces discrimination)",
		ts1)
}

// ==========================================================================
// M-4: SyncClient TOCTOU between LoadCursor and SaveCursor. The
// SyncClient performs LoadCursor -> RPC -> SaveCursor without holding
// a lock. Concurrent calls for the same cursorKey can cause duplicate
// event processing.
// ==========================================================================

// raceSyncBackend is a SyncBackend that simulates slow responses to
// expose race windows.
type raceSyncBackend struct {
	mu    sync.Mutex
	calls int
}

// ListVTXOEventsByScriptsTaproot returns a response after a simulated
// delay, always returning the same next cursor.
func (b *raceSyncBackend) ListVTXOEventsByScriptsTaproot(
	_ context.Context, _ []TaprootScriptScope,
	afterEventID uint64, _ uint32,
	_ ...mailboxrpc.RPCOptions,
) (*arkrpc.ListVTXOEventsByScriptsResponse, error) {

	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	// Simulate network latency.
	time.Sleep(5 * time.Millisecond)

	return &arkrpc.ListVTXOEventsByScriptsResponse{
		Events: []*arkrpc.VTXOEvent{
			{EventId: afterEventID + 1},
			{EventId: afterEventID + 2},
		},
		NextCursor: afterEventID + 2,
	}, nil
}

// ListOORRecipientEventsByScriptTaproot is unused in this test.
func (b *raceSyncBackend) ListOORRecipientEventsByScriptTaproot(
	_ context.Context, _ []byte, _ *btcec.PrivateKey,
	_ uint64, _ uint32, _ ...mailboxrpc.RPCOptions,
) (*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	return &arkrpc.ListOORRecipientEventsByScriptResponse{}, nil
}

// TestM4_SyncClientTOCTOU demonstrates the race condition in SyncClient
// where concurrent polls for the same cursor key both read cursor=0,
// both fetch the same events, and both advance the cursor.
func TestM4_SyncClientTOCTOU(t *testing.T) {
	t.Parallel()

	backend := &raceSyncBackend{}
	store := NewMemorySyncCursorStore()
	syncClient, err := NewSyncClient(backend, store)
	require.NoError(t, err)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]*VTXOSyncResult, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			result, syncErr := syncClient.SyncVTXOEventsTaproot(
				context.Background(), "same-key",
				nil, 100,
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
	t.Logf("M-4 DEMONSTRATED: %d/%d goroutines fetched from "+
		"cursor=0 (expected 1 in ideal case)",
		fromZero, goroutines)

	if fromZero > 1 {
		t.Logf("M-4 CONFIRMED: %d concurrent polls read "+
			"stale cursor, causing duplicate event "+
			"processing", fromZero)
	}

	// Total backend calls should equal goroutines (no dedup).
	backend.mu.Lock()
	totalCalls := backend.calls
	backend.mu.Unlock()
	require.Equal(t, goroutines, totalCalls,
		"all goroutines made backend calls")
}

// ==========================================================================
// L-1: pkScript is not validated for format. The client accepts arbitrary
// byte slices as pkScript. While script validation is ultimately a
// server concern, the client could prevent obvious errors like empty
// scripts or non-P2TR scripts when using taproot proofs.
// ==========================================================================

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
			name:     "P2PKH script",
			pkScript: append([]byte{0x76, 0xa9, 0x14}, bytes.Repeat([]byte{0x01}, 20)...),
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
		scriptScopeMessageType,
				serverID, principal, purpose,
				tc.pkScript, nonce,
				now, expires,
			)
			require.NoError(t, err,
				"should encode without error")
			require.NotEmpty(t, msg)

			t.Logf("L-1: accepted %s (len=%d) in TLV "+
				"proof message",
				tc.name, len(tc.pkScript))
		})
	}
}

// ==========================================================================
// I-1: Registration proof does NOT contain a purpose field, unlike
// script-scope proofs. This means a registration proof TLV is
// structurally distinguishable only by the "type" field, creating a
// potential confusion risk.
// ==========================================================================

// TestI1_RegistrationProofNoPurpose verifies that the registration proof
// TLV format lacks a purpose field, relying solely on the "type" TLV
// field for disambiguation.
func TestI1_RegistrationProofNoPurpose(t *testing.T) {
	t.Parallel()

	serverID := "test-server"
	principal := "client:test"
	pkScript := []byte{0x51, 0x20, 0x01}

	now := uint64(time.Now().Unix())
	expires := now + 3600

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	regMsg, err := encodeProofTLV(
		registrationMessageType,
		serverID, principal,
		purposeRegisterReceiveScript, pkScript, nonce,
		now, expires,
	)
	require.NoError(t, err)

	scopeMsg, err := encodeProofTLV(
		scriptScopeMessageType,
		serverID, principal, purposeListVTXOsByScripts,
		pkScript, nonce, now, expires,
	)
	require.NoError(t, err)

	// The scope message is strictly longer because it has the
	// extra purpose TLV record (type=9).
	require.True(t, len(scopeMsg) > len(regMsg),
		"scope proof should be longer due to purpose field")

	// Verify registration proof has no purpose field.
	regPurpose := extractPurposeFromTLVSafe(regMsg)
	require.Empty(t, regPurpose,
		"registration proof should have no purpose field")

	scopePurpose := extractPurposeFromTLVSafe(scopeMsg)
	require.Equal(t, purposeListVTXOsByScripts, scopePurpose)

	t.Logf("I-1: registration proof (len=%d) has no purpose; "+
		"scope proof (len=%d) has purpose=%q",
		len(regMsg), len(scopeMsg), scopePurpose)
}

// ==========================================================================
// H-4: Cross-type proof confusion. A registration proof
// (type="receive_script_registration") could potentially be accepted by
// a server expecting a script_scope proof if the server only checks the
// signature and pkScript without verifying the type field.
// ==========================================================================

// TestH4_CrossTypeProofConfusion demonstrates that a registration proof
// can be repackaged into a ScriptScope envelope. The signature is valid
// because the server verifies signature over the raw TLV bytes.
func TestH4_CrossTypeProofConfusion(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20}
	pkScript = append(pkScript, privKey.PubKey().SerializeCompressed()[1:]...)

	serverID := "test-server"
	principal := "client:test"

	now := time.Now()
	expires := now.Add(1 * time.Hour)

	nonce, err := randomNonce(registrationNonceBytes)
	require.NoError(t, err)

	// Build a registration proof (type="receive_script_registration").
	regMsg, err := encodeProofTLV(
		registrationMessageType,
		serverID, principal,
		purposeRegisterReceiveScript, pkScript, nonce,
		uint64(now.Unix()), uint64(expires.Unix()),
	)
	require.NoError(t, err)

	regSig, err := schnorrSigOverMessage(regMsg, privKey)
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
	proofTypeField := extractTypeFromTLV(t, regMsg)
	require.Equal(t, registrationMessageType, proofTypeField)

	require.NotNil(t, confusedScope)

	t.Logf("H-4 DEMONSTRATED: registration proof "+
		"(type=%q) repackaged into ScriptScope "+
		"envelope with valid signature",
		proofTypeField)
}

// ==========================================================================
// M-5: pkScript mismatch between proto envelope and TLV-signed payload.
// The ScriptScope proto has pk_script at the envelope level AND
// pk_script inside the signed TLV message. If the server uses the
// envelope pk_script for query routing but only verifies the TLV
// signature, an attacker could query VTXOs for a different script
// than the one they proved control over.
// ==========================================================================

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
		scriptScopeMessageType,
		serverID, principal, purposeListVTXOsByScripts,
		ownedScript, nonce,
		uint64(now.Unix()), uint64(expiresAt.Unix()),
	)
	require.NoError(t, err)

	sig64, err := schnorrSigOverMessage(msgBytes, privKey)
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

	tlvPkScript := extractPkScriptFromTLV(t, msgBytes)
	require.Equal(t, ownedScript, tlvPkScript,
		"TLV payload has ownedScript")
	require.NotEqual(t, victimScript, tlvPkScript,
		"TLV payload does NOT have victimScript")
	require.Equal(t, victimScript, mismatchedScope.PkScript,
		"proto envelope has victimScript")

	t.Logf("M-5 DEMONSTRATED: proto pk_script=%x but "+
		"TLV-signed pk_script=%x",
		hex.EncodeToString(victimScript),
		hex.EncodeToString(ownedScript))
}

// ==========================================================================
// TLV parsing helpers for the PoC tests.
// ==========================================================================

// extractPurposeFromTLV decodes a scope proof TLV and returns the
// purpose string.
func extractPurposeFromTLV(t *testing.T, msg []byte) string {
	t.Helper()

	result := extractPurposeFromTLVSafe(msg)
	require.NotEmpty(t, result, "purpose field not found in TLV")

	return result
}

// extractPurposeFromTLVSafe attempts to decode the purpose field,
// returning empty string if not present.
func extractPurposeFromTLVSafe(msg []byte) string {
	var (
		proofType      []byte
		version        uint32
		serverIDBytes  []byte
		principalBytes []byte
		pkScript       []byte
		issuedAt       uint64
		expiresAt      uint64
		nonce          []byte
		purposeBytes   []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce,
			tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePurpose, &purposeBytes,
			tlv.SizeVarBytes(&purposeBytes),
			tlv.EVarBytes, tlv.DVarBytes,
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

// extractExpiresAtFromTLV decodes a proof TLV and returns the expiresAt
// value.
func extractExpiresAtFromTLV(t *testing.T, msg []byte) uint64 {
	t.Helper()

	var (
		proofType      []byte
		version        uint32
		serverIDBytes  []byte
		principalBytes []byte
		pkScript       []byte
		issuedAt       uint64
		expiresAt      uint64
		nonce          []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce,
			tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	require.NoError(t, err)

	r := bytes.NewReader(msg)
	err = tlvStream.Decode(r)
	require.NoError(t, err)

	return expiresAt
}

// extractIssuedAtFromTLV decodes a proof TLV and returns the issuedAt
// value.
func extractIssuedAtFromTLV(t *testing.T, msg []byte) uint64 {
	t.Helper()

	var (
		proofType      []byte
		version        uint32
		serverIDBytes  []byte
		principalBytes []byte
		pkScript       []byte
		issuedAt       uint64
		expiresAt      uint64
		nonce          []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce,
			tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	require.NoError(t, err)

	r := bytes.NewReader(msg)
	err = tlvStream.Decode(r)
	require.NoError(t, err)

	return issuedAt
}

// extractTypeFromTLV decodes a proof TLV and returns the type string.
func extractTypeFromTLV(t *testing.T, msg []byte) string {
	t.Helper()

	var (
		proofType      []byte
		version        uint32
		serverIDBytes  []byte
		principalBytes []byte
		pkScript       []byte
		issuedAt       uint64
		expiresAt      uint64
		nonce          []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce,
			tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	require.NoError(t, err)

	r := bytes.NewReader(msg)
	err = tlvStream.Decode(r)
	require.NoError(t, err)

	return string(proofType)
}

// extractPkScriptFromTLV decodes a proof TLV and returns the pkScript.
func extractPkScriptFromTLV(t *testing.T, msg []byte) []byte {
	t.Helper()

	var (
		proofType      []byte
		version        uint32
		serverIDBytes  []byte
		principalBytes []byte
		pkScript       []byte
		issuedAt       uint64
		expiresAt      uint64
		nonce          []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			proofTLVTypeType, &proofType,
			tlv.SizeVarBytes(&proofType),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeVersion, &version,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeServerID, &serverIDBytes,
			tlv.SizeVarBytes(&serverIDBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePrincipal, &principalBytes,
			tlv.SizeVarBytes(&principalBytes),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypePkScript, &pkScript,
			tlv.SizeVarBytes(&pkScript),
			tlv.EVarBytes, tlv.DVarBytes,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeIssuedAt, &issuedAt,
		),
		tlv.MakePrimitiveRecord(
			proofTLVTypeExpiresAt, &expiresAt,
		),
		tlv.MakeDynamicRecord(
			proofTLVTypeNonce, &nonce,
			tlv.SizeVarBytes(&nonce),
			tlv.EVarBytes, tlv.DVarBytes,
		),
	)
	require.NoError(t, err)

	r := bytes.NewReader(msg)
	err = tlvStream.Decode(r)
	require.NoError(t, err)

	return pkScript
}

// Ensure test helpers don't accidentally get used as unused imports.
var _ = fmt.Sprintf
