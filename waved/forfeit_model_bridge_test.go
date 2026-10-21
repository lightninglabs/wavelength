package waved

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	forfeitbridge "github.com/lightninglabs/wavelength/p-models/forfeitsigning/bridge"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestForfeitSigningBrokerModelTrace replays the model's answer and replay
// sequence against the production connector-bound signature broker.
func TestForfeitSigningBrokerModelTrace(t *testing.T) {
	t.Parallel()

	trace, err := forfeitbridge.ParseTrace(
		filepath.Join(
			"..", "p-models", "forfeitsigning", "traces",
			"broker_valid_replay.json",
		),
	)
	require.NoError(t, err)

	broker := newForfeitSignatureBroker()
	req, paymentHash, signerPrivs :=
		testForfeitParticipantSignRequestWithSigners(t)
	pending, err := pendingForfeitSignatureRequest(
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       pendingForfeitSigningRoute(),
		}, req,
	)
	require.NoError(t, err)

	requestID := string(pending.GetRequestId())
	broker.requests[requestID] = &forfeitSignatureRequest{
		proto:   pending,
		signReq: req,
	}
	broker.order = []string{requestID}

	first := testDaemonForfeitParticipantSignatureForRequest(
		t, req, signerPrivs[1],
	)
	alternate := testDaemonForfeitParticipantSignatureForRequest(
		t, req, signerPrivs[1],
		schnorr.CustomNonce(
			[32]byte{0x42},
		),
	)
	require.NotEqual(t, first.GetSignature(), alternate.GetSignature())

	invalid := &waverpc.ForfeitParticipantSignature{
		Pubkey:    bytes.Clone(alternate.GetPubkey()),
		Signature: bytes.Clone(alternate.GetSignature()),
	}
	invalid.Signature[len(invalid.Signature)-1] ^= 0x01
	wrongKey := testDaemonForfeitParticipantSignatureForRequest(
		t, req, signerPrivs[0],
	)

	for stepIndex, step := range trace.Steps {
		if step.Op != "submit" {
			t.Fatalf("step %d: unknown operation %q", stepIndex,
				step.Op)
		}

		var signature *waverpc.ForfeitParticipantSignature
		switch step.Signature {
		case "first_valid":
			signature = first

		case "alternate_valid":
			signature = alternate

		case "invalid":
			signature = invalid

		case "wrong_key":
			signature = wrongKey

		default:
			t.Fatalf("step %d: unknown signature %q", stepIndex,
				step.Signature)
		}

		err := broker.submit(
			pending.GetRequestId(),
			[]*waverpc.ForfeitParticipantSignature{signature},
		)
		switch step.Expect {
		case "accepted_first", "accepted_replay":
			require.NoError(t, err, "step %d", stepIndex)

		case "rejected_invalid":
			require.Equal(
				t, codes.InvalidArgument, status.Code(err),
				"step %d", stepIndex,
			)

		default:
			t.Fatalf("step %d: unknown expectation %q", stepIndex,
				step.Expect)
		}
	}

	broker.mu.Lock()
	retained := broker.requests[requestID].signatures
	broker.mu.Unlock()
	require.Len(t, retained, 1)
	require.Equal(
		t, first.GetSignature(), retained[0].Signature.Serialize(),
	)
	require.True(
		t,
		sameSubmittedParticipantSet(
			retained, []*waverpc.ForfeitParticipantSignature{first},
		),
	)
}
