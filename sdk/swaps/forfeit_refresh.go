package swaps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/proto"
)

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

func (s *ReceiveSession) handleOutSwapForfeitSignatureRequest(
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
	if err := s.validateOutSwapForfeitSignaturePayload(
		notification.Payload,
	); err != nil {
		return err
	}
	if s.client == nil || s.client.daemon == nil {
		return fmt.Errorf("daemon connection is not configured")
	}
	if s.client.server == nil {
		return fmt.Errorf("swap server connection is not configured")
	}

	req, err := SignVTXOForfeitRequestFromPayload(notification.Payload)
	if err != nil {
		return err
	}

	resp, err := s.client.daemon.SignVTXOForfeit(ctx, req)
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

	if err := s.client.server.SubmitOutSwapForfeitSignature(
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

func (s *ReceiveSession) validateOutSwapForfeitSignaturePayload(
	payload *ForfeitSignaturePayload) error {

	if payload.PaymentHash != s.PaymentHash {
		return fmt.Errorf("out-swap forfeit signature payment hash " +
			"mismatch")
	}
	if s.vhtlcOutpoint != "" && payload.VHTLCOutpoint != s.vhtlcOutpoint {
		return fmt.Errorf("out-swap forfeit signature vHTLC outpoint " +
			"mismatch")
	}
	if s.vhtlcAmount != 0 && payload.VHTLCAmountSat != s.vhtlcAmount {
		return fmt.Errorf("out-swap forfeit signature vHTLC amount " +
			"mismatch")
	}
	if len(s.vhtlcPkScript) != 0 &&
		!bytes.Equal(payload.VHTLCPkScript, s.vhtlcPkScript) {
		return fmt.Errorf("out-swap forfeit signature vHTLC script " +
			"mismatch")
	}
	if len(s.vhtlcPolicyTemplate) != 0 &&
		!bytes.Equal(
			payload.VHTLCPolicyTemplate, s.vhtlcPolicyTemplate,
		) {
		return fmt.Errorf("out-swap forfeit signature vHTLC policy " +
			"mismatch")
	}

	return nil
}

func (s *ReceiveSession) respondToOutSwapForfeitSignatureRequests(
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

			s.client.log.WarnS(
				ctx, "Unable to receive out-swap forfeit "+
					"signature request", err,
			)
			waitErr := waitForFixedPoll(
				ctx, s.client.waitPollInterval,
			)
			if waitErr != nil {
				return
			}

			continue
		}

		if err := s.handleOutSwapForfeitSignatureRequest(
			ctx, notification,
		); err != nil {

			if ctx.Err() != nil {
				return
			}

			s.client.log.WarnS(
				ctx, "Unable to handle out-swap forfeit "+
					"signature request", err,
			)
			waitErr := waitForFixedPoll(
				ctx, s.client.waitPollInterval,
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
