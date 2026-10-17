package swaps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/proto"
)

// receiveForfeitBinding is the immutable receive-side context that a mailbox
// forfeit request must match before it reaches the signer.
type receiveForfeitBinding struct {
	paymentHash   lntypes.Hash
	vhtlcOutpoint string
	vhtlcAmount   int64
	vhtlcPkScript []byte
	policy        []byte
}

// receiveForfeitBindingGate publishes immutable bindings after their funding
// transitions cross the configured persistence step. A later refresh
// atomically replaces the snapshot so the responder follows the current
// authoritative outpoint. The snapshot is crash-durable only when the client
// has a store.
type receiveForfeitBindingGate struct {
	binding atomic.Pointer[receiveForfeitBinding]
}

// newReceiveForfeitBindingGate returns an unpublished signing-context gate.
func newReceiveForfeitBindingGate() *receiveForfeitBindingGate {
	return &receiveForfeitBindingGate{}
}

// publishReceiveForfeitBinding publishes the latest complete funded binding.
// Callers must invoke it only after the corresponding session mutation and
// configured persistence step have succeeded.
func (s *ReceiveSession) publishReceiveForfeitBinding() {
	if s == nil || s.forfeitBindingGate == nil {
		return
	}

	binding, ok := receiveForfeitBindingFromSession(s)
	if !ok {
		return
	}

	s.forfeitBindingGate.binding.Store(&binding)
}

// receiveForfeitBindingFromSession copies a complete authoritative funding
// context from the session. Incomplete and pre-funding states remain
// unpublished so mailbox delivery can be retried after the configured
// persistence step succeeds.
func receiveForfeitBindingFromSession(s *ReceiveSession) (receiveForfeitBinding,
	bool) {

	switch s.state {
	case ReceiveStateVHTLCFunded, ReceiveStateClaimInitiated,
		ReceiveStateCompleted:

	default:
		return receiveForfeitBinding{}, false
	}

	if s.PaymentHash == (lntypes.Hash{}) || s.vhtlcOutpoint == "" ||
		s.vhtlcAmount <= 0 || len(s.vhtlcPkScript) == 0 ||
		len(s.vhtlcPolicyTemplate) == 0 {
		return receiveForfeitBinding{}, false
	}

	return receiveForfeitBinding{
		paymentHash:   s.PaymentHash,
		vhtlcOutpoint: s.vhtlcOutpoint,
		vhtlcAmount:   s.vhtlcAmount,
		vhtlcPkScript: bytes.Clone(s.vhtlcPkScript),
		policy:        bytes.Clone(s.vhtlcPolicyTemplate),
	}, true
}

// load returns the immutable binding published at the authoritative session
// boundary.
func (g *receiveForfeitBindingGate) load() (receiveForfeitBinding, error) {
	if g == nil {
		return receiveForfeitBinding{}, fmt.Errorf("receive vHTLC " +
			"funding is not authoritative")
	}

	binding := g.binding.Load()
	if binding == nil {
		return receiveForfeitBinding{}, fmt.Errorf("receive vHTLC " +
			"funding is not authoritative")
	}

	return *binding, nil
}

// receiveForfeitResponder owns the immutable dependencies used by the mailbox
// goroutine. It never dereferences the mutable ReceiveSession while the state
// machine is applying or rolling back a transition.
type receiveForfeitResponder struct {
	client      *SwapClient
	bindingGate *receiveForfeitBindingGate
}

// ForfeitSignaturePayloadFromVTXORequest converts the vtxo manager's exact
// connector-bound signing request into the swap-server transcript shape.
func ForfeitSignaturePayloadFromVTXORequest(
	req *vtxo.ForfeitParticipantSignRequest) (*ForfeitSignaturePayload,
	error) {

	if req == nil {
		return nil, fmt.Errorf("forfeit participant sign request is " +
			"required")
	}
	if req.VTXO == nil {
		return nil, fmt.Errorf("forfeit participant VTXO is required")
	}
	if req.SpendPath == nil {
		return nil, fmt.Errorf("forfeit participant spend path is " +
			"required")
	}
	if req.ForfeitTx == nil {
		return nil, fmt.Errorf("forfeit transaction is required")
	}

	spendPath, err := req.SpendPath.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode forfeit spend path: %w", err)
	}

	unsignedForfeitTx, err := serializeForfeitTx(req.ForfeitTx)
	if err != nil {
		return nil, err
	}

	paymentHash, err := paymentHashFromVHTLCTemplate(
		req.VTXO.PolicyTemplate,
	)
	if err != nil {
		return nil, err
	}

	payload := &ForfeitSignaturePayload{
		PaymentHash:           paymentHash,
		VHTLCOutpoint:         req.VTXO.Outpoint.String(),
		VHTLCAmountSat:        int64(req.VTXO.Amount),
		VHTLCPkScript:         bytes.Clone(req.VTXO.PkScript),
		VHTLCPolicyTemplate:   bytes.Clone(req.VTXO.PolicyTemplate),
		ForfeitSpendPath:      spendPath,
		UnsignedForfeitTx:     unsignedForfeitTx,
		ConnectorOutpoint:     req.ConnectorOutpoint.String(),
		ConnectorAmountSat:    req.ConnectorAmount,
		ConnectorPkScript:     bytes.Clone(req.ConnectorPkScript),
		ServerForfeitPkScript: bytes.Clone(req.ServerForfeitPkScript),
	}
	payload.RequestID = stableForfeitSignatureRequestID(payload)

	return payload, nil
}

// SignVTXOForfeitRequestFromPayload maps a swap-server transcript into the
// daemon's exact local signing oracle request.
func SignVTXOForfeitRequestFromPayload(payload *ForfeitSignaturePayload) (
	*waverpc.SignVTXOForfeitRequest, error) {

	if _, err := forfeitSignaturePayloadToProto(payload); err != nil {
		return nil, err
	}

	return &waverpc.SignVTXOForfeitRequest{
		VtxoOutpoint:       payload.VHTLCOutpoint,
		VtxoAmountSat:      payload.VHTLCAmountSat,
		VtxoPkScript:       bytes.Clone(payload.VHTLCPkScript),
		VtxoPolicyTemplate: bytes.Clone(payload.VHTLCPolicyTemplate),
		SpendPath:          bytes.Clone(payload.ForfeitSpendPath),
		UnsignedForfeitTx:  bytes.Clone(payload.UnsignedForfeitTx),
		ConnectorOutpoint:  payload.ConnectorOutpoint,
		ConnectorAmountSat: payload.ConnectorAmountSat,
		ConnectorPkScript:  bytes.Clone(payload.ConnectorPkScript),
		ServerForfeitPkScript: bytes.Clone(
			payload.ServerForfeitPkScript,
		),
	}, nil
}

func (r *receiveForfeitResponder) handleOutSwapForfeitSignatureRequest(
	ctx context.Context,
	notification *OutSwapForfeitSignatureNotification) error {

	if notification == nil {
		return fmt.Errorf("out-swap forfeit signature notification " +
			"is required")
	}
	if notification.Payload == nil {
		return fmt.Errorf("out-swap forfeit signature payload is " +
			"required")
	}
	binding, err := r.bindingGate.load()
	if err != nil {
		return err
	}
	if err := validateOutSwapForfeitSignaturePayload(
		binding, notification.Payload,
	); err != nil {
		return err
	}
	if r.client == nil || r.client.daemon == nil {
		return fmt.Errorf("daemon connection is not configured")
	}
	if r.client.server == nil {
		return fmt.Errorf("swap server connection is not configured")
	}

	req, err := SignVTXOForfeitRequestFromPayload(notification.Payload)
	if err != nil {
		return err
	}

	resp, err := r.client.daemon.SignVTXOForfeit(ctx, req)
	if err != nil {
		return fmt.Errorf("sign out-swap forfeit payload: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("sign out-swap forfeit payload: empty " +
			"daemon response")
	}

	signature := &ForfeitParticipantSignature{
		PubKey:    append([]byte(nil), resp.GetPubkey()...),
		Signature: append([]byte(nil), resp.GetSignature()...),
	}
	if _, err := forfeitParticipantSignatureToProto(signature); err != nil {
		return fmt.Errorf("daemon returned invalid forfeit "+
			"signature: %w", err)
	}

	if err := r.client.server.SubmitOutSwapForfeitSignature(
		ctx, notification.Payload, signature,
	); err != nil {
		return fmt.Errorf("submit out-swap forfeit signature: %w", err)
	}

	if notification.Ack != nil {
		if err := notification.Ack(ctx); err != nil {
			return fmt.Errorf("ack out-swap forfeit signature "+
				"request: %w", err)
		}
	}

	return nil
}

func validateOutSwapForfeitSignaturePayload(binding receiveForfeitBinding,
	payload *ForfeitSignaturePayload) error {

	if payload == nil {
		return fmt.Errorf("out-swap forfeit signature payload is " +
			"required")
	}

	if payload.PaymentHash != binding.paymentHash {
		return fmt.Errorf("out-swap forfeit signature payment hash " +
			"mismatch")
	}
	if payload.VHTLCOutpoint != binding.vhtlcOutpoint {
		return fmt.Errorf("out-swap forfeit signature vHTLC outpoint " +
			"mismatch")
	}
	if payload.VHTLCAmountSat != binding.vhtlcAmount {
		return fmt.Errorf("out-swap forfeit signature vHTLC amount " +
			"mismatch")
	}
	if !bytes.Equal(payload.VHTLCPkScript, binding.vhtlcPkScript) {
		return fmt.Errorf("out-swap forfeit signature vHTLC script " +
			"mismatch")
	}
	if !bytes.Equal(
		payload.VHTLCPolicyTemplate, binding.policy,
	) {
		return fmt.Errorf("out-swap forfeit signature vHTLC policy " +
			"mismatch")
	}

	templateHash, err := paymentHashFromVHTLCTemplate(
		payload.VHTLCPolicyTemplate,
	)
	if err != nil {
		return fmt.Errorf("validate out-swap forfeit signature vHTLC "+
			"policy: %w", err)
	}
	if templateHash != payload.PaymentHash ||
		templateHash != binding.paymentHash {
		return fmt.Errorf("out-swap forfeit signature vHTLC policy " +
			"payment hash mismatch")
	}

	return nil
}

func (r *receiveForfeitResponder) respondToOutSwapForfeitSignatureRequests(
	ctx context.Context, receiver OutSwapForfeitSignatureReceiver,
	paymentHash lntypes.Hash, clientPubKey *btcec.PublicKey) {

	if receiver == nil || clientPubKey == nil {
		return
	}

	for {
		notification, err := receiver.WaitOutSwapForfeitSignature(
			ctx, paymentHash, clientPubKey,
		)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			r.client.log.WarnS(
				ctx, "Unable to receive out-swap forfeit "+
					"signature request", err,
			)
			waitErr := waitForFixedPoll(
				ctx, r.client.waitPollInterval,
			)
			if waitErr != nil {
				return
			}

			continue
		}

		if err := r.handleOutSwapForfeitSignatureRequest(
			ctx, notification,
		); err != nil {

			if ctx.Err() != nil {
				return
			}

			r.client.log.WarnS(
				ctx, "Unable to handle out-swap forfeit "+
					"signature request", err,
			)
			waitErr := waitForFixedPoll(
				ctx, r.client.waitPollInterval,
			)
			if waitErr != nil {
				return
			}
		}
	}
}

func serializeForfeitTx(tx *wire.MsgTx) ([]byte, error) {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize forfeit tx: %w", err)
	}

	return buf.Bytes(), nil
}

func stableForfeitSignatureRequestID(payload *ForfeitSignaturePayload) []byte {
	protoPayload, err := forfeitSignaturePayloadToProto(
		&ForfeitSignaturePayload{
			RequestID:             []byte("request-id-placeholder"),
			PaymentHash:           payload.PaymentHash,
			VHTLCOutpoint:         payload.VHTLCOutpoint,
			VHTLCAmountSat:        payload.VHTLCAmountSat,
			VHTLCPkScript:         payload.VHTLCPkScript,
			VHTLCPolicyTemplate:   payload.VHTLCPolicyTemplate,
			ForfeitSpendPath:      payload.ForfeitSpendPath,
			UnsignedForfeitTx:     payload.UnsignedForfeitTx,
			ConnectorOutpoint:     payload.ConnectorOutpoint,
			ConnectorAmountSat:    payload.ConnectorAmountSat,
			ConnectorPkScript:     payload.ConnectorPkScript,
			ServerForfeitPkScript: payload.ServerForfeitPkScript,
		},
	)
	if err != nil {
		sum := sha256.Sum256(payload.UnsignedForfeitTx)

		return sum[:]
	}
	protoPayload.RequestId = nil

	raw, err := proto.Marshal(protoPayload)
	if err != nil {
		sum := sha256.Sum256(payload.UnsignedForfeitTx)

		return sum[:]
	}

	sum := sha256.Sum256(raw)

	return sum[:]
}

func paymentHashFromVHTLCTemplate(raw []byte) (lntypes.Hash, error) {
	template, err := arkscript.DecodePolicyTemplate(raw)
	if err != nil {
		return lntypes.Hash{}, fmt.Errorf("decode vHTLC policy "+
			"template: %w", err)
	}

	var found lntypes.Hash
	for _, leaf := range template.Leaves {
		hash, ok, err := paymentHashFromNode(leaf.Node)
		if err != nil {
			return lntypes.Hash{}, err
		}
		if !ok {
			continue
		}
		if found != (lntypes.Hash{}) && found != hash {
			return lntypes.Hash{}, fmt.Errorf("vHTLC policy " +
				"template contains multiple payment hashes")
		}
		found = hash
	}
	if found == (lntypes.Hash{}) {
		return lntypes.Hash{}, fmt.Errorf("vHTLC policy template " +
			"missing payment hash")
	}

	return found, nil
}

func paymentHashFromNode(node arkscript.Node) (lntypes.Hash, bool, error) {
	switch n := node.(type) {
	case *arkscript.Condition:
		hash, ok, err := paymentHashFromPredicate(n.Predicate)
		if err != nil || ok {
			return hash, ok, err
		}

		return paymentHashFromNode(n.Inner)

	case *arkscript.CSV:
		return paymentHashFromNode(n.Inner)

	default:
		return lntypes.Hash{}, false, nil
	}
}

func paymentHashFromPredicate(predicate []byte) (lntypes.Hash, bool, error) {
	tokenizer := txscript.MakeScriptTokenizer(0, predicate)
	var sawSHA256 bool
	for tokenizer.Next() {
		if tokenizer.Opcode() == txscript.OP_SHA256 {
			sawSHA256 = true
			continue
		}
		if !sawSHA256 || len(tokenizer.Data()) != lntypes.HashSize {
			continue
		}

		var hash lntypes.Hash
		copy(hash[:], tokenizer.Data())

		return hash, true, nil
	}
	if err := tokenizer.Err(); err != nil {
		return lntypes.Hash{}, false, fmt.Errorf("parse vHTLC "+
			"predicate: %w", err)
	}

	return lntypes.Hash{}, false, nil
}
