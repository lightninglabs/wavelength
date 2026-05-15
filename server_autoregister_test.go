package darepo

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btclog/v2"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/serverconn"
	"github.com/stretchr/testify/require"
)

// newTLSBindingTestServer builds a minimal Server suitable for
// exercising verifyTLSBindingSig in isolation. The mailbox binding
// registry is initialized so a successful verify can be followed by
// a Bind call without panicking.
func newTLSBindingTestServer(t *testing.T, requireBinding bool) *Server {
	t.Helper()

	return &Server{
		cfg: &Config{
			Mailbox: &MailboxConfig{
				RequireTLSBindingSig: requireBinding,
			},
		},
		log:                btclog.Disabled,
		mailboxTLSBindings: newMailboxTLSBindings(),
	}
}

// signTLSBind is a small wrapper that signs over a (clientKey,
// leafSPKI) pair and returns the hex-encoded signature ready to be
// dropped into an envelope header.
func signTLSBind(t *testing.T, clientKey *btcec.PrivateKey,
	leafSPKI []byte) string {

	t.Helper()

	sig, err := serverconn.SignMailboxTLSBind(clientKey, leafSPKI)
	require.NoError(t, err)

	return hex.EncodeToString(sig.Serialize())
}

// TestVerifyTLSBindingSigHappyPath confirms a binding signature over
// the leaf actually presented on the connection verifies cleanly,
// which is the normal first-contact path that PR #443's
// fingerprint binding then commits to.
func TestVerifyTLSBindingSigHappyPath(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leafCert := makeTestCert(t, "alice")
	leafSPKI := leafCert.RawSubjectPublicKeyInfo

	sigHex := signTLSBind(t, clientKey, leafSPKI)

	env := &mailboxpb.Envelope{
		Sender: "alice",
		Headers: map[string]string{
			serverconn.TLSBindHeaderKey: sigHex,
		},
	}

	peerInfo, ok := extractTLSPeer(tlsPeerCtx(t, leafCert))
	require.True(t, ok)

	// Both soft (mandatory=false) and hard (mandatory=true)
	// modes must accept a correctly-signed binding.
	for _, mandatory := range []bool{false, true} {
		srv := newTLSBindingTestServer(t, mandatory)
		err = srv.verifyTLSBindingSig(
			t.Context(), env, clientKey.PubKey(), peerInfo,
		)
		require.NoError(t, err)
	}
}

// TestVerifyTLSBindingSigReplayAcrossSession is the core attacker
// model from issue #448: an adversary captures Alice's signed Send
// envelope (bytes + headers verbatim) and replays it across a TLS
// session backed by a different leaf the attacker controls. The
// binding sig is over Alice's *original* SPKI, not the attacker's
// observed SPKI, so verification must fail regardless of rollout
// mode.
func TestVerifyTLSBindingSigReplayAcrossSession(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	aliceLeaf := makeTestCert(t, "alice")
	attackerLeaf := makeTestCert(t, "alice")

	// Alice signs over her own leaf.
	sigHex := signTLSBind(t, clientKey, aliceLeaf.RawSubjectPublicKeyInfo)

	// The captured envelope is then replayed over an attacker
	// TLS session.
	env := &mailboxpb.Envelope{
		Sender: "alice",
		Headers: map[string]string{
			serverconn.TLSBindHeaderKey: sigHex,
		},
	}

	attackerPeer, ok := extractTLSPeer(tlsPeerCtx(t, attackerLeaf))
	require.True(t, ok)

	srv := newTLSBindingTestServer(t, false)
	err = srv.verifyTLSBindingSig(
		t.Context(), env, clientKey.PubKey(), attackerPeer,
	)
	require.Error(t, err)

	// And in hard mode the same envelope is rejected.
	srvHard := newTLSBindingTestServer(t, true)
	err = srvHard.verifyTLSBindingSig(
		t.Context(), env, clientKey.PubKey(), attackerPeer,
	)
	require.Error(t, err)
}

// TestVerifyTLSBindingSigMissingHeader covers both rollout modes
// for a pre-#448 client that never sets the TLS-binding header:
// soft mode logs and accepts; hard mode rejects.
func TestVerifyTLSBindingSigMissingHeader(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leafCert := makeTestCert(t, "alice")

	env := &mailboxpb.Envelope{
		Sender:  "alice",
		Headers: map[string]string{},
	}

	peerInfo, ok := extractTLSPeer(tlsPeerCtx(t, leafCert))
	require.True(t, ok)

	// Soft rollout (default): missing header is allowed so
	// legacy clients are not locked out during upgrade.
	softSrv := newTLSBindingTestServer(t, false)
	err = softSrv.verifyTLSBindingSig(
		t.Context(), env, clientKey.PubKey(), peerInfo,
	)
	require.NoError(t, err)

	// Hard mode: missing header is a registration-blocker.
	hardSrv := newTLSBindingTestServer(t, true)
	err = hardSrv.verifyTLSBindingSig(
		t.Context(), env, clientKey.PubKey(), peerInfo,
	)
	require.Error(t, err)
}

// TestVerifyTLSBindingSigWrongKey ensures a binding signature
// produced by a key other than the Schnorr-verified mailbox key is
// rejected, even when it covers the correct observed leaf SPKI.
func TestVerifyTLSBindingSigWrongKey(t *testing.T) {
	t.Parallel()

	aliceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	mallory, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leafCert := makeTestCert(t, "alice")

	// Mallory signs the binding instead of Alice.
	sigHex := signTLSBind(t, mallory, leafCert.RawSubjectPublicKeyInfo)

	env := &mailboxpb.Envelope{
		Sender: "alice",
		Headers: map[string]string{
			serverconn.TLSBindHeaderKey: sigHex,
		},
	}

	peerInfo, ok := extractTLSPeer(tlsPeerCtx(t, leafCert))
	require.True(t, ok)

	// Soft mode: bad signature is always rejected. Only an
	// outright-missing header gets the soft-accept treatment.
	srv := newTLSBindingTestServer(t, false)
	err = srv.verifyTLSBindingSig(
		t.Context(), env, aliceKey.PubKey(), peerInfo,
	)
	require.Error(t, err)
}

// TestVerifyTLSBindingSigWrongLeaf rejects signatures whose covered
// SPKI does not match the leaf actually observed on the wire — a
// stricter variant of the replay test where the attacker may even
// learn the victim's TLS leaf SPKI but cannot make it match a
// different connection's observed leaf.
func TestVerifyTLSBindingSigWrongLeaf(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	wrongLeaf := makeTestCert(t, "alice")
	observedLeaf := makeTestCert(t, "alice")

	// Sign over a leaf SPKI other than the one observed.
	sigHex := signTLSBind(t, clientKey, wrongLeaf.RawSubjectPublicKeyInfo)

	env := &mailboxpb.Envelope{
		Sender: "alice",
		Headers: map[string]string{
			serverconn.TLSBindHeaderKey: sigHex,
		},
	}

	peerInfo, ok := extractTLSPeer(tlsPeerCtx(t, observedLeaf))
	require.True(t, ok)

	srv := newTLSBindingTestServer(t, false)
	err = srv.verifyTLSBindingSig(
		t.Context(), env, clientKey.PubKey(), peerInfo,
	)
	require.Error(t, err)
}

// TestVerifyTLSBindingSigMalformedHeader rejects garbage header
// bytes regardless of rollout mode. A legacy client would omit the
// header entirely — only an attack or a broken client emits
// non-hex / unparseable bytes.
func TestVerifyTLSBindingSigMalformedHeader(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	leafCert := makeTestCert(t, "alice")

	cases := []struct {
		name string
		raw  string
	}{
		{
			"non hex",
			"not-hex!!",
		},
		// 64 hex chars (32 bytes) — wrong length for schnorr.
		{
			"wrong length",
			"00000000000000000000000000000000" +
				"00000000000000000000000000000000",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := &mailboxpb.Envelope{
				Sender: "alice",
				Headers: map[string]string{
					serverconn.TLSBindHeaderKey: tc.raw,
				},
			}

			peerInfo, ok := extractTLSPeer(tlsPeerCtx(t, leafCert))
			require.True(t, ok)

			srv := newTLSBindingTestServer(t, false)
			err := srv.verifyTLSBindingSig(
				t.Context(), env, clientKey.PubKey(), peerInfo,
			)
			require.Error(t, err)
		})
	}
}
