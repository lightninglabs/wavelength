package swaps

import (
	"bytes"
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	forfeitbridge "github.com/lightninglabs/wavelength/p-models/forfeitsigning/bridge"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestForfeitSigningSessionModelTrace replays the model's concrete authority
// lifecycle against the production receive-session gate and mailbox responder.
func TestForfeitSigningSessionModelTrace(t *testing.T) {
	t.Parallel()

	trace, err := forfeitbridge.ParseTrace(
		filepath.Join(
			"..", "..", "p-models", "forfeitsigning", "traces",
			"session_authority_replay.json",
		),
	)
	require.NoError(t, err)

	paymentHash := lntypes.Hash{0x31, 0x32, 0x33}
	payload := testReceiveForfeitSignaturePayload(t, paymentHash)
	daemonConn := &testDaemonConn{
		signForfeitResp: &waverpc.SignVTXOForfeitResponse{
			Pubkey:    []byte("participant-pubkey"),
			Signature: []byte("participant-signature"),
		},
	}
	serverConn := &testSwapServerConn{}
	client := NewSwapClientWithStore(
		serverConn, daemonConn, nil, nil, newTestSwapStore(t),
	)
	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	session := &ReceiveSession{
		client:              client,
		state:               ReceiveStateHTLCEventAccepted,
		PaymentHash:         paymentHash,
		clientPubKey:        clientPriv.PubKey(),
		operatorPubKey:      operatorPriv.PubKey(),
		vhtlcPkScript:       bytes.Clone(payload.VHTLCPkScript),
		vhtlcPolicyTemplate: bytes.Clone(payload.VHTLCPolicyTemplate),
		forfeitBindingGate:  newReceiveForfeitBindingGate(),
	}
	fundedOutpoint := payload.VHTLCOutpoint
	fundedAmount := payload.VHTLCAmountSat

	for stepIndex, step := range trace.Steps {
		switch step.Op {
		case "publish_failed":
			require.True(
				t, step.Durable, "step %d: failed "+
					"publication must exercise the "+
					"durable boundary", stepIndex,
			)
			session.payerFeeMsat = math.MaxInt64 + 1
			err := session.mutateAndPersist(
				t.Context(),
				func() error {
					session.vhtlcOutpoint = fundedOutpoint
					session.vhtlcAmount = fundedAmount

					return session.transition(
						receiveEventVHTLCFunded,
					)
				},
			)
			require.ErrorContains(
				t, err, "overflows int64", "step %d", stepIndex,
			)

		case "publish":
			require.True(
				t, step.Durable, "step %d: production "+
					"restart trace must persist", stepIndex,
			)
			session.payerFeeMsat = 0
			err := session.mutateAndPersist(
				t.Context(),
				func() error {
					session.vhtlcOutpoint = fundedOutpoint
					session.vhtlcAmount = fundedAmount

					return session.transition(
						receiveEventVHTLCFunded,
					)
				},
			)
			require.NoError(t, err, "step %d", stepIndex)

		case "restart":
			session, err = client.ResumeReceiveViaLightning(
				t.Context(), paymentHash,
			)
			require.NoError(t, err, "step %d", stepIndex)

		case "deliver":
			replayForfeitSigningSessionDelivery(
				t, stepIndex, session, payload, daemonConn,
				serverConn, step,
			)

		default:
			t.Fatalf("step %d: unknown operation %q", stepIndex,
				step.Op)
		}
	}
}

// replayForfeitSigningSessionDelivery applies one model delivery to the real
// responder and checks whether signing, submission, and acknowledgement cross
// the expected boundaries.
func replayForfeitSigningSessionDelivery(t *testing.T, stepIndex int,
	session *ReceiveSession, basePayload *ForfeitSignaturePayload,
	daemonConn *testDaemonConn, serverConn *testSwapServerConn,
	step forfeitbridge.Step) {

	t.Helper()

	payload := *basePayload
	payload.VHTLCPkScript = bytes.Clone(basePayload.VHTLCPkScript)
	payload.VHTLCPolicyTemplate = bytes.Clone(
		basePayload.VHTLCPolicyTemplate,
	)
	switch step.Signature {
	case "first_valid", "alternate_valid":
		daemonConn.signForfeitResp.Signature = []byte(step.Signature)

	default:
		t.Fatalf("step %d: unknown signature %q", stepIndex,
			step.Signature)
	}

	switch step.Identity {
	case "exact":
	case "wrong_payment_hash":
		payload.PaymentHash[0]++

	case "wrong_outpoint":
		payload.VHTLCOutpoint = "different:0"

	case "wrong_amount":
		payload.VHTLCAmountSat++

	case "wrong_script":
		payload.VHTLCPkScript[0] ^= 0x01

	case "wrong_policy":
		payload.VHTLCPolicyTemplate[0] ^= 0x01

	case "wrong_policy_hash":
		applyPolicyHashDrift(t, session, &payload)

	default:
		t.Fatalf("step %d: unknown identity %q", stepIndex,
			step.Identity)
	}

	beforeSign := daemonConn.signForfeitCalls
	beforeSubmit := serverConn.submitForfeitCalls
	ackCalls := 0
	ackFailure := errors.New("model ACK failure")
	if step.Ack != "success" && step.Ack != "failure" {
		t.Fatalf("step %d: unknown ACK result %q", stepIndex, step.Ack)
	}
	notification := &OutSwapForfeitSignatureNotification{
		Payload: &payload,
		Ack: func(context.Context) error {
			ackCalls++
			if step.Ack == "failure" {
				return ackFailure
			}

			return nil
		},
	}

	responder := testReceiveForfeitResponder(t, session)
	err := responder.handleOutSwapForfeitSignatureRequest(
		t.Context(), notification,
	)

	switch step.Expect {
	case "rejected_unacked":
		require.Error(t, err, "step %d", stepIndex)
		require.Equal(t, beforeSign, daemonConn.signForfeitCalls)
		require.Equal(t, beforeSubmit, serverConn.submitForfeitCalls)
		require.Zero(t, ackCalls)

	case "accepted_unacked":
		require.ErrorIs(t, err, ackFailure, "step %d", stepIndex)
		require.Equal(t, beforeSign+1, daemonConn.signForfeitCalls)
		require.Equal(t, beforeSubmit+1, serverConn.submitForfeitCalls)
		require.Equal(t, 1, ackCalls)
		require.Equal(
			t, []byte(step.Signature),
			serverConn.lastSubmitForfeitSig.Signature,
		)

	case "accepted_acked":
		require.NoError(t, err, "step %d", stepIndex)
		require.Equal(t, beforeSign+1, daemonConn.signForfeitCalls)
		require.Equal(t, beforeSubmit+1, serverConn.submitForfeitCalls)
		require.Equal(t, 1, ackCalls)
		require.Equal(
			t, []byte(step.Signature),
			serverConn.lastSubmitForfeitSig.Signature,
		)

	default:
		t.Fatalf("step %d: unknown expectation %q", stepIndex,
			step.Expect)
	}
}

// applyPolicyHashDrift keeps the request and published policy bytes equal while
// making their embedded payment hash disagree with the session payment hash.
func applyPolicyHashDrift(t *testing.T, session *ReceiveSession,
	payload *ForfeitSignaturePayload) {

	t.Helper()

	differentHash := lntypes.Hash{0xaa, 0xbb, 0xcc}
	differentPayload := testReceiveForfeitSignaturePayload(t, differentHash)
	session.vhtlcPolicyTemplate = bytes.Clone(
		differentPayload.VHTLCPolicyTemplate,
	)
	payload.VHTLCPolicyTemplate = bytes.Clone(
		differentPayload.VHTLCPolicyTemplate,
	)
	session.publishReceiveForfeitBinding()
}
