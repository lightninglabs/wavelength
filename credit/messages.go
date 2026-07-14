package credit

import (
	"fmt"
	"io"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightningnetwork/lnd/tlv"
)

// CreditActorServiceKeyName is the receptionist key used to discover the credit
// supervisor actor in the actor system.
const CreditActorServiceKeyName = "credit-client"

// NewServiceKey returns the service key for looking up the credit supervisor
// actor in the actor system's receptionist.
func NewServiceKey() actor.ServiceKey[CreditMsg, CreditResp] {
	return actor.NewServiceKey[CreditMsg, CreditResp](
		CreditActorServiceKeyName,
	)
}

// CreditMsg is the message constraint for the credit supervisor. The supervisor
// runs on a plain in-memory mailbox, so its messages only need to be actor
// messages, not TLV-serializable: admission, resume routing, terminal reaping,
// and listing all happen in-process. Only ResumeCreditOpRequest also crosses a
// per-operation child's durable mailbox, so it alone additionally satisfies
// CreditDurableMsg.
type CreditMsg interface {
	actor.Message

	creditMsgSealed()
}

// CreditDurableMsg is the message constraint for a per-operation child's
// durable mailbox. It embeds actor.TLVMessage so both ResumeCreditOpRequest and
// the framework-injected RestartMessage survive a restart. The supervisor
// pre-writes the control-plane row before spawning a child, so resume is the
// child's only application-level inbound message; everything else the child
// needs it reloads from its durable row.
type CreditDurableMsg interface {
	actor.TLVMessage
}

// CreditResp is a sealed interface for responses produced by credit actors.
type CreditResp interface {
	actor.Message

	creditRespSealed()
}

// ResumeCreditOpRequestTLVType identifies a ResumeCreditOpRequest, the only
// application message that crosses a durable child mailbox. The 0x71xx range
// avoids collisions with the actor framework reserved types (0xFFFE/0xFFFF),
// the OOR range (0x70xx), and the ledger range (0x90xx).
const ResumeCreditOpRequestTLVType tlv.Type = 0x7102

// tlv record types for the ResumeCreditOpRequest payload. These are LOCAL to
// the message payload stream, not global mailbox TLV types.
const (
	recOpID      tlv.Type = 2
	recFromTimer tlv.Type = 10
)

// StartCreditPayRequest asks the credit supervisor to admit and drive a credit
// or mixed pay (with an optional Ark top-up) for one BOLT-11 invoice.
type StartCreditPayRequest struct {
	actor.BaseMessage

	// OpKey is the stable idempotency key (pay:<payment_hash_hex>) reused
	// for the server credit op AND the delegated OOR transfer.
	OpKey string

	// Invoice is the target BOLT-11 invoice.
	Invoice string

	// PaymentHash is the invoice payment hash.
	PaymentHash [32]byte

	// AmountSat is the invoice amount in satoshis.
	AmountSat uint64

	// TopupSat is the Ark top-up amount required to cover the credit
	// shortfall (0 when the account already has enough credits).
	TopupSat uint64

	// MaxCreditSat is the credit cap passed to StartPay.
	MaxCreditSat uint64

	// MaxFeeSat is the caller's max routing fee.
	MaxFeeSat uint64

	// CreditOnly marks a pay that settles entirely from credit with no
	// Lightning swap leg. The wallet's credit-op projector owns the
	// terminal WalletEntry transition for such pays, since no swap session
	// observes them. A mixed pay leaves this false so the swap monitor
	// stays the single terminal authority for the shared payment-hash row.
	CreditOnly bool
}

// MessageType returns the human-readable message type.
func (m *StartCreditPayRequest) MessageType() string {
	return "StartCreditPayRequest"
}

func (m *StartCreditPayRequest) creditMsgSealed() {}

// StartCreditReceiveRequest asks the credit supervisor to admit and drive a
// server-owned Lightning receive that credits the account.
type StartCreditReceiveRequest struct {
	actor.BaseMessage

	// OpKey is the stable idempotency key (recv:<payment_hash_hex>).
	OpKey string

	// AmountSat is the requested receive amount in satoshis.
	AmountSat uint64

	// Memo is embedded in the server-owned invoice.
	Memo string
}

// MessageType returns the human-readable message type.
func (m *StartCreditReceiveRequest) MessageType() string {
	return "StartCreditReceiveRequest"
}

func (m *StartCreditReceiveRequest) creditMsgSealed() {}

// RedeemRequest asks the credit supervisor to admit and drive a redemption of
// available credits back into a wallet-owned Ark vTXO. It is issued by the
// wallet-owned auto-redeem policy, never by an external user.
type RedeemRequest struct {
	actor.BaseMessage

	// OpKey is the stable idempotency key (redeem:<client_redeem_id>).
	OpKey string

	// AmountSat is the amount of available credit to redeem.
	AmountSat uint64
}

// MessageType returns the human-readable message type.
func (m *RedeemRequest) MessageType() string {
	return "RedeemRequest"
}

func (m *RedeemRequest) creditMsgSealed() {}

// ResumeCreditOpRequest asks the supervisor (or a per-operation child) to
// re-drive the work implied by the current persisted state. It is issued by the
// boot-time restore and by retry-timer expiries. It is the only application
// message that crosses a durable child mailbox, so it is the only one that is
// TLV-serializable.
type ResumeCreditOpRequest struct {
	actor.BaseMessage

	// OpID identifies the operation to resume.
	OpID string

	// FromRetryTimer is true when a fired retry timer drove this resume,
	// and false when a boot restore did. Only a timer expiry advances the
	// retry attempt counter.
	FromRetryTimer bool
}

// MessageType returns the human-readable message type.
func (m *ResumeCreditOpRequest) MessageType() string {
	return "ResumeCreditOpRequest"
}

func (m *ResumeCreditOpRequest) creditMsgSealed() {}

// TLVType returns the unique TLV type identifier.
func (m *ResumeCreditOpRequest) TLVType() tlv.Type {
	return ResumeCreditOpRequestTLVType
}

// Encode serializes the message.
func (m *ResumeCreditOpRequest) Encode(w io.Writer) error {
	if m == nil {
		return fmt.Errorf("resume credit op request must be provided")
	}

	opID := []byte(m.OpID)
	var fromTimer uint8
	if m.FromRetryTimer {
		fromTimer = 1
	}

	return encodeRecords(
		w, tlv.MakePrimitiveRecord(recOpID, &opID),
		tlv.MakePrimitiveRecord(recFromTimer, &fromTimer),
	)
}

// Decode deserializes the message.
func (m *ResumeCreditOpRequest) Decode(r io.Reader) error {
	var (
		opID      []byte
		fromTimer uint8
	)

	if err := decodeRecords(
		r, tlv.MakePrimitiveRecord(recOpID, &opID),
		tlv.MakePrimitiveRecord(recFromTimer, &fromTimer),
	); err != nil {
		return err
	}

	m.OpID = string(opID)
	m.FromRetryTimer = fromTimer != 0

	return nil
}

// ConsiderRedeemRequest asks the supervisor to materialize available credits
// into a vTXO, now that a settled receive cleared the auto-redeem watermark (or
// the boot-time reconcile observed an over-watermark balance). The supervisor
// applies the no-pending-pay/redeem interlock before admitting a redeem
// operation, so the caller signals intent without owning the redeem decision.
// It crosses only the in-memory supervisor mailbox, so it is not
// TLV-serializable.
type ConsiderRedeemRequest struct {
	actor.BaseMessage

	// AvailableSat is the earmark-adjusted available balance the caller
	// observed, the upper bound on what may be redeemed.
	AvailableSat uint64
}

// MessageType returns the human-readable message type.
func (m *ConsiderRedeemRequest) MessageType() string {
	return "ConsiderRedeemRequest"
}

func (m *ConsiderRedeemRequest) creditMsgSealed() {}

// CreditTerminalNotification tells the supervisor that a per-operation child
// committed a terminal snapshot, so the supervisor can reap the child. The
// supervisor re-checks the durable row before reaping, so a stale or duplicate
// notification is harmless.
type CreditTerminalNotification struct {
	actor.BaseMessage

	// OpID identifies the operation that reached a terminal status.
	OpID string
}

// MessageType returns the human-readable message type.
func (m *CreditTerminalNotification) MessageType() string {
	return "CreditTerminalNotification"
}

func (m *CreditTerminalNotification) creditMsgSealed() {}

// ListCreditOpsRequest asks the supervisor for compact summaries of the credit
// operations it knows about.
type ListCreditOpsRequest struct {
	actor.BaseMessage

	// PendingOnly restricts the result to non-terminal operations.
	PendingOnly bool
}

// MessageType returns the human-readable message type.
func (m *ListCreditOpsRequest) MessageType() string {
	return "ListCreditOpsRequest"
}

func (m *ListCreditOpsRequest) creditMsgSealed() {}

// RestoreNonTerminalRequest asks the supervisor to respawn and resume every
// non-terminal operation from the control-plane store. Routing it through the
// supervisor mailbox serializes the restore with concurrent admissions.
type RestoreNonTerminalRequest struct {
	actor.BaseMessage
}

// MessageType returns the human-readable message type.
func (m *RestoreNonTerminalRequest) MessageType() string {
	return "RestoreNonTerminalRequest"
}

func (m *RestoreNonTerminalRequest) creditMsgSealed() {}

// StartCreditResponse returns the admitted operation id for a start request.
type StartCreditResponse struct {
	actor.BaseMessage

	// OpID is the admitted operation id.
	OpID string

	// Existing is true when the supervisor returned an already-known
	// operation instead of admitting a new one for this request.
	Existing bool

	// Invoice is the server-owned Lightning receive invoice. It is set for
	// a credit receive admission so the wallet can return it synchronously;
	// it is empty for pay and redeem admissions.
	Invoice string

	// PaymentHash is the receive invoice payment hash, when present.
	PaymentHash []byte
}

// MessageType returns the human-readable message type.
func (m *StartCreditResponse) MessageType() string {
	return "StartCreditResponse"
}

func (m *StartCreditResponse) creditRespSealed() {}

// CreditOpSummary is a compact status view of one credit operation.
type CreditOpSummary struct {
	// OpID is the stable operation id.
	OpID string

	// OpKey is the stable idempotency key.
	OpKey string

	// Kind is the operation family.
	Kind OpKind

	// State is the FSM state string.
	State State

	// Pending is true while the operation is non-terminal.
	Pending bool

	// AmountSat is the principal amount.
	AmountSat int64

	// CreditOnly marks a pay that settles entirely from credit with no
	// Lightning swap leg, so the wallet projector owns its terminal
	// WalletEntry transition. Always false for receive and redeem ops.
	CreditOnly bool

	// LastError is the terminal failure reason, when failed.
	LastError string
}

// ListCreditOpsResponse returns credit operation summaries.
type ListCreditOpsResponse struct {
	actor.BaseMessage

	// Ops contains matching credit operation summaries.
	Ops []CreditOpSummary
}

// MessageType returns the human-readable message type.
func (m *ListCreditOpsResponse) MessageType() string {
	return "ListCreditOpsResponse"
}

func (m *ListCreditOpsResponse) creditRespSealed() {}

// AckResponse acknowledges a fire-and-forget control message (resume, terminal
// notification) that has no richer reply.
type AckResponse struct {
	actor.BaseMessage
}

// MessageType returns the human-readable message type.
func (m *AckResponse) MessageType() string {
	return "AckResponse"
}

func (m *AckResponse) creditRespSealed() {}

// encodeRecords writes a tlv stream of the provided records to w.
func encodeRecords(w io.Writer, records ...tlv.Record) error {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// decodeRecords reads a tlv stream of the provided records from r.
func decodeRecords(r io.Reader, records ...tlv.Record) error {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Decode(r)
}

// NewCodec builds the message codec for a per-operation child's durable
// mailbox. Only ResumeCreditOpRequest crosses that mailbox at the application
// level; the supervisor pre-writes the durable row before spawning a child, so
// every other admission detail is reloaded from the row rather than redelivered
// as a message.
func NewCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(
		ResumeCreditOpRequestTLVType,
		func() actor.TLVMessage {
			return &ResumeCreditOpRequest{}
		},
	)

	// The framework injects RestartMessage as the first message after a
	// restart, and delivers DurableAsk responses as AskResponse.
	codec.MustRegister(actor.RestartTLVType, func() actor.TLVMessage {
		return &actor.RestartMessage{}
	})
	codec.MustRegister(actor.AskResponseMsgType, func() actor.TLVMessage {
		return &actor.AskResponse{}
	})

	return codec
}
