//go:build swapruntime

package swapclientserver

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"

	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
)

// creditServerBridge adapts the daemon swap subserver to the credit
// subsystem's CreditServer interface. It routes every call through the existing
// swapclientrpc handlers so the credit actor reuses the daemon's account-key
// resolution, payment-hash dedup, and worker registry; it only maps between the
// proto shapes and the credit domain types.
type creditServerBridge struct {
	svc *swapClientService
}

// Compile-time check that the bridge satisfies the credit server surface.
var _ credit.CreditServer = (*creditServerBridge)(nil)

// CreateCredit forwards to the swap subserver's CreateCredit handler. The
// account pubkey is resolved by the daemon from its own identity, so the
// supplied accountPubKey is intentionally ignored here.
func (b *creditServerBridge) CreateCredit(ctx context.Context, _ []byte,
	idempotencyKey string, source credit.CreditSource, amountSat uint64,
	memo string) (*credit.CreateCreditResult, error) {

	protoSource, err := creditSourceToProto(source)
	if err != nil {
		return nil, err
	}

	resp, err := b.svc.CreateCredit(ctx, &swapclientrpc.CreateCreditRequest{
		IdempotencyKey: idempotencyKey,
		Source:         protoSource,
		AmountSat:      amountSat,
		Memo:           memo,
	})
	if err != nil {
		return nil, err
	}

	return &credit.CreateCreditResult{
		OperationID:       resp.GetOperationId(),
		State:             creditStateFromProtoEnum(resp.GetState()),
		Invoice:           resp.GetInvoice(),
		PaymentHash:       decodeHexHash(resp.GetPaymentHash()),
		AmountSat:         resp.GetAmountSat(),
		DestinationPubkey: resp.GetDestinationPubkey(),
	}, nil
}

// ListCredits forwards to the swap subserver's ListCredits handler.
func (b *creditServerBridge) ListCredits(ctx context.Context, _ []byte) (
	*credit.CreditSnapshot, error) {

	resp, err := b.svc.ListCredits(ctx, &swapclientrpc.ListCreditsRequest{
		Limit: math.MaxUint32,
	})
	if err != nil {
		return nil, err
	}

	snapshot := &credit.CreditSnapshot{
		FinalizedSat: resp.GetFinalizedSat(),
		ReservedSat:  resp.GetReservedSat(),
		AvailableSat: resp.GetAvailableSat(),
	}
	for _, op := range resp.GetOperations() {
		snapshot.Operations = append(snapshot.Operations,
			credit.ServerCreditOp{
				OperationID: op.GetOperationId(),
				State: creditStateFromProtoEnum(
					op.GetState(),
				),
				PaymentHash: decodeHexHash(op.GetPaymentHash()),
			})
	}

	return snapshot, nil
}

// RedeemCredit forwards to the swap subserver's RedeemCredit handler.
func (b *creditServerBridge) RedeemCredit(ctx context.Context, _ []byte,
	idempotencyKey string, amountSat uint64, destinationPubKey []byte) (
	*credit.RedeemResult, error) {

	resp, err := b.svc.RedeemCredit(ctx, &swapclientrpc.RedeemCreditRequest{
		IdempotencyKey:    idempotencyKey,
		AmountSat:         amountSat,
		DestinationPubkey: destinationPubKey,
	})
	if err != nil {
		return nil, err
	}

	return &credit.RedeemResult{
		OperationID: resp.GetOperationId(),
		State:       creditStateFromProtoEnum(resp.GetState()),
		RedeemedSat: resp.GetRedeemedSat(),
	}, nil
}

// StartPay forwards to the swap subserver's StartPay handler, which dedups by
// payment hash and starts (or reuses) the background pay worker.
func (b *creditServerBridge) StartPay(ctx context.Context, invoice string,
	maxFeeSat, routingFeeBudgetSat, maxCreditSat uint64) error {

	_, err := b.svc.StartPay(ctx, &swapclientrpc.StartPayRequest{
		Invoice:             invoice,
		MaxFeeSat:           maxFeeSat,
		RoutingFeeBudgetSat: routingFeeBudgetSat,
		MaxCreditSat:        maxCreditSat,
	})

	return err
}

// creditDaemonBridge adapts the daemon and Ark facade to the credit
// subsystem's CreditDaemon interface.
type creditDaemonBridge struct {
	daemon swaps.DaemonConn
	rpc    *waved.RPCServer
}

// Compile-time check that the bridge satisfies the credit daemon surface.
var _ credit.CreditDaemon = (*creditDaemonBridge)(nil)

// IdentityPubKey returns the compressed wallet identity pubkey.
func (b *creditDaemonBridge) IdentityPubKey(ctx context.Context) ([]byte,
	error) {

	key, err := b.daemon.IdentityPubKey(ctx)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, fmt.Errorf("identity pubkey is required")
	}

	return key.SerializeCompressed(), nil
}

// DustLimit returns the operator dust limit from the daemon GetInfo surface.
func (b *creditDaemonBridge) DustLimit(ctx context.Context) (uint64, error) {
	info, err := b.rpc.GetInfo(ctx, &waverpc.GetInfoRequest{})
	if err != nil {
		return 0, err
	}
	if info.GetServerInfo() == nil {
		return 0, fmt.Errorf("server info is required for dust limit")
	}

	return info.GetServerInfo().GetDustLimit(), nil
}

// SendOOR submits an idempotency-keyed OOR transfer to a pubkey-backed
// destination through the daemon, so the OOR registry dedups the transfer.
func (b *creditDaemonBridge) SendOOR(ctx context.Context,
	destinationPubKey []byte, amountSat uint64, idempotencyKey string) (
	string, error) {

	if amountSat > math.MaxInt64 {
		return "", fmt.Errorf("oor amount exceeds int64 range")
	}

	resp, err := b.rpc.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{{
			Destination: &waverpc.Output_Pubkey{
				Pubkey: append(
					[]byte(nil), destinationPubKey...,
				),
			},
			AmountSat: int64(amountSat),
		}},
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return "", err
	}

	return resp.GetSessionId(), nil
}

// AllocateReceiveScript allocates a fresh wallet-owned receive destination.
func (b *creditDaemonBridge) AllocateReceiveScript(ctx context.Context,
	label string) ([]byte, []byte, error) {

	info, err := b.daemon.AllocateReceiveScript(ctx, label)
	if err != nil {
		return nil, nil, err
	}
	if info == nil || len(info.PubKeyXOnly) == 0 ||
		len(info.PkScript) == 0 {
		return nil, nil, fmt.Errorf("receive script is required")
	}

	return append([]byte(nil), info.PubKeyXOnly...),
		append([]byte(nil), info.PkScript...), nil
}

// FindLiveVTXOByPkScript reports whether a live VTXO matching pkScript is
// indexed, and its amount.
func (b *creditDaemonBridge) FindLiveVTXOByPkScript(ctx context.Context,
	pkScript []byte) (bool, int64, error) {

	vtxo, err := b.daemon.FindLiveVTXOByPkScript(ctx, pkScript)
	if err != nil {
		return false, 0, err
	}
	if vtxo == nil {
		return false, 0, nil
	}

	return true, vtxo.AmountSat, nil
}

// creditSourceToProto maps a credit funding source to its proto enum.
func creditSourceToProto(source credit.CreditSource) (
	swapclientrpc.CreditFundingSource, error) {

	switch source {
	case credit.SourceLightningReceive:
		return swapclientrpc.
				CreditFundingSource_CREDIT_FUNDING_SOURCE_LIGHTNING_RECEIVE,
			nil

	case credit.SourceArkTopUp:
		return swapclientrpc.
			CreditFundingSource_CREDIT_FUNDING_SOURCE_ARK_TOPUP, nil

	default:
		return 0, fmt.Errorf("unknown credit funding source %d", source)
	}
}

// creditStateFromProtoEnum maps a proto credit state enum to the credit
// domain state. An unknown value maps to the empty state, which the credit
// actor treats as not-yet-terminal.
func creditStateFromProtoEnum(
	state swapclientrpc.CreditOperationState) credit.ServerCreditState {

	switch state {
	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_CREATED:
		return credit.ServerStateCreated

	case swapclientrpc.
		CreditOperationState_CREDIT_OPERATION_STATE_AWAITING_PAYMENT:
		return credit.ServerStateAwaitingPayment

	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_CREDITED:
		return credit.ServerStateCredited

	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_RESERVED:
		return credit.ServerStateReserved

	case swapclientrpc.
		CreditOperationState_CREDIT_OPERATION_STATE_PAYING_LIGHTNING:
		return credit.ServerStatePayingLightning

	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_DEBITED:
		return credit.ServerStateDebited

	case swapclientrpc.
		CreditOperationState_CREDIT_OPERATION_STATE_SENDING_OOR:
		return credit.ServerStateSendingOOR

	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_REDEEMED:
		return credit.ServerStateRedeemed

	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_RELEASED:
		return credit.ServerStateReleased

	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_EXPIRED:
		return credit.ServerStateExpired

	case swapclientrpc.CreditOperationState_CREDIT_OPERATION_STATE_FAILED:
		return credit.ServerStateFailed

	default:
		return ""
	}
}

// decodeHexHash decodes a hex payment hash to bytes, returning nil on an empty
// or malformed value (the field is informational on the credit row).
func decodeHexHash(hexHash string) []byte {
	if hexHash == "" {
		return nil
	}

	raw, err := hex.DecodeString(hexHash)
	if err != nil {
		return nil
	}

	return raw
}
