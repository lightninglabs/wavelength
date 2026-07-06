package darepod

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	arktx "github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func localForfeitSigningRoute() daemonrpc.ForfeitSigningRoute {
	route := daemonrpc.
		ForfeitSigningRoute_FORFEIT_SIGNING_ROUTE_LOCAL_SIGNER

	return route
}

func pendingForfeitSigningRoute() daemonrpc.ForfeitSigningRoute {
	route := daemonrpc.
		ForfeitSigningRoute_FORFEIT_SIGNING_ROUTE_PENDING_REQUEST

	return route
}

// TestForfeitSignatureBrokerSurfacesAndCompletesRequest verifies the daemon
// bridge between the VTXO actor's connector-bound signing callback and the
// externally visible pending-request RPC shape. A custom refresh cannot know
// the exact forfeit transaction until the round assigns a connector, so the
// callback must block, publish the full transcript, and resume only after the
// external protocol submits the remote participant signature.
func TestForfeitSignatureBrokerSurfacesAndCompletesRequest(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash, signerPrivs :=
		testForfeitParticipantSignRequestWithSigners(t)
	unregister := broker.registerContext(req.VTXO.Outpoint.String(),
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       pendingForfeitSigningRoute(),
		},
	)
	defer unregister()

	signCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type signResult struct {
		sigs []*types.ForfeitParticipantSig
		err  error
	}
	results := make(chan signResult, 1)
	go func() {
		sigs, err := broker.sign(signCtx, req)
		results <- signResult{sigs: sigs, err: err}
	}()

	var pending *daemonrpc.PendingForfeitParticipantSignatureRequest
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		requests, next := broker.list(0, 10)
		require.Len(c, requests, 1)
		require.Equal(c, requests[0].GetSequence(), next)
		pending = requests[0]
	}, time.Second, 10*time.Millisecond)

	require.Equal(t, paymentHash[:], pending.GetPaymentHash())
	require.Equal(t, req.VTXO.Outpoint.String(), pending.GetVtxoOutpoint())
	require.EqualValues(t, req.VTXO.Amount, pending.GetVtxoAmountSat())
	require.Equal(t, req.VTXO.PkScript, pending.GetVtxoPkScript())
	require.Equal(
		t, req.VTXO.PolicyTemplate, pending.GetVtxoPolicyTemplate(),
	)
	require.NotEmpty(t, pending.GetRequestId())
	require.NotEmpty(t, pending.GetUnsignedForfeitTx())
	require.Equal(
		t, req.ConnectorOutpoint.String(),
		pending.GetConnectorOutpoint(),
	)

	participantSigs := testDaemonForfeitParticipantSignaturesForRequest(
		t, req, signerPrivs[1:]...,
	)
	err := broker.submit(pending.GetRequestId(), participantSigs)
	require.NoError(t, err)

	select {
	case result := <-results:
		require.NoError(t, result.err)
		require.Len(t, result.sigs, 1)
		require.True(
			t, sameSubmittedParticipantSet(
				result.sigs, participantSigs,
			),
		)

	case <-time.After(time.Second):
		t.Fatal("signer callback did not receive submitted signature")
	}

	broker.mu.Lock()
	_, ok := broker.contexts[req.VTXO.Outpoint.String()]
	broker.mu.Unlock()
	require.False(t, ok)
}

// TestForfeitSignatureBrokerDeleteContextsClearsOutpoints verifies that round
// rollback can remove queued custom-refresh signing metadata before any
// connector-bound signing request is created.
func TestForfeitSignatureBrokerDeleteContextsClearsOutpoints(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash, _ := testForfeitParticipantSignRequestWithSigners(t)
	keep := wire.OutPoint{
		Hash:  req.VTXO.Outpoint.Hash,
		Index: req.VTXO.Outpoint.Index + 1,
	}

	broker.registerContext(req.VTXO.Outpoint.String(),
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       pendingForfeitSigningRoute(),
		},
	)
	broker.registerContext(keep.String(), forfeitSigningContext{
		paymentHash: paymentHash[:],
		route:       pendingForfeitSigningRoute(),
	})

	broker.deleteContexts([]wire.OutPoint{req.VTXO.Outpoint})

	broker.mu.Lock()
	_, dropped := broker.contexts[req.VTXO.Outpoint.String()]
	_, retained := broker.contexts[keep.String()]
	broker.mu.Unlock()

	require.False(t, dropped)
	require.True(t, retained)
}

// TestForfeitSignatureBrokerDelegatesInSwapRequests verifies the daemon's
// custom-refresh broker can answer local-signer requests synchronously while
// preserving the broker callback as the VTXO-manager hook.
func TestForfeitSignatureBrokerDelegatesLocalSignerRequests(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash := testForfeitParticipantSignRequest(t)
	unregister := broker.registerContext(req.VTXO.Outpoint.String(),
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       localForfeitSigningRoute(),
		},
	)
	defer unregister()

	expected := []*types.ForfeitParticipantSig{{}}
	calls := 0
	broker.setLocalSigner(func(_ context.Context,
		gotReq *vtxo.ForfeitParticipantSignRequest) (
		[]*types.ForfeitParticipantSig, error) {

		calls++
		require.Same(t, req, gotReq)

		return expected, nil
	})

	signatures, err := broker.sign(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Same(t, expected[0], signatures[0])
	require.Empty(t, broker.requests)

	broker.mu.Lock()
	_, ok := broker.contexts[req.VTXO.Outpoint.String()]
	broker.mu.Unlock()
	require.False(t, ok)
}

// TestForfeitSignatureBrokerRejectsMissingLocalSigner verifies that a request
// explicitly routed to the daemon-local signer fails closed when no local
// signer is installed. Without this guard, the daemon could accidentally turn
// a locally owned participant-signature request into a pending external
// request that no peer is expected to answer.
func TestForfeitSignatureBrokerRejectsMissingLocalSigner(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash := testForfeitParticipantSignRequest(t)
	unregister := broker.registerContext(req.VTXO.Outpoint.String(),
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       localForfeitSigningRoute(),
		},
	)
	defer unregister()

	signatures, err := broker.sign(t.Context(), req)
	require.ErrorContains(t, err, "local forfeit participant signer")
	require.Nil(t, signatures)
	require.Empty(t, broker.requests)
}

// TestForfeitSignatureBrokerRemovesCancelledWaiter verifies that a cancelled
// external-signature request does not leave a dead waiter channel retained in
// the broker. The pending request itself remains listable so the coordinator
// can still observe and complete it for a later retry.
func TestForfeitSignatureBrokerRemovesCancelledWaiter(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash := testForfeitParticipantSignRequest(t)
	unregister := broker.registerContext(req.VTXO.Outpoint.String(),
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       pendingForfeitSigningRoute(),
		},
	)
	defer unregister()

	signCtx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 1)
	go func() {
		_, err := broker.sign(signCtx, req)
		results <- err
	}()

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		broker.mu.Lock()
		defer broker.mu.Unlock()

		require.Len(c, broker.requests, 1)
		for _, pending := range broker.requests {
			require.Len(c, pending.waiters, 1)
		}
	}, time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-results:
		require.ErrorIs(t, err, context.Canceled)

	case <-time.After(time.Second):
		t.Fatal("signer callback did not observe cancellation")
	}

	broker.mu.Lock()
	defer broker.mu.Unlock()

	require.Len(t, broker.requests, 1)
	for _, pending := range broker.requests {
		require.Empty(t, pending.waiters)
	}
}

// TestForfeitSignatureBrokerTimesOutPendingRequest verifies an unanswered
// external-signature request cannot block the VTXO actor indefinitely. The
// broker removes the dead waiter and drops the outpoint correlation, leaving
// the swap-level coordinator to fall back to recovery once its refresh marker
// goes stale.
func TestForfeitSignatureBrokerTimesOutPendingRequest(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	broker.waitTimeout = 10 * time.Millisecond
	req, paymentHash := testForfeitParticipantSignRequest(t)
	unregister := broker.registerContext(req.VTXO.Outpoint.String(),
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       pendingForfeitSigningRoute(),
		},
	)
	defer unregister()

	signatures, err := broker.sign(t.Context(), req)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, signatures)

	broker.mu.Lock()
	defer broker.mu.Unlock()

	require.NotEmpty(t, broker.requests)
	for _, pending := range broker.requests {
		require.Empty(t, pending.waiters)
	}
	_, ok := broker.contexts[req.VTXO.Outpoint.String()]
	require.False(t, ok)
}

// TestForfeitSignatureBrokerClearsContextOnTranscriptError verifies a
// malformed VTXO callback cannot leak its temporary outpoint correlation. This
// keeps later callbacks from being routed through stale swap metadata after the
// exact connector-backed transcript failed to build.
func TestForfeitSignatureBrokerClearsContextOnTranscriptError(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash := testForfeitParticipantSignRequest(t)
	req.SpendPath = nil
	unregister := broker.registerContext(req.VTXO.Outpoint.String(),
		forfeitSigningContext{
			paymentHash: paymentHash[:],
			route:       pendingForfeitSigningRoute(),
		},
	)
	defer unregister()

	signatures, err := broker.sign(t.Context(), req)
	require.ErrorContains(t, err, "spend path is required")
	require.Nil(t, signatures)

	broker.mu.Lock()
	defer broker.mu.Unlock()

	_, ok := broker.contexts[req.VTXO.Outpoint.String()]
	require.False(t, ok)
}

// TestForfeitSignatureBrokerListDoesNotAdvancePastEarlierPendingRequest
// verifies answered requests do not move the polling cursor past an earlier
// unanswered request. Callers use next_sequence as their next after_sequence;
// if the broker advanced over answered later entries, an earlier pending
// transcript could become invisible forever.
func TestForfeitSignatureBrokerListDoesNotAdvancePastEarlierPendingRequest(
	t *testing.T) {

	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash := testForfeitParticipantSignRequest(t)
	correlation := forfeitSigningContext{
		paymentHash: paymentHash[:],
		route:       pendingForfeitSigningRoute(),
	}

	first, err := pendingForfeitSignatureRequest(correlation, req)
	require.NoError(t, err)
	first.Sequence = 1

	secondReq := *req
	secondOutpoint := testWalletOpsOutpoint(32)
	secondReq.VTXO = &vtxo.Descriptor{}
	*secondReq.VTXO = *req.VTXO
	secondReq.VTXO.Outpoint = secondOutpoint

	second, err := pendingForfeitSignatureRequest(
		correlation, &secondReq,
	)
	require.NoError(t, err)
	second.Sequence = 2

	firstID := string(first.GetRequestId())
	secondID := string(second.GetRequestId())
	broker.requests[firstID] = &forfeitSignatureRequest{proto: first}
	broker.requests[secondID] = &forfeitSignatureRequest{
		proto:    second,
		answered: true,
		signatures: []*types.ForfeitParticipantSig{{
			PubKey: req.VTXO.ClientKey.PubKey,
		}},
	}
	broker.order = []string{firstID, secondID}

	requests, next := broker.list(0, 10)
	require.Len(t, requests, 1)
	require.Equal(t, first.GetRequestId(), requests[0].GetRequestId())
	require.Equal(t, first.GetSequence(), next)
}

// TestForfeitSignatureBrokerPrunesOldAnsweredRequests verifies answered
// requests keep a bounded idempotency window without retaining all historical
// connector transcripts in long-lived daemons.
func TestForfeitSignatureBrokerPrunesOldAnsweredRequests(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()

	for idx := 0; idx < defaultAnsweredForfeitRequestLimit+2; idx++ {
		id := fmt.Sprintf("answered-%03d", idx)
		broker.requests[id] = &forfeitSignatureRequest{
			proto: &daemonrpc.
				PendingForfeitParticipantSignatureRequest{
				RequestId: []byte(id),
				Sequence:  uint64(idx + 1),
			},
			answered: true,
		}
		broker.order = append(broker.order, id)
	}

	broker.mu.Lock()
	broker.pruneAnsweredRequestsLocked()
	broker.mu.Unlock()

	require.Len(t, broker.order, defaultAnsweredForfeitRequestLimit)
	require.NotContains(t, broker.requests, "answered-000")
	require.NotContains(t, broker.requests, "answered-001")
	require.Contains(
		t, broker.requests, fmt.Sprintf("answered-%03d",
			defaultAnsweredForfeitRequestLimit+1),
	)
}

// TestForfeitSignatureBrokerSubmitAllowsNoExternalParticipants verifies a
// pending request can be answered with no external signatures when the selected
// spend path only requires the local VTXO actor and the operator.
func TestForfeitSignatureBrokerSubmitAllowsNoExternalParticipants(
	t *testing.T) {

	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash := testLocalOnlyForfeitParticipantSignRequest(t)
	correlation := forfeitSigningContext{
		paymentHash: paymentHash[:],
		route:       pendingForfeitSigningRoute(),
	}
	pending, err := pendingForfeitSignatureRequest(correlation, req)
	require.NoError(t, err)

	requestID := string(pending.GetRequestId())
	broker.requests[requestID] = &forfeitSignatureRequest{
		proto:   pending,
		signReq: req,
	}
	broker.order = []string{requestID}

	err = broker.submit(pending.GetRequestId(), nil)
	require.NoError(t, err)

	err = broker.submit(pending.GetRequestId(), nil)
	require.NoError(t, err)

	requests, _ := broker.list(0, 10)
	require.Empty(t, requests)

	broker.mu.Lock()
	require.True(t, broker.requests[requestID].answered)
	require.Empty(t, broker.requests[requestID].signatures)
	broker.mu.Unlock()
}

// TestForfeitSignatureRequestIDIncludesConnectorAmount verifies the pending
// request id commits to the assigned connector value. The connector amount is
// part of the exact forfeit transaction's prevout value, so two otherwise
// identical requests with different connector amounts must not share a wake-up
// id.
func TestForfeitSignatureRequestIDIncludesConnectorAmount(t *testing.T) {
	t.Parallel()

	req, paymentHash := testForfeitParticipantSignRequest(t)
	correlation := forfeitSigningContext{
		paymentHash: paymentHash[:],
		route:       pendingForfeitSigningRoute(),
	}

	first, err := pendingForfeitSignatureRequest(correlation, req)
	require.NoError(t, err)

	req.ConnectorAmount++
	second, err := pendingForfeitSignatureRequest(correlation, req)
	require.NoError(t, err)

	require.NotEqual(t, first.GetRequestId(), second.GetRequestId())
}

// TestForfeitSignatureBrokerSubmitRejectsUnknownRequest verifies signatures
// cannot be submitted for an arbitrary request id. The swap protocol above the
// daemon may retry submissions, but it must first observe a pending request
// emitted by the connector-bound VTXO callback.
func TestForfeitSignatureBrokerSubmitRejectsUnknownRequest(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	signer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sig, err := schnorr.Sign(signer, bytes.Repeat([]byte{0x44}, 32))
	require.NoError(t, err)

	err = broker.submit([]byte("missing"),
		[]*daemonrpc.ForfeitParticipantSignature{{
			Pubkey:    signer.PubKey().SerializeCompressed(),
			Signature: sig.Serialize(),
		}},
	)
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestForfeitSignatureBrokerSubmitRejectsInvalidSignature verifies a
// parseable signature must still validate against the pending forfeit
// transcript before the broker accepts it.
func TestForfeitSignatureBrokerSubmitRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash, signerPrivs :=
		testForfeitParticipantSignRequestWithSigners(t)
	correlation := forfeitSigningContext{
		paymentHash: paymentHash[:],
		route:       pendingForfeitSigningRoute(),
	}
	pending, err := pendingForfeitSignatureRequest(correlation, req)
	require.NoError(t, err)

	requestID := string(pending.GetRequestId())
	broker.requests[requestID] = &forfeitSignatureRequest{
		proto:   pending,
		signReq: req,
	}
	broker.order = []string{requestID}

	invalid := testDaemonForfeitParticipantSignaturesForRequest(
		t, req, signerPrivs[1:]...,
	)
	invalid[0].Signature[len(invalid[0].Signature)-1] ^= 0x01

	err = broker.submit(pending.GetRequestId(), invalid)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(
		t, status.Convert(err).Message(),
		"invalid participant signature",
	)

	broker.mu.Lock()
	require.Empty(t, broker.requests[requestID].signatures)
	broker.mu.Unlock()
}

// TestForfeitSignatureBrokerSubmitRequiresRemoteParticipant verifies the
// broker only waits for signatures that the local VTXO actor cannot produce on
// its own. The actor signs req.VTXO.ClientKey locally before it calls the
// broker, so submitting that local signature through the RPC must not satisfy a
// custom VHTLC path that still needs the remote participant.
func TestForfeitSignatureBrokerSubmitRequiresRemoteParticipant(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash, signerPrivs :=
		testForfeitParticipantSignRequestWithSigners(t)
	correlation := forfeitSigningContext{
		paymentHash: paymentHash[:],
		route:       pendingForfeitSigningRoute(),
	}
	pending, err := pendingForfeitSignatureRequest(correlation, req)
	require.NoError(t, err)

	requestID := string(pending.GetRequestId())
	broker.requests[requestID] = &forfeitSignatureRequest{
		proto:   pending,
		signReq: req,
	}
	broker.order = []string{requestID}

	localSig := testDaemonForfeitParticipantSignaturesForRequest(
		t, req, signerPrivs[0],
	)
	err = broker.submit(pending.GetRequestId(), localSig)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(
		t, status.Convert(err).Message(),
		"unexpected participant key",
	)

	broker.mu.Lock()
	require.Empty(t, broker.requests[requestID].signatures)
	broker.mu.Unlock()
}

// TestForfeitSignatureBrokerSubmitIsIdempotent verifies retrying an already
// answered request with the same participant signature is idempotent.
func TestForfeitSignatureBrokerSubmitIsIdempotent(t *testing.T) {
	t.Parallel()

	broker := newForfeitSignatureBroker()
	req, paymentHash, signerPrivs :=
		testForfeitParticipantSignRequestWithSigners(t)
	correlation := forfeitSigningContext{
		paymentHash: paymentHash[:],
		route:       pendingForfeitSigningRoute(),
	}
	pending, err := pendingForfeitSignatureRequest(correlation, req)
	require.NoError(t, err)

	requestID := string(pending.GetRequestId())
	broker.requests[requestID] = &forfeitSignatureRequest{
		proto:   pending,
		signReq: req,
	}
	broker.order = []string{requestID}

	sigs := testDaemonForfeitParticipantSignaturesForRequest(
		t, req, signerPrivs[1:]...,
	)

	err = broker.submit(pending.GetRequestId(), sigs)
	require.NoError(t, err)

	err = broker.submit(pending.GetRequestId(), sigs)
	require.NoError(t, err)
}

// testDaemonForfeitParticipantSignaturesForRequest signs the VTXO input of the
// pending forfeit request with every signer.
func testDaemonForfeitParticipantSignaturesForRequest(t *testing.T,
	req *vtxo.ForfeitParticipantSignRequest,
	signers ...*btcec.PrivateKey) []*daemonrpc.ForfeitParticipantSignature {

	t.Helper()

	sigs := make(
		[]*daemonrpc.ForfeitParticipantSignature, 0, len(signers),
	)
	for _, signer := range signers {
		prevFetcher, err := arktx.NewForfeitPrevOutFetcher(
			&arktx.VTXOSpendContext{
				Outpoint: req.VTXO.Outpoint,
				Output: &wire.TxOut{
					Value:    int64(req.VTXO.Amount),
					PkScript: req.VTXO.PkScript,
				},
			},
			&arktx.ConnectorSpendContext{
				Outpoint: req.ConnectorOutpoint,
				Output: &wire.TxOut{
					Value:    req.ConnectorAmount,
					PkScript: req.ConnectorPkScript,
				},
			},
		)
		require.NoError(t, err)

		sigHashes := txscript.NewTxSigHashes(req.ForfeitTx, prevFetcher)
		leaf := txscript.NewBaseTapLeaf(req.SpendPath.WitnessScript)
		sighash, err := txscript.CalcTapscriptSignaturehash(
			sigHashes, txscript.SigHashDefault, req.ForfeitTx,
			arktx.ForfeitVTXOInputIndex, prevFetcher, leaf,
		)
		require.NoError(t, err)

		sig, err := schnorr.Sign(signer, sighash)
		require.NoError(t, err)

		sigs = append(sigs, &daemonrpc.ForfeitParticipantSignature{
			Pubkey:    signer.PubKey().SerializeCompressed(),
			Signature: sig.Serialize(),
		})
	}

	return sigs
}

// sameSubmittedParticipantSet reports whether the broker result matches the
// submitted signature set without relying on order.
func sameSubmittedParticipantSet(got []*types.ForfeitParticipantSig,
	want []*daemonrpc.ForfeitParticipantSignature) bool {

	gotMap := make(map[string][]byte, len(got))
	for _, sig := range got {
		gotMap[string(sig.PubKey.SerializeCompressed())] =
			sig.Signature.Serialize()
	}

	for _, sig := range want {
		key := string(sig.GetPubkey())
		if !bytes.Equal(gotMap[key], sig.GetSignature()) {
			return false
		}
		delete(gotMap, key)
	}

	return len(gotMap) == 0
}

// testForfeitParticipantSignRequest returns a connector-bound forfeit signing
// request with a valid forfeit transaction.
func testForfeitParticipantSignRequest(t *testing.T) (
	*vtxo.ForfeitParticipantSignRequest, lntypes.Hash) {

	t.Helper()

	req, paymentHash, _ := testForfeitParticipantSignRequestWithSigners(t)

	return req, paymentHash
}

// testForfeitParticipantSignRequestWithSigners returns a connector-bound
// forfeit signing request and all non-operator keys needed to sign it.
func testForfeitParticipantSignRequestWithSigners(t *testing.T) (
	*vtxo.ForfeitParticipantSignRequest, lntypes.Hash,
	[]*btcec.PrivateKey) {

	t.Helper()

	policy, preimage, senderPriv, receiverPriv, operatorPriv :=
		testVHTLCPolicyFixture(t)
	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)
	pkScript, err := policy.PkScript()
	require.NoError(t, err)
	forfeitPath, err := policy.RefundPath()
	require.NoError(t, err)

	vtxoOutpoint := testWalletOpsOutpoint(31)
	connectorOutpoint := testWalletOpsOutpoint(32)
	connectorAmount := btcutil.Amount(330)
	serverForfeitPkScript := []byte{
		0x51,
		0x20,
	}
	tx, err := arktx.BuildForfeitTxWithContext(
		&vtxoOutpoint, btcutil.Amount(42_000),
		&connectorOutpoint, connectorAmount,
		serverForfeitPkScript, arktx.ForfeitTxContext{
			VTXOSequence: forfeitPath.RequiredSequence,
			LockTime:     forfeitPath.RequiredLockTime,
		},
	)
	require.NoError(t, err)

	req := &vtxo.ForfeitParticipantSignRequest{
		VTXO: &vtxo.Descriptor{
			Outpoint:       vtxoOutpoint,
			Amount:         btcutil.Amount(42_000),
			PkScript:       pkScript,
			PolicyTemplate: policyTemplate,
			ClientKey: keychain.KeyDescriptor{
				PubKey: senderPriv.PubKey(),
			},
			OperatorKey: operatorPriv.PubKey(),
		},
		SpendPath:         forfeitPath,
		ForfeitTx:         tx,
		ConnectorOutpoint: connectorOutpoint,
		ConnectorAmount:   int64(connectorAmount),
		ConnectorPkScript: []byte{
			0x51,
		},
		ServerForfeitPkScript: serverForfeitPkScript,
	}

	return req, preimage.Hash(), []*btcec.PrivateKey{
		senderPriv,
		receiverPriv,
	}
}

// testLocalOnlyForfeitParticipantSignRequest returns a forfeit request whose
// selected path requires only the descriptor's local key and the operator key.
func testLocalOnlyForfeitParticipantSignRequest(t *testing.T) (
	*vtxo.ForfeitParticipantSignRequest, lntypes.Hash) {

	t.Helper()

	policy, preimage, _, receiverPriv, operatorPriv :=
		testVHTLCPolicyFixture(t)
	policyTemplate, err := policy.Template.Encode()
	require.NoError(t, err)
	pkScript, err := policy.PkScript()
	require.NoError(t, err)
	forfeitPath, err := policy.ClaimPath(preimage)
	require.NoError(t, err)

	vtxoOutpoint := testWalletOpsOutpoint(41)
	connectorOutpoint := testWalletOpsOutpoint(42)
	connectorAmount := btcutil.Amount(330)
	serverForfeitPkScript := []byte{
		0x51,
		0x20,
	}
	tx, err := arktx.BuildForfeitTxWithContext(
		&vtxoOutpoint, btcutil.Amount(42_000),
		&connectorOutpoint, connectorAmount,
		serverForfeitPkScript, arktx.ForfeitTxContext{
			VTXOSequence: forfeitPath.RequiredSequence,
			LockTime:     forfeitPath.RequiredLockTime,
		},
	)
	require.NoError(t, err)

	req := &vtxo.ForfeitParticipantSignRequest{
		VTXO: &vtxo.Descriptor{
			Outpoint:       vtxoOutpoint,
			Amount:         btcutil.Amount(42_000),
			PkScript:       pkScript,
			PolicyTemplate: policyTemplate,
			ClientKey: keychain.KeyDescriptor{
				PubKey: receiverPriv.PubKey(),
			},
			OperatorKey: operatorPriv.PubKey(),
		},
		SpendPath:         forfeitPath,
		ForfeitTx:         tx,
		ConnectorOutpoint: connectorOutpoint,
		ConnectorAmount:   int64(connectorAmount),
		ConnectorPkScript: []byte{
			0x51,
		},
		ServerForfeitPkScript: serverForfeitPkScript,
	}

	return req, preimage.Hash()
}
