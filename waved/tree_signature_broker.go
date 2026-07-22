package waved

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultTreeSigningRequestLimit  = 100
	defaultAnsweredTreeRequestLimit = 256
	treeSigningRequestDomain        = "waved-tree-signing-request-v1"

	// defaultTreeSigningWaitTimeout bounds how long the round FSM blocks
	// waiting for an external party to answer one tree-signing request.
	defaultTreeSigningWaitTimeout = 5 * time.Minute
)

// treeSigResult carries the material an external party submits for one
// tree-signing request. Exactly one field is populated per request round.
type treeSigResult struct {
	nonce      tree.Musig2PubNonce
	partialSig *musig2.PartialSignature
}

type treeSignatureRequest struct {
	proto    *waverpc.PendingTreeSigningRequest
	answered bool
	result   treeSigResult
	waiters  []chan treeSigResult
}

// treeSignatureBroker exposes the round FSM's external MuSig2 tree-signing
// callbacks (ExternalTreeSignerBackend) to an outside party over daemon RPC.
// When a VTXO's cosigner key is marked external, the FSM's proxy signer calls
// FetchTreeNonce / FetchTreePartialSig; the broker parks each call as a pending
// request, surfaces it via ListPendingTreeSigningRequests, and unblocks it when
// SubmitTreeSignatures supplies the material. State is deliberately
// daemon-local: a restart abandons in-flight rounds rather than resuming a
// stale signing transcript.
type treeSignatureBroker struct {
	mu sync.Mutex

	nextSequence uint64
	requests     map[string]*treeSignatureRequest
	order        []string

	waitTimeout time.Duration
}

func newTreeSignatureBroker() *treeSignatureBroker {
	return &treeSignatureBroker{
		requests:    make(map[string]*treeSignatureRequest),
		waitTimeout: defaultTreeSigningWaitTimeout,
	}
}

// FetchTreeNonce blocks until the external party submits a public nonce for the
// requested session, or the context is cancelled.
func (b *treeSignatureBroker) FetchTreeNonce(ctx context.Context,
	req round.TreeSigningSessionRequest) (tree.Musig2PubNonce, error) {

	pending, err := pendingTreeSigningRequest(
		req, waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE,
	)
	if err != nil {
		return tree.Musig2PubNonce{}, err
	}

	result, err := b.block(ctx, pending)
	if err != nil {
		return tree.Musig2PubNonce{}, err
	}

	return result.nonce, nil
}

// FetchTreePartialSig blocks until the external party submits a partial
// signature for the requested session, or the context is cancelled.
func (b *treeSignatureBroker) FetchTreePartialSig(ctx context.Context,
	req round.TreeSigningSessionRequest) (*musig2.PartialSignature, error) {

	pending, err := pendingTreeSigningRequest(
		req, waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG,
	)
	if err != nil {
		return nil, err
	}

	result, err := b.block(ctx, pending)
	if err != nil {
		return nil, err
	}

	return result.partialSig, nil
}

// block parks the pending request and waits for it to be answered.
func (b *treeSignatureBroker) block(ctx context.Context,
	pending *waverpc.PendingTreeSigningRequest) (treeSigResult, error) {

	requestID := string(pending.GetRequestId())
	waiter := make(chan treeSigResult, 1)

	b.mu.Lock()
	stored, ok := b.requests[requestID]
	if ok {
		if !samePendingTreeSigningRequest(stored.proto, pending) {
			b.mu.Unlock()

			return treeSigResult{}, fmt.Errorf("tree signing " +
				"request id conflict")
		}
	} else {
		b.nextSequence++
		pending.Sequence = b.nextSequence
		stored = &treeSignatureRequest{proto: pending}
		b.requests[requestID] = stored
		b.order = append(b.order, requestID)
	}

	if stored.answered {
		result := stored.result
		b.mu.Unlock()

		return result, nil
	}

	stored.waiters = append(stored.waiters, waiter)
	b.mu.Unlock()

	waitCtx, cancel := b.waitContext(ctx)
	defer cancel()

	select {
	case result := <-waiter:
		return result, nil

	case <-waitCtx.Done():
		b.removeWaiter(requestID, waiter)

		return treeSigResult{}, waitCtx.Err()
	}
}

func (b *treeSignatureBroker) waitContext(ctx context.Context) (context.Context,
	context.CancelFunc) {

	if b.waitTimeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, b.waitTimeout)
}

func (b *treeSignatureBroker) removeWaiter(requestID string,
	waiter chan treeSigResult) {

	b.mu.Lock()
	defer b.mu.Unlock()

	stored := b.requests[requestID]
	if stored == nil {
		return
	}

	for i, candidate := range stored.waiters {
		if candidate != waiter {
			continue
		}

		stored.waiters = append(
			stored.waiters[:i], stored.waiters[i+1:]...,
		)

		return
	}
}

// list returns pending, unanswered tree-signing requests with a sequence above
// after, up to limit entries.
func (b *treeSignatureBroker) list(after uint64, limit uint32) (
	[]*waverpc.PendingTreeSigningRequest, uint64) {

	if b == nil {
		return nil, after
	}
	if limit == 0 {
		limit = defaultTreeSigningRequestLimit
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.pruneAnsweredRequestsLocked()

	requests := make([]*waverpc.PendingTreeSigningRequest, 0, limit)
	next := after
	for _, id := range b.order {
		req := b.requests[id]
		if req == nil || req.proto.GetSequence() <= after {
			continue
		}
		if req.answered {
			continue
		}

		requests = append(
			requests, clonePendingTreeSigningRequest(req.proto),
		)
		next = req.proto.GetSequence()
		if uint32(len(requests)) >= limit {
			break
		}
	}

	return requests, next
}

// submit records the external party's material for one pending request and
// wakes the blocked round FSM.
func (b *treeSignatureBroker) submit(requestID []byte,
	sigRound waverpc.TreeSigningRound, publicNonce,
	partialSignature []byte) error {

	if b == nil {
		return status.Error(
			codes.FailedPrecondition,
			"tree signature broker is not configured",
		)
	}
	if len(requestID) == 0 {
		return status.Error(
			codes.InvalidArgument, "request_id is required",
		)
	}

	id := string(requestID)

	b.mu.Lock()
	defer b.mu.Unlock()

	req, ok := b.requests[id]
	if !ok {
		return status.Error(
			codes.NotFound, "tree signing request not found",
		)
	}
	if req.proto.GetRound() != sigRound {
		return status.Errorf(codes.InvalidArgument, "round mismatch: "+
			"request is %v, submitted %v", req.proto.GetRound(),
			sigRound)
	}

	result, err := parseTreeSignatureResult(
		sigRound, publicNonce, partialSignature,
	)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}

	if req.answered {
		if sameTreeSigResult(sigRound, req.result, result) {
			return nil
		}

		return status.Error(
			codes.AlreadyExists,
			"tree signing request already answered",
		)
	}

	req.answered = true
	req.result = result
	waiters := req.waiters
	req.waiters = nil
	for _, waiter := range waiters {
		select {
		case waiter <- result:
		default:
		}
		close(waiter)
	}
	b.pruneAnsweredRequestsLocked()

	return nil
}

func (b *treeSignatureBroker) pruneAnsweredRequestsLocked() {
	answered := 0
	for i := len(b.order) - 1; i >= 0; i-- {
		id := b.order[i]
		req := b.requests[id]
		if req == nil {
			b.order = append(b.order[:i], b.order[i+1:]...)

			continue
		}
		if !req.answered {
			continue
		}

		answered++
		if answered <= defaultAnsweredTreeRequestLimit {
			continue
		}

		delete(b.requests, id)
		b.order = append(b.order[:i], b.order[i+1:]...)
	}
}

// pendingTreeSigningRequest builds the wire request for one signing round from
// the FSM's internal session request.
func pendingTreeSigningRequest(req round.TreeSigningSessionRequest,
	sigRound waverpc.TreeSigningRound) (*waverpc.PendingTreeSigningRequest,
	error) {

	if req.CosignerKey == nil {
		return nil, fmt.Errorf("cosigner key is required")
	}
	if len(req.Cosigners) == 0 {
		return nil, fmt.Errorf("cosigners are required")
	}

	cosigners := make([][]byte, 0, len(req.Cosigners))
	for i, key := range req.Cosigners {
		if key == nil {
			return nil, fmt.Errorf("cosigner %d is nil", i)
		}
		cosigners = append(cosigners, key.SerializeCompressed())
	}

	roundID := req.RoundID

	pending := &waverpc.PendingTreeSigningRequest{
		Round:          sigRound,
		RoundId:        append([]byte(nil), roundID[:]...),
		CosignerPubkey: req.CosignerKey.SerializeCompressed(),
		SessionId:      append([]byte(nil), req.SessionID[:]...),
		Cosigners:      cosigners,
		SweepTapscriptRoot: append(
			[]byte(nil), req.SweepTapscriptRoot...,
		),
	}

	if sigRound == waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG {
		pending.Sighash = append([]byte(nil), req.SigHash[:]...)
		pending.AggregateNonce = append(
			[]byte(nil), req.AggNonce[:]...,
		)
	}

	pending.RequestId = treeSigningRequestID(pending)

	return pending, nil
}

// treeSigningRequestID is the stable digest over a request's transcript. The
// round tag plus the round-two-only sighash and aggregate nonce give the NONCE
// and PARTIAL_SIG requests for one session distinct ids.
func treeSigningRequestID(req *waverpc.PendingTreeSigningRequest) []byte {
	h := sha256.New()
	h.Write([]byte(treeSigningRequestDomain))
	h.Write([]byte{0})
	h.Write([]byte{byte(req.GetRound())})
	h.Write(req.GetRoundId())
	h.Write(req.GetCosignerPubkey())
	h.Write(req.GetSessionId())
	for _, cosigner := range req.GetCosigners() {
		h.Write(cosigner)
	}
	h.Write(req.GetSweepTapscriptRoot())
	h.Write(req.GetSighash())
	h.Write(req.GetAggregateNonce())

	return h.Sum(nil)
}

// parseTreeSignatureResult validates and decodes the submitted material for the
// given round. It does not cryptographically verify a partial signature against
// the session (that would require reconstructing the MuSig2 context); an
// invalid partial signature instead surfaces downstream as a tree-signature
// validation failure when the operator returns the aggregated signature.
func parseTreeSignatureResult(sigRound waverpc.TreeSigningRound, publicNonce,
	partialSignature []byte) (treeSigResult, error) {

	switch sigRound {
	case waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE:
		if len(publicNonce) != musig2.PubNonceSize {
			return treeSigResult{}, fmt.Errorf("public_nonce must "+
				"be %d bytes, got %d", musig2.PubNonceSize,
				len(publicNonce))
		}
		var nonce tree.Musig2PubNonce
		copy(nonce[:], publicNonce)

		return treeSigResult{nonce: nonce}, nil

	case waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG:
		if len(partialSignature) == 0 {
			return treeSigResult{}, fmt.Errorf(
				"partial_signature is required")
		}
		sig := &musig2.PartialSignature{}
		if err := sig.Decode(
			bytes.NewReader(partialSignature),
		); err != nil {
			return treeSigResult{}, fmt.Errorf("decode partial "+
				"signature: %w", err)
		}

		return treeSigResult{partialSig: sig}, nil

	default:
		return treeSigResult{}, fmt.Errorf("unsupported signing "+
			"round: %v", sigRound)
	}
}

// sameTreeSigResult reports whether two results for the same round are equal,
// so an idempotent resubmit of identical material is accepted.
func sameTreeSigResult(sigRound waverpc.TreeSigningRound, a,
	b treeSigResult) bool {

	switch sigRound {
	case waverpc.TreeSigningRound_TREE_SIGNING_ROUND_NONCE:
		return a.nonce == b.nonce

	case waverpc.TreeSigningRound_TREE_SIGNING_ROUND_PARTIAL_SIG:
		if a.partialSig == nil || b.partialSig == nil {
			return a.partialSig == b.partialSig
		}

		return serializePartialSig(a.partialSig) ==
			serializePartialSig(b.partialSig)

	default:
		return false
	}
}

// serializePartialSig encodes a partial signature to a comparable string.
func serializePartialSig(sig *musig2.PartialSignature) string {
	var buf bytes.Buffer
	if err := sig.Encode(&buf); err != nil {
		return ""
	}

	return buf.String()
}

// clonePendingTreeSigningRequest returns a detached copy of a pending request
// so callers cannot mutate broker-owned state.
func clonePendingTreeSigningRequest(
	req *waverpc.PendingTreeSigningRequest,
) *waverpc.PendingTreeSigningRequest {

	if req == nil {
		return nil
	}

	cosigners := make([][]byte, 0, len(req.GetCosigners()))
	for _, cosigner := range req.GetCosigners() {
		cosigners = append(cosigners, bytes.Clone(cosigner))
	}

	return &waverpc.PendingTreeSigningRequest{
		RequestId:          bytes.Clone(req.GetRequestId()),
		Sequence:           req.GetSequence(),
		Round:              req.GetRound(),
		RoundId:            bytes.Clone(req.GetRoundId()),
		CosignerPubkey:     bytes.Clone(req.GetCosignerPubkey()),
		SessionId:          bytes.Clone(req.GetSessionId()),
		Cosigners:          cosigners,
		SweepTapscriptRoot: bytes.Clone(req.GetSweepTapscriptRoot()),
		Sighash:            bytes.Clone(req.GetSighash()),
		AggregateNonce:     bytes.Clone(req.GetAggregateNonce()),
	}
}

// samePendingTreeSigningRequest reports whether two pending requests carry
// identical signing transcripts.
func samePendingTreeSigningRequest(
	a, b *waverpc.PendingTreeSigningRequest) bool {

	if !bytes.Equal(a.GetRequestId(), b.GetRequestId()) ||
		a.GetRound() != b.GetRound() ||
		!bytes.Equal(a.GetRoundId(), b.GetRoundId()) ||
		!bytes.Equal(a.GetCosignerPubkey(), b.GetCosignerPubkey()) ||
		!bytes.Equal(a.GetSessionId(), b.GetSessionId()) ||
		!bytes.Equal(
			a.GetSweepTapscriptRoot(), b.GetSweepTapscriptRoot(),
		) ||
		!bytes.Equal(a.GetSighash(), b.GetSighash()) ||
		!bytes.Equal(a.GetAggregateNonce(), b.GetAggregateNonce()) {
		return false
	}

	if len(a.GetCosigners()) != len(b.GetCosigners()) {
		return false
	}
	for i := range a.GetCosigners() {
		if !bytes.Equal(a.GetCosigners()[i], b.GetCosigners()[i]) {
			return false
		}
	}

	return true
}

// Compile-time assertion that the broker satisfies the FSM's external
// tree-signer backend seam.
var _ round.ExternalTreeSignerBackend = (*treeSignatureBroker)(nil)
