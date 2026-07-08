package walletdk

import (
	"net/http"
	"time"

	"google.golang.org/grpc"
)

// ConnectConfig controls a walletdk client connected to an external daemon.
type ConnectConfig struct {
	// Address is the target of a daemon exposing walletdkrpc. For gRPC
	// transport this is the gRPC address; for REST transport this is the
	// HTTP gateway base address.
	Address string

	// Transport selects how Connect talks to the daemon. Empty defaults to
	// TransportGRPC.
	Transport Transport

	// TLSCertPath is an optional daemon TLS certificate path. When empty,
	// walletdk uses system roots unless Insecure is set.
	TLSCertPath string

	// MacaroonPath is an optional daemon RPC macaroon path.
	MacaroonPath string

	// Insecure disables TLS for local development or injected listeners.
	Insecure bool

	// DialOptions are appended to the transport and auth dial options.
	// Only used with TransportGRPC.
	DialOptions []grpc.DialOption

	// HTTPClient is the HTTP client used with TransportREST. Nil uses
	// http.DefaultClient, or a TLS-cert-specific client when TLSCertPath
	// is set.
	HTTPClient *http.Client
}

// Transport selects the RPC transport used by Connect.
type Transport string

const (
	// TransportGRPC connects to the daemon with native gRPC.
	TransportGRPC Transport = "grpc"

	// TransportREST connects to the daemon through grpc-gateway HTTP/JSON.
	TransportREST Transport = "rest"
)

// WalletState mirrors the daemon's wallet lifecycle enum so SDK
// consumers can render wallet setup progress without collapsing
// LOCKED and SYNCING into one "not ready" state. WalletStateSyncing
// and WalletStateReady mean seed material is loaded; only
// WalletStateReady means the wallet is fully usable.
type WalletState int32

const (
	// WalletStateUnspecified is the proto3 zero value. The daemon
	// never emits this; reserved so a missing field deserializes to
	// a safe non-ready state.
	WalletStateUnspecified WalletState = 0

	// WalletStateNone indicates no wallet has been created yet.
	WalletStateNone WalletState = 1

	// WalletStateLocked indicates a wallet database exists but its
	// password has not been provided; signing is unavailable.
	WalletStateLocked WalletState = 2

	// WalletStateReady indicates the wallet is initialized, unlocked,
	// and signing is available.
	WalletStateReady WalletState = 3

	// WalletStateSyncing indicates the wallet is unlocked and the
	// backing chain source is catching up before wallet RPCs are safe.
	WalletStateSyncing WalletState = 4
)

// Info summarizes daemon readiness for wallet applications.
type Info struct {
	Version         string
	Commit          string
	Network         string
	BlockHeight     uint32
	ServerConnected bool
	WalletType      string
	WalletState     WalletState
	IdentityPubKey  string
}

// WalletReady reports whether the daemon wallet is fully unlocked and
// ready to sign. Convenience predicate over WalletState so callers that
// only need the binary state don't have to import the enum.
func (i *Info) WalletReady() bool {
	if i == nil {
		return false
	}

	return i.WalletState == WalletStateReady
}

// CreateWalletRequest creates or imports a daemon wallet.
type CreateWalletRequest struct {
	Mnemonic       []string
	SeedPassphrase []byte
	WalletPassword []byte
	RecoverState   bool
	RecoveryWindow uint32
}

// CreateWalletResult returns the seed words, daemon identity, and optional
// recovery counters.
type CreateWalletResult struct {
	Mnemonic                    []string
	EncipheredSeed              []byte
	IdentityPubKey              string
	RecoveryRan                 bool
	RecoveredBoardingAddresses  uint32
	RecoveredBoardingUTXOs      uint32
	RecoveredVTXOs              uint32
	RecoveredOORReceiveScripts  uint32
	RecoveredOORRecipientEvents uint32
}

// UnlockWalletRequest unlocks an existing embedded daemon wallet.
type UnlockWalletRequest struct {
	WalletPassword []byte
}

// UnlockWalletResult returns the daemon identity after unlock.
type UnlockWalletResult struct {
	IdentityPubKey string
}

// OpenWalletResult reports the outcome of OpenWalletFromPasskey. Imported is
// true when a new local wallet was created from the derived seed (fresh
// device); false when an existing local wallet was unlocked. Mnemonic is set
// only on import, for backup display.
type OpenWalletResult struct {
	Imported       bool
	Mnemonic       []string
	IdentityPubKey string
}

// Balance is the wallet-level balance view.
type Balance struct {
	ConfirmedSat       int64
	PendingInSat       int64
	PendingOutSat      int64
	CreditAvailableSat uint64
	CreditReservedSat  uint64
}

// DepositRequest creates a tracked boarding address.
type DepositRequest struct {
	AmountSatHint uint64
}

// DepositResult returns a boarding address and its initial activity entry.
type DepositResult struct {
	Address string
	Entry   Entry
}

// ReceiveRequest creates a Lightning invoice payable into the wallet.
type ReceiveRequest struct {
	AmountSat uint64
	Memo      string
}

// ReceiveResult contains the invoice and initial wallet entry.
type ReceiveResult struct {
	Invoice string
	Entry   Entry
}

// PrepareSendRequest validates and previews an outbound payment without
// moving funds.
type PrepareSendRequest struct {
	Invoice        string
	OnchainAddress string
	AmountSat      uint64
	Note           string
	MaxFeeSat      uint64

	// SweepAll drains every live VTXO to OnchainAddress. PrepareSend
	// snapshots the live VTXO set and SendPrepared later spends that
	// exact set. Ignored on the invoice path.
	SweepAll bool
}

// SendRail identifies the expected settlement rail for a prepared send.
type SendRail string

const (
	SendRailUnspecified     SendRail = "unspecified"
	SendRailOffchainUnknown SendRail = "offchain_unknown"
	SendRailInArk           SendRail = "in_ark"
	SendRailLightning       SendRail = "lightning"
	SendRailOnchain         SendRail = "onchain"
	SendRailCredit          SendRail = "credit"
	SendRailMixed           SendRail = "mixed"
)

// SendQuoteStatus describes how complete the prepare-time quote is.
type SendQuoteStatus string

const (
	SendQuoteStatusUnspecified SendQuoteStatus = "unspecified"
	SendQuoteStatusComplete    SendQuoteStatus = "complete"
	SendQuoteStatusLocalOnly   SendQuoteStatus = "local_only"
)

// PrepareSendResult contains the preview and intent id for a send.
type PrepareSendResult struct {
	SendIntentID            string
	AmountSat               int64
	ExpectedFeeSat          int64
	FeeKnown                bool
	ExpectedTotalOutflowSat int64
	TotalOutflowKnown       bool
	Rail                    SendRail
	QuoteStatus             SendQuoteStatus
	DestinationSummary      string
	InvoiceDescription      string
	PaymentHash             string
	ExpiresAtUnix           int64
	SelectedOutpoints       []string
	Warning                 string
	CreditPreview           *CreditPreview
}

// CreditPreview describes how a prepared invoice send will use sat-native
// server credits.
type CreditPreview struct {
	MustUseCredit      bool
	CreditAppliedSat   uint64
	CreditShortfallSat uint64
	CreditTopupSat     uint64
	ArkFundingSat      uint64
}

// SendPreparedRequest dispatches a prepared outbound payment.
type SendPreparedRequest struct {
	// SendIntentID is consumed before dispatch. If dispatch returns an
	// error, callers should prepare a fresh send before retrying.
	SendIntentID string
}

// SendResult contains the initial wallet entry for an outbound payment.
type SendResult struct {
	Entry Entry

	// ActualAmountSat is the real amount that will leave the wallet for
	// this operation. For invoice sends it matches the invoice principal.
	// For a bounded onchain send it matches the requested amount (the
	// seal-time fee handshake returns change). For a sweep-all onchain
	// send it reflects the swept VTXO total, so host UIs SHOULD echo it
	// back to the user before treating the send as confirmed.
	ActualAmountSat int64
}

// ListView selects which slice of wallet state List returns. The empty
// value is treated as ListViewActivity for backwards-feel.
type ListView string

const (
	// ListViewActivity returns the merged WalletEntry stream
	// (send / recv / deposit / exit). Default.
	ListViewActivity ListView = "activity"

	// ListViewVTXOs returns the live VTXO inventory.
	ListViewVTXOs ListView = "vtxos"

	// ListViewOnchain returns the on-chain transaction history
	// (boarding, sweeps, leave outputs).
	ListViewOnchain ListView = "onchain"
)

// ListRequest controls wallet activity listing. View selects the slice
// of wallet state to return; PendingOnly and Kinds apply only when
// View is ListViewActivity (or empty).
type ListRequest struct {
	// View selects the response shape. Empty is treated as
	// ListViewActivity.
	View ListView

	// PendingOnly applies to ListViewActivity only.
	PendingOnly bool

	// Kinds applies to ListViewActivity only.
	Kinds []EntryKind

	// Limit caps the page size; zero uses the daemon default.
	Limit uint32

	// Offset is the pagination offset. It applies to the VTXOs and
	// Onchain views; the Activity view paginates by Cursor and ignores
	// Offset.
	Offset uint32

	// Cursor is the opaque pagination token for the Activity view. Empty
	// starts from the newest entry; otherwise pass the NextCursor returned
	// by the previous ActivityList page.
	Cursor string
}

// ListResult is a tagged union: exactly one of Activity, VTXOs, or
// Onchain is populated according to the view requested. Callers should
// switch on View to pick the right field.
type ListResult struct {
	// View is the populated variant. Mirrors ListRequest.View; an
	// empty request view is reported as ListViewActivity.
	View ListView

	// Activity is populated when View == ListViewActivity.
	Activity *ActivityList

	// VTXOs is populated when View == ListViewVTXOs.
	VTXOs *VTXOInventory

	// Onchain is populated when View == ListViewOnchain.
	Onchain *OnchainHistory
}

// ActivityList is the merged WalletEntry stream returned by the
// activity view.
type ActivityList struct {
	Entries []Entry

	// Total is the number of entries on this page, not a full-feed count:
	// the feed is cursor-paged, so use HasMore to decide whether to fetch
	// again.
	Total uint32

	// HasMore reports whether more entries exist after this page.
	HasMore bool

	// NextCursor is the token to pass as ListRequest.Cursor to fetch the
	// next page. Empty when HasMore is false.
	NextCursor string
}

// VTXOInventory is the live VTXO inventory returned by the vtxos view.
type VTXOInventory struct {
	VTXOs []WalletVTXO
	Total uint32
}

// WalletVTXO is the wallet-facing view of one VTXO. Internal lifecycle
// detail (forfeiting flow, chain depth) is hidden; power-users reach
// the full shape via `ark vtxos list`.
type WalletVTXO struct {
	Outpoint       string
	AmountSat      int64
	Status         string
	BatchExpiry    int32
	RelativeExpiry uint32
	CommitmentTxid string
}

// OnchainHistory is the on-chain transaction history returned by the
// onchain view.
type OnchainHistory struct {
	Txs     []OnchainTx
	Total   uint32
	HasMore bool
}

// OnchainTx is the wallet-facing view of one on-chain transaction.
type OnchainTx struct {
	Txid               string
	Kind               string
	AmountSat          int64
	FeeSat             int64
	Status             string
	ConfirmationHeight int32
	CreatedAt          time.Time
	Description        string
}

// ExitRequest triggers an exit for a VTXO outpoint. When Destination is
// set, the SDK first attempts a cooperative leave (LeaveVTXOs RPC) with
// the leave output bound for the supplied on-chain address; if that path
// succeeds, the SDK returns a cooperative result. Unilateral unroll is
// reachable only when ForceUnrollAck carries the daemon's exact
// acknowledgement string.
type ExitRequest struct {
	// Outpoint identifies the VTXO to exit in "txid:index" format.
	Outpoint string

	// Destination is the on-chain address that receives the leave
	// output when the cooperative path succeeds. The address must be
	// valid for the daemon's configured network. Empty asks the daemon
	// to generate a fresh backing-wallet destination internally.
	Destination string

	// ForceUnrollAck must be exactly "I_KNOW_WHAT_I_AM_DOING" to bypass
	// cooperative leave and start unilateral unroll. Cannot be combined
	// with Destination; the server rejects the pair with InvalidArgument.
	ForceUnrollAck string
}

// ExitPath identifies which branch of the Exit decision tree the
// daemon ended up taking. Callers should switch on Path rather than
// chaining nil-checks across the result's variant fields.
type ExitPath string

const (
	// ExitPathCooperative means the cooperative leave was admitted by
	// the operator; QueuedOutpoints carries the round's selection
	// echo. Cooperative round completion is asynchronous; subscribe
	// via walletdkrpc.SubscribeWallet to confirm terminal state.
	ExitPathCooperative ExitPath = "cooperative"

	// ExitPathUnilateral means the caller supplied the exact force
	// acknowledgement and the daemon started unilateral unroll. Created
	// and ActorID describe the unilateral unroll job.
	ExitPathUnilateral ExitPath = "unilateral"

	// ExitPathUnilateralFallback is retained for source compatibility
	// with the prior SDK result shape. New forced unrolls return
	// ExitPathUnilateral; current wallet RPC behavior never returns this
	// path because cooperative failures are surfaced directly.
	ExitPathUnilateralFallback ExitPath = "unilateral_fallback"
)

// ExitResult is a tagged union over the three exit paths. Callers
// should read Path first and only inspect the variant fields
// associated with that path; the remaining fields are zero-valued.
type ExitResult struct {
	// Path discriminates between the three legal outcomes; callers
	// MUST switch on Path before reading the variant fields below.
	Path ExitPath

	// Cooperative is true iff Path == ExitPathCooperative. Retained
	// for backwards compatibility with the v1 result shape; new
	// callers should prefer Path.
	Cooperative bool

	// QueuedOutpoints lists the outpoints the cooperative leave
	// admitted into a round. Populated only when
	// Path == ExitPathCooperative.
	QueuedOutpoints []string

	// Created reports whether the unilateral-unroll path spawned a
	// fresh job. Populated when Path is ExitPathUnilateral or
	// ExitPathUnilateralFallback.
	Created bool

	// ActorID identifies the durable unroll job that owns the
	// unilateral path. Populated when Path is ExitPathUnilateral or
	// ExitPathUnilateralFallback.
	ActorID string

	// CooperativeError is retained for source compatibility with the
	// prior SDK fallback result shape. Current wallet RPC behavior never
	// populates it because cooperative failures are returned directly
	// instead of falling back to unilateral unroll.
	CooperativeError string
}

// ExitStatusRequest queries the current phase of an exit job.
type ExitStatusRequest struct {
	Outpoint string
}

// ExitJobStatus collapses the underlying unroll job phases to a short
// wallet-facing string set.
type ExitJobStatus string

const (
	ExitJobStatusUnspecified   ExitJobStatus = "unspecified"
	ExitJobStatusPending       ExitJobStatus = "pending"
	ExitJobStatusMaterializing ExitJobStatus = "materializing"
	ExitJobStatusCSVPending    ExitJobStatus = "csv_pending"
	ExitJobStatusSweeping      ExitJobStatus = "sweeping"
	ExitJobStatusCompleted     ExitJobStatus = "completed"
	ExitJobStatusFailed        ExitJobStatus = "failed"
)

// ExitStatusResult reports the status of one exit job. Found is false
// when no job exists for the requested outpoint (not an error).
type ExitStatusResult struct {
	Found     bool
	Status    ExitJobStatus
	SweepTxid string
	LastError string
}

// GetExitPlanRequest previews unilateral-exit readiness for a slice of VTXOs.
type GetExitPlanRequest struct {
	Outpoints  []string
	ConfTarget uint32
}

// ExitPlanEntry describes how to fund the backing wallet before Exit for a
// single previewed VTXO outpoint.
type ExitPlanEntry struct {
	Outpoint                   string
	FundingAddress             string
	RequiredConfirmations      uint32
	RequiredFeeUTXOCount       uint32
	UsableFeeUTXOCount         uint32
	RecommendedUTXOAmountSat   int64
	RecommendedTotalFundingSat int64
	FundingShortfallSat        int64
	CanStart                   bool
	ExitJobFound               bool
	ExitStatus                 ExitJobStatus
	SweepTxid                  string
	LastError                  string

	// Err is a per-outpoint failure (empty on success).
	Err string
}

// GetExitPlanResult describes the combined backing-wallet funding plan for
// every previewed outpoint plus aggregate totals.
type GetExitPlanResult struct {
	Plans                      []ExitPlanEntry
	FeeRateSatPerVByte         int64
	CanStart                   bool
	TotalFundingShortfallSat   int64
	TotalRecommendedFundingSat int64
}

// SweepWalletRequest previews or broadcasts a backing-wallet sweep.
type SweepWalletRequest struct {
	DestinationAddress string
	Broadcast          bool
	FeeRateSatPerVByte int64
	ConfTarget         uint32
}

// WalletSweepInput describes one backing-wallet UTXO selected by SweepWallet.
type WalletSweepInput struct {
	Outpoint  string
	AmountSat int64
}

// SweepWalletResult contains the selected inputs and optional broadcast txid.
type SweepWalletResult struct {
	Inputs             []WalletSweepInput
	TotalInputSat      int64
	EstimatedFeeSat    int64
	NetAmountSat       int64
	FeeRateSatPerVByte int64
	CanBroadcast       bool
	Txid               string
	FailureReason      string
}

// Status summarizes wallet readiness and pending activity.
type Status struct {
	Ready        bool
	Unlocked     bool
	Network      string
	Balance      Balance
	PendingCount uint32
}

// SubscribeRequest controls wallet activity subscriptions.
type SubscribeRequest struct {
	IncludeExisting bool
	Kinds           []EntryKind

	// Cursor resumes the stream after a prior Entry.Cursor (or a
	// *SubscribeGapError.Cursor): the daemon replays every activity event
	// after it, then streams live. Zero replays the full history when
	// IncludeExisting is set, or streams live-only otherwise.
	Cursor int64
}

// EntryKind is the user-visible wallet activity category.
type EntryKind string

const (
	// EntryKindSend is an outbound wallet payment.
	EntryKindSend EntryKind = "send"

	// EntryKindReceive is an inbound Lightning-to-wallet receive.
	EntryKindReceive EntryKind = "receive"

	// EntryKindDeposit is a boarding on-chain deposit.
	EntryKindDeposit EntryKind = "deposit"

	// EntryKindExit is a cooperative wallet-to-on-chain exit.
	EntryKindExit EntryKind = "exit"
)

// EntryStatus is the collapsed wallet activity state.
type EntryStatus string

const (
	// EntryStatusPending means the activity is still in flight.
	EntryStatusPending EntryStatus = "pending"

	// EntryStatusComplete means the activity finished successfully.
	EntryStatusComplete EntryStatus = "complete"

	// EntryStatusFailed means the activity reached a terminal failure.
	EntryStatusFailed EntryStatus = "failed"
)

// Entry is the wallet-facing activity row used by UI and bridge layers.
type Entry struct {
	ID            string
	Kind          EntryKind
	Status        EntryStatus
	AmountSat     int64
	FeeSat        int64
	Counterparty  string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Note          string
	FailureReason string

	// Cursor is the monotonic event-log position of this update on a
	// SubscribeWallet stream. Persist it and pass it back as
	// SubscribeRequest.Cursor to resume without gaps. It is zero outside
	// the subscription path (List / Send / Recv / Deposit results).
	Cursor int64

	// Progress carries the lifecycle metadata the daemon already
	// normalized for this entry (phase, payment hash, txid, confirmation
	// height, vHTLC outpoint). It is nil when the backing subsystem
	// supplied no progress hint.
	Progress *EntryProgress

	// Request echoes the user-recognizable request that created the entry
	// (a Lightning invoice, an on-chain address, or an Ark address). It is
	// nil when the backing subsystem did not persist one.
	Request *EntryRequest

	// FailureCode is a stable, machine-readable classification of why the
	// entry failed. It is empty unless Status is failed, mirroring
	// FailureReason, which remains the human-readable supplement.
	FailureCode EntryFailureCode
}

// EntryPhase is a coarse, wrapper-owned lifecycle phase for an Entry. It does
// not replace Status: Status answers pending/complete/failed, while Phase
// explains the current backing-system step. Like the other Entry enums it is
// a lowercase string decoupled from the proto enum so renumbering cannot break
// callers; switch on it rather than comparing proto values.
type EntryPhase string

const (
	// EntryPhaseUnspecified means the backing subsystem provided no
	// lifecycle hint.
	EntryPhaseUnspecified EntryPhase = "unspecified"

	// EntryPhaseRequestCreated means the request was created but no payment
	// has been observed yet.
	EntryPhaseRequestCreated EntryPhase = "request_created"

	// EntryPhaseWaitingForPayment means the wallet is waiting for an
	// inbound payment or swap funding.
	EntryPhaseWaitingForPayment EntryPhase = "waiting_for_payment"

	// EntryPhasePaymentDetected means a payment was detected but is not yet
	// settled.
	EntryPhasePaymentDetected EntryPhase = "payment_detected"

	// EntryPhaseSettling means the operation is settling through Ark,
	// Lightning, or on-chain machinery.
	EntryPhaseSettling EntryPhase = "settling"

	// EntryPhaseConfirmed means the backing operation is confirmed or
	// otherwise durably complete.
	EntryPhaseConfirmed EntryPhase = "confirmed"

	// EntryPhaseRefunding means the operation is currently refunding.
	EntryPhaseRefunding EntryPhase = "refunding"

	// EntryPhaseRefunded means the refund path completed.
	EntryPhaseRefunded EntryPhase = "refunded"

	// EntryPhaseFailed means the backing operation reached a terminal
	// failed state.
	EntryPhaseFailed EntryPhase = "failed"

	// EntryPhaseWaitingForConfirmation means an on-chain payment was
	// detected and is waiting for block confirmation.
	EntryPhaseWaitingForConfirmation EntryPhase = "waiting_for_confirmation"
)

// EntryProgress is the wrapper-owned view of the lifecycle metadata the daemon
// computes for an Entry. Fields are populated on a best-effort basis by the
// backing subsystem; an empty field means "not applicable / not yet known".
type EntryProgress struct {
	// Phase is the coarse lifecycle phase for the entry.
	Phase EntryPhase

	// PhaseLabel is the short lowercase label the daemon emitted; clients
	// may render it directly instead of switching on Phase.
	PhaseLabel string

	// PaymentHash is populated for Lightning-backed send/recv entries.
	PaymentHash string

	// Txid is populated when the backing ledger row has an on-chain txid.
	Txid string

	// ConfirmationHeight is populated once the source records it.
	ConfirmationHeight int32

	// VTXOOutpoint is populated when a swap observes the Ark vHTLC output.
	VTXOOutpoint string

	// Preimage is the hex-encoded Lightning payment preimage once the swap
	// revealed it. For a completed Lightning-backed send this is the proof
	// of payment for the paid invoice (sha256(preimage) == PaymentHash); it
	// is empty until durably known and for non-Lightning entries.
	Preimage string
}

// EntryRequestType discriminates which request shape an EntryRequest carries.
// Callers should switch on Type before reading the variant fields.
type EntryRequestType string

const (
	// EntryRequestTypeLightning marks a Lightning send/recv request; the
	// LightningInvoice and PaymentHash fields are populated.
	EntryRequestTypeLightning EntryRequestType = "lightning"

	// EntryRequestTypeOnchain marks a deposit/exit request; the
	// OnchainAddress field is populated.
	EntryRequestTypeOnchain EntryRequestType = "onchain"

	// EntryRequestTypeArk marks a direct Ark send/recv request; the
	// ArkAddress field is populated.
	EntryRequestTypeArk EntryRequestType = "ark"
)

// EntryRequest is the wrapper-owned, flattened view of the proto request
// oneof. Exactly one variant's fields are populated, named by Type; read Type
// first and treat the other fields as zero.
type EntryRequest struct {
	// Type names the populated variant.
	Type EntryRequestType

	// LightningInvoice is the BOLT-11 payment request. Populated when Type
	// is EntryRequestTypeLightning.
	LightningInvoice string

	// PaymentHash identifies the Lightning invoice and stays stable after
	// the invoice is no longer convenient to display. Populated when Type
	// is EntryRequestTypeLightning.
	PaymentHash string

	// OnchainAddress is the bech32 on-chain address originally issued or
	// targeted. Populated when Type is EntryRequestTypeOnchain.
	OnchainAddress string

	// ArkAddress is the Ark address originally issued or targeted.
	// Populated when Type is EntryRequestTypeArk.
	ArkAddress string
}

// EntryFailureCode is a wrapper-owned, stable classification of why a failed
// Entry failed. Like the other Entry enums it is a lowercase string decoupled
// from the proto enum; switch on it rather than comparing proto values.
type EntryFailureCode string

const (
	// EntryFailureCodeTimedOut means the operation exceeded the wallet
	// deadline before reaching a terminal state.
	EntryFailureCodeTimedOut EntryFailureCode = "timed_out"

	// EntryFailureCodeExpired means the swap expired before it was funded.
	EntryFailureCodeExpired EntryFailureCode = "expired"

	// EntryFailureCodeRefunded means an outbound payment was refunded back
	// to the wallet.
	EntryFailureCodeRefunded EntryFailureCode = "refunded"

	// EntryFailureCodeNeedsIntervention means the swap reached an anomalous
	// state requiring manual recovery.
	EntryFailureCodeNeedsIntervention EntryFailureCode = "needs_intervention" //nolint:ll

	// EntryFailureCodeFailed is a generic terminal failure with no more
	// specific classification.
	EntryFailureCodeFailed EntryFailureCode = "failed"
)
