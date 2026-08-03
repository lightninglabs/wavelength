package waved

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

func treeBrokerTestKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return priv.PubKey()
}

// treeBrokerTestRequest builds a nonce-round session request for the broker.
func treeBrokerTestRequest(t *testing.T) round.TreeSigningSessionRequest {
	t.Helper()

	cosigner := treeBrokerTestKey(t)
	operator := treeBrokerTestKey(t)

	var sessionID [32]byte
	sessionID[0] = 0x09

	return round.TreeSigningSessionRequest{
		RoundID:     round.RoundID(uuid.New()),
		CosignerKey: cosigner,
		SessionID:   sessionID,
		Cosigners: []*btcec.PublicKey{
			cosigner,
			operator,
		},
		SweepTapscriptRoot: []byte{
			0x01,
			0x02,
			0x03,
		},
	}
}

// serializedPartialSig returns valid partial-signature bytes for submission.
func serializedPartialSig(t *testing.T, v uint32) []byte {
	t.Helper()

	sig := &musig2.PartialSignature{S: new(btcec.ModNScalar)}
	sig.S.SetInt(v)

	var buf bytes.Buffer
	require.NoError(t, sig.Encode(&buf))

	return buf.Bytes()
}

// TestTreeSignatureBrokerNonceRoundTrip drives the broker exactly as the round
// FSM's proxy signer does: FetchTreeNonce blocks, the external party sees the
// request via list, submits a nonce, and the blocked fetch returns it.
func TestTreeSignatureBrokerNonceRoundTrip(t *testing.T) {
	t.Parallel()

	b := newTreeSignatureBroker()
	req := treeBrokerTestRequest(t)

	var wantNonce tree.Musig2PubNonce
	wantNonce[0] = 0x5a
	wantNonce[1] = 0xa5

	type result struct {
		nonce tree.Musig2PubNonce
		err   error
	}
	done := make(chan result, 1)
	go func() {
		nonce, err := b.FetchTreeNonce(context.Background(), req)
		done <- result{nonce: nonce, err: err}
	}()

	// The external party polls until the request appears, verifies the
	// transcript, and submits the nonce.
	var pending *waverpc.PendingTreeSigningRequest
	require.Eventually(t, func() bool {
		reqs, _ := b.list(0, 0)
		if len(reqs) != 1 {
			return false
		}
		pending = reqs[0]

		return true
	}, 2*time.Second, 5*time.Millisecond)

	require.Equal(
		t, waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
		pending.GetRound(),
	)
	require.Equal(
		t, req.CosignerKey.SerializeCompressed(),
		pending.GetCosignerPubkey(),
	)
	require.Equal(t, req.SessionID[:], pending.GetSessionId())
	require.Len(t, pending.GetCosigners(), 2)
	require.Equal(
		t, req.SweepTapscriptRoot, pending.GetSweepTapscriptRoot(),
	)

	require.NoError(
		t,
		b.submit(
			pending.GetRequestId(),
			waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
			wantNonce[:], nil,
		),
	)

	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, wantNonce, got.nonce)

	// The answered request is no longer listed.
	reqs, _ := b.list(0, 0)
	require.Empty(t, reqs)
}

// TestTreeSignatureBrokerPartialSigRoundTrip drives the round-two path: the
// partial-sig request carries the sighash and aggregate nonce, and a submitted
// partial signature unblocks the fetch.
func TestTreeSignatureBrokerPartialSigRoundTrip(t *testing.T) {
	t.Parallel()

	b := newTreeSignatureBroker()
	req := treeBrokerTestRequest(t)
	req.SigHash[0] = 0x77
	req.AggNonce[0] = 0xcd

	wantSigBytes := serializedPartialSig(t, 99)

	type result struct {
		sig *musig2.PartialSignature
		err error
	}
	done := make(chan result, 1)
	go func() {
		sig, err := b.FetchTreePartialSig(context.Background(), req)
		done <- result{sig: sig, err: err}
	}()

	var pending *waverpc.PendingTreeSigningRequest
	require.Eventually(t, func() bool {
		reqs, _ := b.list(0, 0)
		if len(reqs) != 1 {
			return false
		}
		pending = reqs[0]

		return true
	}, 2*time.Second, 5*time.Millisecond)

	require.Equal(
		t, waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG,
		pending.GetRound(),
	)
	require.Equal(t, req.SigHash[:], pending.GetSighash())
	require.Equal(t, req.AggNonce[:], pending.GetAggregateNonce())

	require.NoError(
		t,
		b.submit(
			pending.GetRequestId(),
			waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG,
			nil, wantSigBytes,
		),
	)

	got := <-done
	require.NoError(t, got.err)
	require.NotNil(t, got.sig)

	var gotBuf bytes.Buffer
	require.NoError(t, got.sig.Encode(&gotBuf))
	require.Equal(t, wantSigBytes, gotBuf.Bytes())
}

// TestTreeSignatureBrokerDistinctRoundIDs verifies the nonce and partial-sig
// requests for one session hash to distinct request ids, so the two rounds do
// not collide in the broker.
func TestTreeSignatureBrokerDistinctRoundIDs(t *testing.T) {
	t.Parallel()

	req := treeBrokerTestRequest(t)

	nonceReq, err := pendingTreeSigningRequest(
		req, waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
	)
	require.NoError(t, err)

	req.SigHash[0] = 0x01
	req.AggNonce[0] = 0x02
	partialReq, err := pendingTreeSigningRequest(
		req, waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG,
	)
	require.NoError(t, err)

	require.NotEqual(t, nonceReq.GetRequestId(), partialReq.GetRequestId())
}

// TestTreeSignatureBrokerSubmitErrors covers the submit rejection paths.
func TestTreeSignatureBrokerSubmitErrors(t *testing.T) {
	t.Parallel()

	b := newTreeSignatureBroker()

	// Unknown request id.
	err := b.submit(
		[]byte{0xde, 0xad},
		waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
		make([]byte, musig2.PubNonceSize), nil,
	)
	require.ErrorContains(t, err, "not found")

	// Empty request id.
	err = b.submit(
		nil, waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
		make([]byte, musig2.PubNonceSize), nil,
	)
	require.ErrorContains(t, err, "request_id is required")

	// Park a nonce request, then submit with the wrong round and a bad
	// nonce length.
	req := treeBrokerTestRequest(t)
	go func() {
		_, _ = b.FetchTreeNonce(context.Background(), req)
	}()

	var pending *waverpc.PendingTreeSigningRequest
	require.Eventually(t, func() bool {
		reqs, _ := b.list(0, 0)
		if len(reqs) != 1 {
			return false
		}
		pending = reqs[0]

		return true
	}, 2*time.Second, 5*time.Millisecond)

	err = b.submit(
		pending.GetRequestId(),
		waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG, nil,
		serializedPartialSig(t, 1),
	)
	require.ErrorContains(t, err, "round mismatch")

	err = b.submit(
		pending.GetRequestId(),
		waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE, []byte{0x00},
		nil,
	)
	require.ErrorContains(t, err, "public_nonce must be")
}

// TestTreeSignatureBrokerTimeout verifies a fetch fails when the external party
// never answers within the wait window.
func TestTreeSignatureBrokerTimeout(t *testing.T) {
	t.Parallel()

	b := newTreeSignatureBroker()
	b.waitTimeout = 50 * time.Millisecond

	_, err := b.FetchTreeNonce(
		context.Background(), treeBrokerTestRequest(t),
	)
	require.Error(t, err)
}

// TestTreeSignatureBrokerIdempotentSubmit verifies resubmitting identical
// material is accepted while conflicting material is rejected.
func TestTreeSignatureBrokerIdempotentSubmit(t *testing.T) {
	t.Parallel()

	b := newTreeSignatureBroker()
	req := treeBrokerTestRequest(t)

	go func() {
		_, _ = b.FetchTreeNonce(context.Background(), req)
	}()

	var pending *waverpc.PendingTreeSigningRequest
	require.Eventually(t, func() bool {
		reqs, _ := b.list(0, 0)
		if len(reqs) != 1 {
			return false
		}
		pending = reqs[0]

		return true
	}, 2*time.Second, 5*time.Millisecond)

	nonce := make([]byte, musig2.PubNonceSize)
	nonce[0] = 0x11
	require.NoError(
		t,
		b.submit(
			pending.GetRequestId(),
			waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
			nonce, nil,
		),
	)

	// Identical resubmit is accepted.
	require.NoError(
		t,
		b.submit(
			pending.GetRequestId(),
			waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
			nonce, nil,
		),
	)

	// Conflicting resubmit is rejected.
	other := make([]byte, musig2.PubNonceSize)
	other[0] = 0x22
	err := b.submit(
		pending.GetRequestId(),
		waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE, other, nil,
	)
	require.ErrorContains(t, err, "already answered")
}
