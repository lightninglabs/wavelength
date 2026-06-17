package darepod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	arktx "github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/vtxo"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultForfeitSignatureRequestLimit = 100
	forfeitSignatureRequestDomain       = "darepod-forfeit-signature-" +
		"request-v1"
)

func unspecifiedForfeitSigningDirection() daemonrpc.ForfeitSigningDirection {
	return daemonrpc.
		ForfeitSigningDirection_FORFEIT_SIGNING_DIRECTION_UNSPECIFIED
}

// forfeitSigningContext is the daemon-local correlation metadata supplied
// when a caller queues a custom refresh input.
type forfeitSigningContext struct {
	paymentHash []byte
	direction   daemonrpc.ForfeitSigningDirection
}

type forfeitSignatureRequest struct {
	proto      *daemonrpc.PendingForfeitParticipantSignatureRequest
	signReq    *vtxo.ForfeitParticipantSignRequest
	signatures []*types.ForfeitParticipantSig
	waiters    []chan []*types.ForfeitParticipantSig
}

// forfeitSignatureBroker exposes connector-bound VTXO signer callbacks to
// external swap coordination over daemon RPC.
type forfeitSignatureBroker struct {
	mu sync.Mutex

	nextSequence uint64
	contexts     map[string]forfeitSigningContext
	requests     map[string]*forfeitSignatureRequest
	order        []string

	inSwapSigner vtxo.ForfeitParticipantSigner
	waitTimeout time.Duration
}

func newForfeitSignatureBroker() *forfeitSignatureBroker {
	return &forfeitSignatureBroker{
		contexts:    make(map[string]forfeitSigningContext),
		requests:    make(map[string]*forfeitSignatureRequest),
		waitTimeout: DefaultForfeitCollectionTimeout,
	}
}

func (b *forfeitSignatureBroker) setInSwapSigner(
	signer vtxo.ForfeitParticipantSigner) {

	if b == nil {
		return
	}

	b.mu.Lock()
	b.inSwapSigner = signer
	b.mu.Unlock()
}

func (b *forfeitSignatureBroker) registerContext(outpoint string,
	ctx forfeitSigningContext) func() {

	if b == nil || outpoint == "" || len(ctx.paymentHash) == 0 ||
		ctx.direction == unspecifiedForfeitSigningDirection() {
		return func() {}
	}

	b.mu.Lock()
	b.contexts[outpoint] = ctx
	b.mu.Unlock()

	return func() {
		b.deleteContext(outpoint)
	}
}

func (b *forfeitSignatureBroker) sign(ctx context.Context,
	req *vtxo.ForfeitParticipantSignRequest) (
	[]*types.ForfeitParticipantSig, error) {

	if b == nil || req == nil || req.VTXO == nil {
		return nil, nil
	}

	outpoint := req.VTXO.Outpoint.String()

	b.mu.Lock()
	correlation, ok := b.contexts[outpoint]
	b.mu.Unlock()
	if !ok {
		return nil, nil
	}

	if correlation.direction == daemonrpc.
		ForfeitSigningDirection_FORFEIT_SIGNING_DIRECTION_IN_SWAP {

		b.mu.Lock()
		signer := b.inSwapSigner
		b.mu.Unlock()
		if signer != nil {
			sigs, err := signer(ctx, req)
			if err != nil {
				return nil, err
			}

			b.mu.Lock()
			delete(b.contexts, outpoint)
			b.mu.Unlock()

			return sigs, nil
		}
	}

	pending, err := pendingForfeitSignatureRequest(correlation, req)
	if err != nil {
		b.deleteContext(outpoint)

		return nil, err
	}

	requestID := string(pending.GetRequestId())
	waiter := make(chan []*types.ForfeitParticipantSig, 1)

	b.mu.Lock()
	stored, ok := b.requests[requestID]
	if ok {
		if !samePendingForfeitSignatureRequest(stored.proto, pending) {
			b.mu.Unlock()
			b.deleteContext(outpoint)

			return nil, fmt.Errorf("forfeit signature request id " +
				"conflict")
		}
	} else {
		b.nextSequence++
		pending.Sequence = b.nextSequence
		stored = &forfeitSignatureRequest{proto: pending, signReq: req}
		b.requests[requestID] = stored
		b.order = append(b.order, requestID)
	}

	if len(stored.signatures) != 0 {
		sigs := cloneParticipantSigs(stored.signatures)
		b.mu.Unlock()

		return sigs, nil
	}

	stored.waiters = append(stored.waiters, waiter)
	b.mu.Unlock()

	waitCtx, cancel := b.waitContext(ctx)
	defer cancel()

	select {
	case sigs := <-waiter:
		return sigs, nil

	case <-waitCtx.Done():
		b.removeWaiter(requestID, waiter)
		b.deleteContext(outpoint)

		return nil, waitCtx.Err()
	}
}

func (b *forfeitSignatureBroker) waitContext(ctx context.Context) (
	context.Context, context.CancelFunc) {

	if b.waitTimeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, b.waitTimeout)
}

func (b *forfeitSignatureBroker) deleteContext(outpoint string) {
	b.mu.Lock()
	delete(b.contexts, outpoint)
	b.mu.Unlock()
}

func (b *forfeitSignatureBroker) removeWaiter(requestID string,
	waiter chan []*types.ForfeitParticipantSig) {

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
func (b *forfeitSignatureBroker) list(after uint64, limit uint32) (
	[]*daemonrpc.PendingForfeitParticipantSignatureRequest, uint64) {

	if b == nil {
		return nil, after
	}
	if limit == 0 {
		limit = defaultForfeitSignatureRequestLimit
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	requests := make(
		[]*daemonrpc.PendingForfeitParticipantSignatureRequest, 0,
		limit,
	)
	next := after
	for _, id := range b.order {
		req := b.requests[id]
		if req == nil || req.proto.GetSequence() <= after {
			continue
		}
		if len(req.signatures) != 0 {
			next = req.proto.GetSequence()
			continue
		}

		cloned := clonePendingForfeitSignatureRequest(req.proto)
		requests = append(requests, cloned)
		next = req.proto.GetSequence()
		if uint32(len(requests)) >= limit {
			break
		}
	}

	return requests, next
}

func (b *forfeitSignatureBroker) submit(requestID []byte,
	signatures []*daemonrpc.ForfeitParticipantSignature) error {

	if b == nil {
		return status.Error(
			codes.FailedPrecondition,
			"forfeit signature broker is not configured",
		)
	}
	if len(requestID) == 0 {
		return status.Error(
			codes.InvalidArgument, "request_id is required",
		)
	}
	if len(signatures) == 0 {
		return status.Error(
			codes.InvalidArgument,
			"at least one signature is required",
		)
	}

	participantSigs := make(
		[]*types.ForfeitParticipantSig, 0, len(signatures),
	)
	for i, sig := range signatures {
		participantSig, err := parseForfeitParticipantSignature(sig)
		if err != nil {
			return status.Errorf(codes.InvalidArgument,
				"signature %d: %v", i, err)
		}
		participantSigs = append(participantSigs, participantSig)
	}

	id := string(requestID)

	b.mu.Lock()
	defer b.mu.Unlock()

	req, ok := b.requests[id]
	if !ok {
		return status.Error(
			codes.NotFound, "forfeit signature request not found",
		)
	}
	if len(req.signatures) != 0 {
		if sameParticipantSigSet(req.signatures, participantSigs) {
			return nil
		}

		return status.Error(
			codes.AlreadyExists,
			"forfeit signature request already signed",
		)
	}

	if err := verifyForfeitParticipantSignatures(
		req.signReq, participantSigs,
	); err != nil {
		return status.Errorf(codes.InvalidArgument, "verify "+
			"participant signatures: %v", err)
	}

	req.signatures = cloneParticipantSigs(participantSigs)
	delete(b.contexts, req.proto.GetVtxoOutpoint())
	waiters := req.waiters
	req.waiters = nil
	for _, waiter := range waiters {
		select {
		case waiter <- cloneParticipantSigs(participantSigs):
		default:
		}
		close(waiter)
	}

	return nil
}

func pendingForfeitSignatureRequest(correlation forfeitSigningContext,
	req *vtxo.ForfeitParticipantSignRequest) (
	*daemonrpc.PendingForfeitParticipantSignatureRequest, error) {

	if req.SpendPath == nil {
		return nil, fmt.Errorf("spend path is required")
	}
	if req.ForfeitTx == nil {
		return nil, fmt.Errorf("forfeit tx is required")
	}
	if req.VTXO == nil {
		return nil, fmt.Errorf("vTXO is required")
	}

	spendPath, err := req.SpendPath.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode forfeit spend path: %w", err)
	}

	var txBytes bytes.Buffer
	if err := req.ForfeitTx.Serialize(&txBytes); err != nil {
		return nil, fmt.Errorf("serialize forfeit tx: %w", err)
	}

	pending := &daemonrpc.PendingForfeitParticipantSignatureRequest{
		PaymentHash:  append([]byte(nil), correlation.paymentHash...),
		Direction:    correlation.direction,
		VtxoOutpoint: req.VTXO.Outpoint.String(),
		VtxoAmountSat: uint64(
			req.VTXO.Amount,
		),
		VtxoPkScript: append([]byte(nil), req.VTXO.PkScript...),
		VtxoPolicyTemplate: append(
			[]byte(nil), req.VTXO.PolicyTemplate...,
		),
		ForfeitSpendPath:   spendPath,
		UnsignedForfeitTx:  txBytes.Bytes(),
		ConnectorOutpoint:  req.ConnectorOutpoint.String(),
		ConnectorAmountSat: uint64(req.ConnectorAmount),
		ConnectorPkScript:  bytes.Clone(req.ConnectorPkScript),
		ServerForfeitPkScript: append(
			[]byte(nil), req.ServerForfeitPkScript...,
		),
	}
	pending.RequestId = forfeitSignatureRequestID(pending)

	return pending, nil
}

func forfeitSignatureRequestID(
	req *daemonrpc.PendingForfeitParticipantSignatureRequest) []byte {

	h := sha256.New()
	h.Write([]byte(forfeitSignatureRequestDomain))
	h.Write([]byte{0})
	h.Write(req.GetPaymentHash())
	h.Write([]byte{byte(req.GetDirection())})
	h.Write([]byte(req.GetVtxoOutpoint()))
	h.Write(req.GetVtxoPkScript())
	h.Write(req.GetVtxoPolicyTemplate())
	h.Write(req.GetForfeitSpendPath())
	h.Write(req.GetUnsignedForfeitTx())
	h.Write([]byte(req.GetConnectorOutpoint()))
	h.Write(req.GetConnectorPkScript())
	h.Write(req.GetServerForfeitPkScript())
	sum := h.Sum(nil)

	return sum
}

// verifyForfeitParticipantSignatures verifies that the submitted signatures
// satisfy the exact connector-bound participant signing request.
func verifyForfeitParticipantSignatures(req *vtxo.ForfeitParticipantSignRequest,
	sigs []*types.ForfeitParticipantSig) error {

	if req == nil {
		return fmt.Errorf("pending signing request is missing")
	}
	if req.VTXO == nil {
		return fmt.Errorf("pending signing request vtxo is missing")
	}
	if req.VTXO.OperatorKey == nil {
		return fmt.Errorf("pending signing request operator key is " +
			"missing")
	}
	if req.SpendPath == nil {
		return fmt.Errorf("pending signing request spend path is " +
			"missing")
	}
	if req.ForfeitTx == nil {
		return fmt.Errorf("pending signing request forfeit tx is " +
			"missing")
	}

	template, err := arkscript.DecodePolicyTemplate(
		req.VTXO.PolicyTemplate,
	)
	if err != nil {
		return fmt.Errorf("decode policy template: %w", err)
	}
	signingKeys, err := arkscript.SigningKeysForSpendPath(
		template, req.SpendPath,
	)
	if err != nil {
		return fmt.Errorf("resolve signing keys: %w", err)
	}

	operatorKeyID := participantKeyID(req.VTXO.OperatorKey)
	required := make(map[string]*btcec.PublicKey, len(signingKeys))
	for _, key := range signingKeys {
		if key == nil || participantKeyID(key) == operatorKeyID {
			continue
		}

		required[participantKeyID(key)] = key
	}
	if len(required) == 0 {
		return fmt.Errorf("no non-operator participant keys required")
	}

	seen := make(map[string]struct{}, len(sigs))
	for _, sig := range sigs {
		if sig == nil || sig.PubKey == nil || sig.Signature == nil {
			return fmt.Errorf("participant signature is incomplete")
		}

		keyID := participantKeyID(sig.PubKey)
		if _, ok := required[keyID]; !ok {
			return fmt.Errorf("unexpected participant key")
		}
		if _, ok := seen[keyID]; ok {
			return fmt.Errorf("duplicate participant key")
		}
		seen[keyID] = struct{}{}

		ok, err := verifyForfeitParticipantSignature(req, sig)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("invalid participant signature")
		}
	}

	for keyID := range required {
		if _, ok := seen[keyID]; !ok {
			return fmt.Errorf("missing participant signature")
		}
	}

	return nil
}

// verifyForfeitParticipantSignature verifies one participant signature against
// the VTXO input sighash for the pending forfeit transaction.
func verifyForfeitParticipantSignature(req *vtxo.ForfeitParticipantSignRequest,
	sig *types.ForfeitParticipantSig) (bool, error) {

	vtxoOutput := &wire.TxOut{
		Value:    int64(req.VTXO.Amount),
		PkScript: bytes.Clone(req.VTXO.PkScript),
	}
	connectorOutput := &wire.TxOut{
		Value:    req.ConnectorAmount,
		PkScript: bytes.Clone(req.ConnectorPkScript),
	}
	prevFetcher, err := arktx.NewForfeitPrevOutFetcher(
		&arktx.VTXOSpendContext{
			Outpoint: req.VTXO.Outpoint,
			Output:   vtxoOutput,
		},
		&arktx.ConnectorSpendContext{
			Outpoint: req.ConnectorOutpoint,
			Output:   connectorOutput,
		},
	)
	if err != nil {
		return false, fmt.Errorf("prevout fetcher: %w", err)
	}

	sigHashes := txscript.NewTxSigHashes(req.ForfeitTx, prevFetcher)
	leaf := txscript.NewBaseTapLeaf(req.SpendPath.WitnessScript)
	sighash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, txscript.SigHashDefault, req.ForfeitTx,
		arktx.ForfeitVTXOInputIndex, prevFetcher, leaf,
	)
	if err != nil {
		return false, fmt.Errorf("forfeit sighash: %w", err)
	}

	return sig.Signature.Verify(sighash, sig.PubKey), nil
}

// participantKeyID returns the x-only key identity used for participant maps.
func participantKeyID(pub *btcec.PublicKey) string {
	if pub == nil {
		return ""
	}

	return string(schnorr.SerializePubKey(pub))
}

func parseForfeitParticipantSignature(
	sig *daemonrpc.ForfeitParticipantSignature) (
	*types.ForfeitParticipantSig, error) {

	if sig == nil {
		return nil, fmt.Errorf("signature is required")
	}
	pubKey, err := btcec.ParsePubKey(sig.GetPubkey())
	if err != nil {
		return nil, fmt.Errorf("parse pubkey: %w", err)
	}
	schnorrSig, err := schnorr.ParseSignature(sig.GetSignature())
	if err != nil {
		return nil, fmt.Errorf("parse schnorr signature: %w", err)
	}

	return &types.ForfeitParticipantSig{
		PubKey:    pubKey,
		Signature: schnorrSig,
	}, nil
}

func clonePendingForfeitSignatureRequest(
	req *daemonrpc.PendingForfeitParticipantSignatureRequest,
) *daemonrpc.PendingForfeitParticipantSignatureRequest {

	if req == nil {
		return nil
	}

	return &daemonrpc.PendingForfeitParticipantSignatureRequest{
		RequestId:          bytes.Clone(req.GetRequestId()),
		Sequence:           req.GetSequence(),
		PaymentHash:        bytes.Clone(req.GetPaymentHash()),
		Direction:          req.GetDirection(),
		VtxoOutpoint:       req.GetVtxoOutpoint(),
		VtxoAmountSat:      req.GetVtxoAmountSat(),
		VtxoPkScript:       bytes.Clone(req.GetVtxoPkScript()),
		VtxoPolicyTemplate: bytes.Clone(req.GetVtxoPolicyTemplate()),
		ForfeitSpendPath:   bytes.Clone(req.GetForfeitSpendPath()),
		UnsignedForfeitTx:  bytes.Clone(req.GetUnsignedForfeitTx()),
		ConnectorOutpoint:  req.GetConnectorOutpoint(),
		ConnectorAmountSat: req.GetConnectorAmountSat(),
		ConnectorPkScript:  bytes.Clone(req.GetConnectorPkScript()),
		ServerForfeitPkScript: bytes.Clone(
			req.GetServerForfeitPkScript(),
		),
	}
}

func samePendingForfeitSignatureRequest(
	a, b *daemonrpc.PendingForfeitParticipantSignatureRequest) bool {

	return bytes.Equal(a.GetRequestId(), b.GetRequestId()) &&
		bytes.Equal(a.GetPaymentHash(), b.GetPaymentHash()) &&
		a.GetDirection() == b.GetDirection() &&
		a.GetVtxoOutpoint() == b.GetVtxoOutpoint() &&
		a.GetVtxoAmountSat() == b.GetVtxoAmountSat() &&
		bytes.Equal(a.GetVtxoPkScript(), b.GetVtxoPkScript()) &&
		bytes.Equal(
			a.GetVtxoPolicyTemplate(), b.GetVtxoPolicyTemplate(),
		) &&
		bytes.Equal(a.GetForfeitSpendPath(), b.GetForfeitSpendPath()) &&
		bytes.Equal(
			a.GetUnsignedForfeitTx(), b.GetUnsignedForfeitTx(),
		) &&
		a.GetConnectorOutpoint() == b.GetConnectorOutpoint() &&
		a.GetConnectorAmountSat() == b.GetConnectorAmountSat() &&
		bytes.Equal(
			a.GetConnectorPkScript(), b.GetConnectorPkScript(),
		) &&
		bytes.Equal(
			a.GetServerForfeitPkScript(),
			b.GetServerForfeitPkScript(),
		)
}

func cloneParticipantSigs(
	sigs []*types.ForfeitParticipantSig,
) []*types.ForfeitParticipantSig {

	out := make([]*types.ForfeitParticipantSig, 0, len(sigs))
	for _, sig := range sigs {
		if sig == nil {
			continue
		}
		out = append(out, &types.ForfeitParticipantSig{
			PubKey:    sig.PubKey,
			Signature: sig.Signature,
		})
	}

	return out
}

func sameParticipantSigSet(a, b []*types.ForfeitParticipantSig) bool {
	if len(a) != len(b) {
		return false
	}

	aByKey, ok := participantSigMap(a)
	if !ok {
		return false
	}
	bByKey, ok := participantSigMap(b)
	if !ok {
		return false
	}
	for key, aSig := range aByKey {
		bSig, ok := bByKey[key]
		if !ok || !bytes.Equal(aSig, bSig) {
			return false
		}
	}

	return true
}

func participantSigMap(sigs []*types.ForfeitParticipantSig) (map[string][]byte,
	bool) {

	out := make(map[string][]byte, len(sigs))
	for _, sig := range sigs {
		if sig == nil || sig.PubKey == nil || sig.Signature == nil {
			return nil, false
		}

		key := string(sig.PubKey.SerializeCompressed())
		if _, ok := out[key]; ok {
			return nil, false
		}
		out[key] = sig.Signature.Serialize()
	}

	return out, true
}

var _ vtxo.ForfeitParticipantSigner = (*forfeitSignatureBroker)(nil).sign
