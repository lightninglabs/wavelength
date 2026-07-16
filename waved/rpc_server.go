package waved

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/btcwbackend"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	arktx "github.com/lightninglabs/wavelength/lib/tx"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/lwwallet"
	"github.com/lightninglabs/wavelength/metrics"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/unrollplan"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// manualUnrollAdmissionTimeout bounds the local daemon work needed
	// to accept a manual unroll. The context is detached from the RPC
	// stream so a CLI disconnect does not cancel the unilateral-exit
	// handoff.
	manualUnrollAdmissionTimeout = 30 * time.Second

	// submittedOORCleanupTimeout bounds the post-RPC cleanup waiter
	// used after a detached OOR actor submit has been accepted but the
	// caller stopped waiting for the response. The wait should normally
	// end when the actor future completes or daemon shutdown fails
	// pending futures; this ceiling prevents a pathological actor stall
	// from retaining wallet/custom input reservations forever.
	submittedOORCleanupTimeout = 10 * time.Minute

	// submittedOORUnlockTimeout bounds the fresh context used to unlock
	// wallet-selected VTXOs when the detached OOR cleanup waiter itself
	// timed out. The cleanupCtx is deliberately expired in that branch, so
	// the unlock must run on a new bounded context to avoid the wallet
	// mailbox rejecting an already-expired Tell.
	submittedOORUnlockTimeout = 30 * time.Second

	// maxOORRecipients mirrors the in-round send cap at the daemon
	// boundary. The OOR actor has its own package-size limits, but the
	// RPC handler resolves scripts and policy templates before handing
	// work to that actor, so it needs its own cheap request-size guard.
	maxOORRecipients = 256
)

// RPCServer implements the daemon's gRPC DaemonService interface.
type RPCServer struct {
	waverpc.UnimplementedDaemonServiceServer

	server *Server

	// customInputLocksMu guards customInputLocks. Together they form
	// a lightweight in-memory mutex on custom OOR input outpoints
	// currently being processed by concurrent SendOOR calls. Without
	// this gate two concurrent callers supplying the same custom
	// input (e.g. the same vHTLC claim outpoint) could each succeed
	// through BuildCustomTransferInputs and end up double-signing
	// the same input. Standard wallet-managed VTXOs are locked via
	// the VTXO manager's reservation flow; custom inputs live
	// outside that managed set, so we dedup them here.
	customInputLocksMu sync.Mutex

	// customInputLocks is the set of custom OOR input outpoints
	// currently reserved by an in-flight SendOOR call.
	customInputLocks map[wire.OutPoint]struct{}

	// oorSignerOverride lets focused RPC tests exercise custom-input
	// signing without booting a full wallet backend.
	oorSignerOverride input.Signer
}

// NewRPCServer creates a new RPCServer backed by the given Server.
func NewRPCServer(server *Server) *RPCServer {
	return &RPCServer{
		server:           server,
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}
}

// SubLogger returns the daemon logger registered for a subsystem tag. Optional
// RPC subservers use this accessor to share waved's log manager without
// depending on the private Server type.
func (r *RPCServer) SubLogger(tag string) btclog.Logger {
	if r == nil || r.server == nil {
		return btclog.Disabled
	}

	return r.server.subLogger(tag)
}

// SignMailboxAuth returns a hex-encoded Schnorr mailbox auth signature for
// the daemon identity key bound to the recipient mailbox ID. Optional
// subservers use this to authenticate mailbox RPCs without learning how the
// daemon wallet backend stores or signs with the identity key.
func (r *RPCServer) SignMailboxAuth(ctx context.Context,
	recipientMailboxID string) (string, error) {

	if r == nil || r.server == nil {
		return "", fmt.Errorf("daemon server unavailable")
	}
	if r.server.clientKeyDesc.PubKey == nil {
		return "", fmt.Errorf("identity key not yet derived; wallet " +
			"not ready")
	}

	sig, err := r.server.signMailboxAuth(ctx, recipientMailboxID)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(sig.Serialize()), nil
}

// OperatorPubKey fetches the current Ark operator pubkey directly from the
// server and refreshes the daemon's cached operator terms snapshot. Optional
// subservers that build new policy scripts should use this rather than
// DaemonService.GetInfo so operator-key rotations are observed before the
// policy is handed to the wallet or OOR layer.
func (r *RPCServer) OperatorPubKey(ctx context.Context) (*btcec.PublicKey,
	error) {

	if r == nil || r.server == nil {
		return nil, fmt.Errorf("daemon server unavailable")
	}

	return r.server.fetchCurrentOperatorPubKey(ctx)
}

// SetVTXOForfeitParticipantSigner installs the optional local signer used by
// custom VTXO refreshes whose ForfeitSigningContext selects
// FORFEIT_SIGNING_ROUTE_LOCAL_SIGNER.
func (r *RPCServer) SetVTXOForfeitParticipantSigner(
	signer vtxo.ForfeitParticipantSigner) error {

	if r == nil || r.server == nil {
		return fmt.Errorf("daemon server unavailable")
	}
	if r.server.forfeitSignatures == nil {
		return fmt.Errorf("forfeit signature broker is not initialized")
	}

	r.server.forfeitSignatures.setLocalSigner(signer)

	return nil
}

// vtxoAdmissionCode maps typed VTXO admission failures to caller-actionable
// gRPC codes while preserving Internal for unexpected selection failures.
func vtxoAdmissionCode(err error) codes.Code {
	switch {
	case errors.Is(err, context.Canceled):
		return codes.Canceled

	case errors.Is(err, context.DeadlineExceeded):
		return codes.DeadlineExceeded

	case errors.Is(err, vtxo.ErrVTXOLiquidityLocked):
		return codes.Aborted

	case errors.Is(err, vtxo.ErrInsufficientSpendableFunds):
		return codes.ResourceExhausted

	default:
		return codes.Internal
	}
}

// ClientTLSCerts returns the daemon identity client certificate used when
// optional subservers dial mTLS-protected mailbox edges.
func (r *RPCServer) ClientTLSCerts() ([]tls.Certificate, error) {
	if r == nil || r.server == nil {
		return nil, fmt.Errorf("daemon server unavailable")
	}

	if r.server.clientKeyDesc.PubKey == nil {
		return nil, fmt.Errorf("identity key not yet derived; wallet " +
			"not ready")
	}

	clientCert, err := serverconn.GenerateClientTLSCert(
		r.server.clientKeyDesc.PubKey,
	)
	if err != nil {
		return nil, fmt.Errorf("generate client TLS cert: %w", err)
	}

	return []tls.Certificate{clientCert}, nil
}

// reserveCustomInputs atomically claims every outpoint in the supplied
// slice. If any outpoint is already reserved by another in-flight call,
// no claims are taken and an error is returned. The returned release
// function reverses the claim and is safe to call on either success or
// failure paths (e.g. via defer).
func (r *RPCServer) reserveCustomInputs(outpoints []wire.OutPoint) (func(),
	error) {

	r.customInputLocksMu.Lock()
	defer r.customInputLocksMu.Unlock()

	// First pass: check for collisions so we don't partially reserve.
	for i := range outpoints {
		if _, taken := r.customInputLocks[outpoints[i]]; taken {
			return nil, fmt.Errorf("custom input %s already "+
				"reserved by another in-flight SendOOR",
				outpoints[i])
		}
	}

	// Second pass: commit the reservations.
	for i := range outpoints {
		r.customInputLocks[outpoints[i]] = struct{}{}
	}

	release := func() {
		r.customInputLocksMu.Lock()
		defer r.customInputLocksMu.Unlock()

		for i := range outpoints {
			delete(r.customInputLocks, outpoints[i])
		}
	}

	return release, nil
}

type oorChangeRecipientBuilder func(context.Context,
	btcutil.Amount) (oortx.RecipientOutput, error)

// sumOORInputAmounts returns the total value consumed by an OOR transfer.
func sumOORInputAmounts(inputs []oor.TransferInput) (btcutil.Amount, error) {
	var total btcutil.Amount
	for i := range inputs {
		input := inputs[i]
		if input.VTXO == nil {
			return 0, fmt.Errorf("input %d missing VTXO", i)
		}

		if input.VTXO.Amount <= 0 {
			return 0, fmt.Errorf("input %d amount must be positive",
				i)
		}

		total += input.VTXO.Amount
	}

	return total, nil
}

// appendOORChangeRecipient appends a wallet-owned change output when selected
// OOR inputs exceed the requested recipient amount. OOR v0 packages are
// fee-less, so the selected input sum must be paid out exactly.
func appendOORChangeRecipient(ctx context.Context,
	recipients []oortx.RecipientOutput,
	inputTotal, outputFloor btcutil.Amount,
	buildChange oorChangeRecipientBuilder) ([]oortx.RecipientOutput,
	btcutil.Amount, error) {

	if len(recipients) == 0 {
		return nil, 0, status.Errorf(codes.InvalidArgument, "OOR "+
			"recipient must be provided")
	}

	var targetAmt btcutil.Amount
	for i, recipient := range recipients {
		if recipient.Value <= 0 {
			return nil, 0, status.Errorf(codes.InvalidArgument,
				"recipient %d amount must be positive", i)
		}

		targetAmt += recipient.Value
	}

	if inputTotal < targetAmt {
		return nil, 0, status.Errorf(codes.InvalidArgument, "selected "+
			"input amount %d sat is below recipient amount %d sat",
			inputTotal, targetAmt)
	}

	out := append([]oortx.RecipientOutput(nil), recipients...)
	change := inputTotal - targetAmt
	if change == 0 {
		return out, 0, nil
	}

	if outputFloor > 0 && change < outputFloor {
		return nil, change, status.Errorf(codes.InvalidArgument, "OOR "+
			"change output %d sat is below VTXO minimum %d sat; "+
			"choose exact inputs or a larger amount", change,
			outputFloor)
	}

	if buildChange == nil {
		return nil, change, status.Errorf(codes.Internal, "OOR "+
			"change builder not configured")
	}

	changeRecipient, err := buildChange(ctx, change)
	if err != nil {
		return nil, change, err
	}

	if len(changeRecipient.PkScript) == 0 {
		return nil, change, status.Errorf(codes.Internal, "OOR "+
			"change script is empty")
	}

	if changeRecipient.Value != 0 && changeRecipient.Value != change {
		return nil, change, status.Errorf(codes.Internal, "OOR change "+
			"builder returned %d sat for %d sat change",
			changeRecipient.Value, change)
	}

	changeRecipient.Value = change
	out = append(out, changeRecipient)

	return out, change, nil
}

// validateOORRecipientFloor enforces the operator's output floor for
// caller-selected OOR recipients. Change outputs are checked later after input
// selection; this guard prevents creating a receiver VTXO that the operator
// would not accept in subsequent cooperative spends.
func validateOORRecipientFloor(amountSat int64,
	outputFloor btcutil.Amount) error {

	if outputFloor <= 0 {
		return nil
	}

	amount := btcutil.Amount(amountSat)
	if amount >= outputFloor {
		return nil
	}

	return status.Errorf(codes.InvalidArgument, "amount %d below operator "+
		"min_vtxo_amount_sat %d", amount, outputFloor)
}

// sendOORRequestRecipients returns the caller-requested OOR recipients in
// request order.
func sendOORRequestRecipients(req *waverpc.SendOORRequest) ([]*waverpc.Output,
	error) {

	if req == nil || len(req.GetRecipients()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"is required")
	}

	if len(req.GetRecipients()) > maxOORRecipients {
		return nil, status.Errorf(codes.InvalidArgument, "too many "+
			"recipients: %d (max %d)", len(req.GetRecipients()),
			maxOORRecipients)
	}

	return req.GetRecipients(), nil
}

// sumSendOORRecipientAmounts validates every requested recipient amount and
// returns the aggregate wallet-selection target.
func sumSendOORRecipientAmounts(recipients []*waverpc.Output) (btcutil.Amount,
	error) {

	var total int64
	for i, recipient := range recipients {
		if recipient == nil {
			return 0, status.Errorf(codes.InvalidArgument,
				"recipient %d is required", i)
		}

		if recipient.AmountSat <= 0 {
			return 0, status.Errorf(codes.InvalidArgument,
				"recipient %d amount must be positive", i)
		}

		if recipient.AmountSat > int64(btcutil.MaxSatoshi) {
			return 0, status.Errorf(codes.InvalidArgument,
				"recipient %d amount must be <= %d", i,
				int64(btcutil.MaxSatoshi))
		}

		if total > int64(btcutil.MaxSatoshi)-recipient.AmountSat {
			return 0, status.Errorf(codes.InvalidArgument, "total "+
				"recipient amount must be <= %d",
				int64(btcutil.MaxSatoshi))
		}

		total += recipient.AmountSat
	}

	return btcutil.Amount(total), nil
}

// indexedSendOORRecipientError keeps the original gRPC code while adding the
// recipient index to the user-facing error.
func indexedSendOORRecipientError(index int, err error) error {
	st := status.Convert(err)

	return status.Errorf(st.Code(), "recipient %d: %s", index, st.Message())
}

// buildSendOORRecipients resolves the daemon RPC recipient descriptors into
// the OOR actor's package-level recipient outputs.
func (r *RPCServer) buildSendOORRecipients(ctx context.Context,
	requestRecipients []*waverpc.Output, terms *types.OperatorTerms) (
	[]oortx.RecipientOutput, error) {

	seen := make(map[string]int, len(requestRecipients))
	recipients := make(
		[]oortx.RecipientOutput, 0, len(requestRecipients),
	)
	for i, out := range requestRecipients {
		pkScript, err := r.resolveOutputPkScript(ctx, out)
		if err != nil {
			return nil, indexedSendOORRecipientError(i, err)
		}

		if err := validateOORRecipientFloor(
			out.AmountSat, terms.MinVTXOAmountFloor(),
		); err != nil {
			return nil, indexedSendOORRecipientError(i, err)
		}

		policyTemplate, err := r.resolveOutputPolicyTemplate(
			ctx, out, pkScript, terms.PubKey, terms.VTXOExitDelay,
		)
		if err != nil {
			return nil, indexedSendOORRecipientError(i, err)
		}

		key := fmt.Sprintf("%d:%x", out.AmountSat, pkScript)
		if first, ok := seen[key]; ok {
			return nil, status.Errorf(codes.InvalidArgument,
				"recipient %d duplicates recipient %d", i,
				first)
		}
		seen[key] = i

		recipients = append(recipients, oortx.RecipientOutput{
			PkScript:           pkScript,
			Value:              btcutil.Amount(out.AmountSat),
			VTXOPolicyTemplate: policyTemplate,
		})
	}

	return recipients, nil
}

// resolveOORRecipientOutpoints maps each requested recipient to the outpoint it
// occupies after the canonical OOR output ordering is applied.
func (r *RPCServer) resolveOORRecipientOutpoints(ctx context.Context,
	sessionID oor.SessionID,
	allRecipients, requestRecipients []oortx.RecipientOutput) []string {

	sessionHash := chainhash.Hash(sessionID)
	outpoints := make([]string, len(requestRecipients))
	for i, recipient := range requestRecipients {
		outpoint, err := oortx.RecipientOutPoint(
			sessionHash, allRecipients, recipient,
		)
		if err != nil {
			r.server.log.WarnS(ctx, "Unable to resolve OOR "+
				"recipient outpoint", err,
				slog.String("session_id", sessionID.String()),
				slog.Int("recipient_index", i))

			continue
		}

		outpoints[i] = outpoint.String()
	}

	return outpoints
}

// walletStateToProto maps the daemon's in-process WalletState enum to
// the public waverpc.WalletState wire enum.
func walletStateToProto(s WalletState) waverpc.WalletState {
	switch s {
	case WalletStateNone:
		return waverpc.WalletState_WALLET_STATE_NONE

	case WalletStateLocked:
		return waverpc.WalletState_WALLET_STATE_LOCKED

	case WalletStateUnlocking:
		return waverpc.WalletState_WALLET_STATE_LOCKED

	case WalletStateSyncing:
		return waverpc.WalletState_WALLET_STATE_SYNCING

	case WalletStateReady:
		return waverpc.WalletState_WALLET_STATE_READY

	default:
		return waverpc.WalletState_WALLET_STATE_UNSPECIFIED
	}
}

// GetInfo returns basic information about the running daemon instance,
// including version, network, and lnd connection state.
func (r *RPCServer) GetInfo(ctx context.Context, _ *waverpc.GetInfoRequest) (
	*waverpc.GetInfoResponse, error) {

	resp := &waverpc.GetInfoResponse{
		Version:         build.Version(),
		Commit:          build.CommitHash,
		Network:         r.server.cfg.Network,
		ServerConnected: r.server.isServerConnected(),
		WalletType:      r.server.cfg.Wallet.Type,
		WalletState: walletStateToProto(
			r.server.WalletLifecycleState(),
		),
	}

	// Populate lnd fields if connected.
	r.server.lnd.WhenSome(
		func(lndSvc *lndclient.GrpcLndServices) {
			resp.LndIdentityPubkey =
				lndSvc.NodePubkey.String()
			resp.LndAlias = lndSvc.NodeAlias

			// Fetch the current best block height from
			// the chain backend via lnd's ChainKit
			// interface.
			_, height, err :=
				lndSvc.ChainKit.GetBestBlock(ctx)
			if err != nil {
				r.server.log.WarnS(
					ctx,
					"Unable to fetch block height",
					err,
				)
			} else {
				resp.BlockHeight = uint32(height)
			}
		},
	)

	// IdentityPubkey is the daemon identity key derived from
	// the active wallet backend. For lnd-backed daemons this
	// intentionally differs from the
	// public node key above and must stay aligned with the indexer proof
	// signer used for script-scoped VTXO queries.
	identityPubkey, err := r.deriveIdentityPubkey(ctx)
	if err != nil {
		r.server.log.WarnS(ctx, "Unable to derive daemon identity key",
			err,
		)
	} else {
		resp.IdentityPubkey = identityPubkey
	}

	// Populate lwwallet fields if the lightweight wallet is active.
	r.server.lwWallet.WhenSome(
		func(w *lwwallet.Wallet) {
			// Get block height from the chain backend.
			if r.server.chainBackend != nil {
				height, _, err :=
					r.server.chainBackend.BestBlock(
						ctx,
					)
				if err != nil {
					r.server.log.WarnS(ctx,
						"Unable to fetch block "+
							"height", err)
				} else {
					resp.BlockHeight = uint32(height)
				}
			}
		},
	)

	// Populate btcwallet fields if the neutrino-backed wallet is
	// active.
	r.server.btcwWallet.WhenSome(
		func(w *btcwbackend.Wallet) {
			// Get block height from the chain backend.
			if r.server.chainBackend != nil {
				height, _, err :=
					r.server.chainBackend.BestBlock(
						ctx,
					)
				if err != nil {
					r.server.log.WarnS(ctx,
						"Unable to fetch block "+
							"height", err)
				} else {
					resp.BlockHeight = uint32(height)
				}
			}
		},
	)

	if terms := r.server.loadOperatorTerms(); terms != nil {
		// PubKey is mandatory in the server's GetInfo response, so a
		// cached operator-terms snapshot should always carry it here.
		if terms.PubKey == nil {
			r.server.log.WarnS(
				ctx, "Cached operator terms missing "+
					"operator pubkey", nil,
			)

			return resp, nil
		}

		minVTXOAmount := terms.MinVTXOAmountFloor()

		// The forfeit penalty key, sweep key and delay are no longer
		// global operator terms; they are delivered per round in the
		// round batch info, so they are not surfaced on this
		// daemon-level ServerInfo snapshot.
		resp.ServerInfo = &waverpc.ServerInfo{
			OperatorPubkey:    terms.PubKey.SerializeCompressed(),
			BoardingExitDelay: terms.BoardingExitDelay,
			VtxoExitDelay:     terms.VTXOExitDelay,
			DustLimit:         uint64(terms.DustLimit),
			MinBoardingAmount: uint64(terms.MinBoardingAmount),
			MaxVtxoAmount:     uint64(terms.MaxVTXOAmount),
			FeeRate:           uint64(terms.FeeRate),
			MinOperatorFee:    uint64(terms.MinOperatorFee),
			MinConfirmations:  terms.MinConfirmations,
			MinVtxoAmountSat:  uint64(minVTXOAmount),
			MaxUserBalance:    uint64(terms.MaxUserBalance),
		}
		resp.ServerInfo.FreeRefreshWindowBlocks =
			terms.FreeRefreshWindowBlocks
	}

	return resp, nil
}

// requireWalletReady returns a gRPC error if the wallet is not yet
// ready. Callers use this to gate RPCs that need wallet access.
func (r *RPCServer) requireWalletReady() error {
	if r.server.isWalletReady() {
		return nil
	}

	switch r.server.WalletLifecycleState() {
	case WalletStateReady:
		return nil

	case WalletStateNone:
		return waverpc.WalletNotReadyError(
			"wallet is not ready (create first)",
		)

	case WalletStateLocked:
		return waverpc.WalletNotReadyError(
			"wallet is not ready (unlock first)",
		)

	case WalletStateUnlocking:
		return waverpc.WalletNotReadyError(
			"wallet unlock is in progress",
		)

	case WalletStateSyncing:
		return waverpc.WalletNotReadyError(
			"wallet is syncing; try again once sync completes",
		)

	default:
		return waverpc.WalletNotReadyError("wallet is not ready")
	}
}

// GetBalance returns the current balance of the wallet, broken down
// by boarding (on-chain) and VTXO (off-chain) balances.
func (r *RPCServer) GetBalance(ctx context.Context,
	_ *waverpc.GetBalanceRequest) (*waverpc.GetBalanceResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	resp := &waverpc.GetBalanceResponse{}

	// Fetch boarding balance by querying the boarding store directly.
	// Going through the wallet actor's Ask path would serialize this
	// read behind any backlogged actor work — each tip-tick handler
	// runs one ListUnspent per tip advance, and a slow backend can
	// keep that call in flight long enough to stall the mailbox while
	// a GetBalance Ask sits queued behind it. The actor's
	// handleGetBoardingBalance is a pure read of boarding lifecycle
	// FetchBoardingIntentsByStatus calls with no in-memory actor
	// state, so this direct store read returns the same value
	// without queuing behind the backlog. See bug-2 in BUGS_FOUND.md
	// for the longer-term actor-side coalescing fix that would let
	// us drop this bypass; the contract notes on
	// handleGetBoardingBalance must stay in sync with this site so
	// the two paths cannot silently diverge.
	if err := r.fetchBoardingBalance(ctx, resp); err != nil {
		return nil, status.Errorf(codes.Internal, "fetch boarding "+
			"balance: %v", err)
	}

	// Fetch the spendable VTXO balance. Propagate query errors rather
	// than silently zeroing — a returned zero is functionally
	// indistinguishable from "no VTXOs" and would mislead a UI that
	// trusts the response, which is the same rationale that makes
	// fetchBoardingBalance error-strict above.
	if r.server.vtxoStore != nil {
		// Only the Live subset is spendable; the other non-terminal
		// states would overstate vtxo_balance_sat.
		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fetch vtxo "+
				"balance: %v", err)
		}
		resp.VtxoBalanceSat = int64(vtxo.SumSpendableBalance(liveVTXOs))
		resp.VtxoPendingSat = int64(vtxo.SumPendingBalance(liveVTXOs))

		// Exiting VTXOs aren't in ListLiveVTXOs; query them directly.
		// Light skips the ancestry side table a balance never walks.
		exiting, err := r.server.vtxoStore.ListVTXOsByStatusLight(
			ctx, vtxo.VTXOStatusUnilateralExit,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fetch "+
				"exiting vtxo balance: %v", err)
		}

		// Exclude recovery-only exit targets (not wallet liquidity).
		// Absent the exit-job store, no such targets exist.
		exitSat := vtxo.SumBalance(exiting)
		if r.server.ueStore != nil {
			exitSat, err = sumWalletUnilateralExits(
				ctx, exiting, r.server.ueStore,
			)
			if err != nil {
				return nil, status.Errorf(codes.Internal,
					"sum exiting vtxo balance: %v", err)
			}
		}
		resp.VtxoUnilateralExitSat = int64(exitSat)
	}

	// Fetch the confirmed balance of the backing on-chain wallet so
	// callers can observe sweep proceeds from unilateral exits. Same
	// rationale as the VTXO fetch above: a per-backend failure must
	// surface to the caller rather than be papered over with zero.
	onchain, err := sumOnchainWalletConfirmed(
		ctx, r.walletBalanceFetchers(),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch onchain "+
			"wallet balance: %v", err)
	}
	resp.OnchainWalletConfirmedSat = int64(onchain)

	resp.TotalConfirmedSat = resp.BoardingConfirmedSat +
		resp.VtxoBalanceSat

	return resp, nil
}

// fetchBoardingBalance populates the boarding-related fields of resp by
// querying the boarding store directly, bypassing the wallet actor's
// serial mailbox. Returns the first query error rather than mixing a
// partial boarding view into the returned balance.
func (r *RPCServer) fetchBoardingBalance(ctx context.Context,
	resp *waverpc.GetBalanceResponse) error {

	boardingStore := r.server.newBoardingStore()

	sumAmounts := func(intents []wallet.BoardingIntent) btcutil.Amount {
		var total btcutil.Amount
		for _, intent := range intents {
			total += intent.ChainInfo.Amount
		}

		return total
	}

	unconfirmed, err := r.fetchUnconfirmedBoardingBalance(
		ctx, boardingStore,
	)
	if err != nil {
		return err
	}
	resp.BoardingUnconfirmedSat = int64(unconfirmed)

	confirmed, err := boardingStore.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	if err != nil {
		return fmt.Errorf("fetch confirmed boarding balance: %w", err)
	}
	resp.BoardingConfirmedSat = int64(sumAmounts(confirmed))

	adopted, err := boardingStore.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusAdopted,
	)
	if err != nil {
		return fmt.Errorf("fetch adopted boarding balance: %w", err)
	}
	resp.BoardingAdoptedSat = int64(sumAmounts(adopted))

	pendingSweep, err := boardingStore.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusSweepPending,
	)
	if err != nil {
		return fmt.Errorf("fetch sweep-pending boarding balance: %w",
			err)
	}
	resp.BoardingPendingSweepSat = int64(sumAmounts(pendingSweep))

	swept, err := boardingStore.FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusSwept,
	)
	if err != nil {
		return fmt.Errorf("fetch swept boarding balance: %w", err)
	}
	resp.BoardingSweptSat = int64(sumAmounts(swept))

	return nil
}

// exitJobLookup reads one exit job's policy identity; kept narrow so the
// balance sum can be faked in tests.
type exitJobLookup interface {
	GetJob(ctx context.Context,
		target wire.OutPoint) (*db.UnilateralExitJobRecord, error)
}

// sumWalletUnilateralExits sums unilateral-exit VTXOs that are wallet
// inventory, excluding recovery-only targets (a non-standard exit policy,
// e.g. a vHTLC refund) whose value is not spendable wallet liquidity.
func sumWalletUnilateralExits(ctx context.Context, exiting []*vtxo.Descriptor,
	jobs exitJobLookup) (btcutil.Amount, error) {

	var total btcutil.Amount
	for _, desc := range exiting {
		job, err := jobs.GetJob(ctx, desc.Outpoint)

		// No job: standard wallet exit.
		if errors.Is(err, db.ErrUnilateralExitJobNotFound) {
			total += desc.Amount
			continue
		}
		if err != nil {
			return 0, err
		}

		// Non-standard policy: recovery-only, not wallet value.
		if actormsg.ExitPolicyKind(job.ExitPolicyKind).Valid() {
			continue
		}

		total += desc.Amount
	}

	return total, nil
}

// fetchUnconfirmedBoardingBalance sums zero-conf wallet UTXOs that pay to known
// boarding scripts.
func (r *RPCServer) fetchUnconfirmedBoardingBalance(ctx context.Context,
	boardingStore *db.BoardingWalletStore) (btcutil.Amount, error) {

	utxos, err := r.server.listBackingWalletUnspent(
		ctx, 0, wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return 0, fmt.Errorf("list unspent: %w", err)
	}

	var total btcutil.Amount
	for _, utxo := range utxos {
		if utxo == nil || utxo.Confirmations != 0 {
			continue
		}

		addr, err := boardingStore.LookupBoardingAddress(
			ctx, utxo.PkScript,
		)
		if err != nil || addr == nil {
			continue
		}

		total += utxo.Amount
	}

	return total, nil
}

// onchainWalletConfirmedFetcher returns the confirmed balance of one
// on-chain wallet backend. Returning an error lets the caller log the
// failure while continuing on to any sibling backends.
type onchainWalletConfirmedFetcher func(
	ctx context.Context) (btcutil.Amount, error)

// walletBalanceFetchers returns one fetcher per active on-chain
// wallet backend. Backends are mutually exclusive in production, but
// returning a slice keeps the summation logic independent of how many
// backends are wired up.
func (r *RPCServer) walletBalanceFetchers() []onchainWalletConfirmedFetcher {
	var fetchers []onchainWalletConfirmedFetcher

	r.server.lnd.WhenSome(func(lndSvc *lndclient.GrpcLndServices) {
		fetchers = append(fetchers, func(ctx context.Context) (
			btcutil.Amount, error) {

			wb, err := lndSvc.Client.WalletBalance(ctx)
			if err != nil {
				return 0, fmt.Errorf("lnd wallet balance: %w",
					err)
			}

			return wb.Confirmed, nil
		})
	})

	r.server.lwWallet.WhenSome(func(w *lwwallet.Wallet) {
		fetchers = append(fetchers, func(ctx context.Context) (
			btcutil.Amount, error) {

			confirmed, _, err := w.Balance(ctx)
			if err != nil {
				return 0, fmt.Errorf("lightweight wallet "+
					"balance: %w", err)
			}

			return confirmed, nil
		})
	})

	r.server.btcwWallet.WhenSome(func(w *btcwbackend.Wallet) {
		fetchers = append(fetchers, func(ctx context.Context) (
			btcutil.Amount, error) {

			confirmed, _, err := w.Balance(ctx)
			if err != nil {
				return 0, fmt.Errorf("btcwallet balance: %w",
					err)
			}

			return confirmed, nil
		})
	})

	return fetchers
}

// sumOnchainWalletConfirmed invokes each fetcher in order and returns
// the accumulated confirmed balance. The first fetcher error is
// returned; partial sums are not surfaced because the caller (the
// GetBalance RPC handler) treats every balance component as
// error-strict — a zero-on-failure response is structurally
// indistinguishable from "no balance" and would mislead any UI that
// trusts the body.
func sumOnchainWalletConfirmed(ctx context.Context,
	fetchers []onchainWalletConfirmedFetcher) (btcutil.Amount, error) {

	var total btcutil.Amount
	for _, fetch := range fetchers {
		confirmed, err := fetch(ctx)
		if err != nil {
			return 0, err
		}

		total += confirmed
	}

	return total, nil
}

// metricsWalletBalance returns the client's on-chain confirmed and
// unconfirmed balance in satoshis for the scrape-driven wallet gauges.
// It branches on the active wallet backend and returns an error (so the
// scrape skips the gauges) when the wallet is not ready or no backend is
// present, rather than reporting a misleading zero.
func (r *RPCServer) metricsWalletBalance(ctx context.Context) (int64, int64,
	error) {

	if !r.server.isWalletReady() {
		return 0, 0, fmt.Errorf("wallet not ready")
	}

	var (
		confirmed, unconfirmed int64
		found                  bool
		qErr                   error
	)
	r.server.lnd.WhenSome(func(lndSvc *lndclient.GrpcLndServices) {
		found = true
		wb, balErr := lndSvc.Client.WalletBalance(ctx)
		if balErr != nil {
			qErr = fmt.Errorf("lnd wallet balance: %w", balErr)

			return
		}
		confirmed = int64(wb.Confirmed)
		unconfirmed = int64(wb.Unconfirmed)
	})
	r.server.lwWallet.WhenSome(func(w *lwwallet.Wallet) {
		found = true
		c, u, balErr := w.Balance(ctx)
		if balErr != nil {
			qErr = fmt.Errorf("lightweight wallet balance: %w",
				balErr)

			return
		}
		confirmed, unconfirmed = int64(c), int64(u)
	})
	r.server.btcwWallet.WhenSome(func(w *btcwbackend.Wallet) {
		found = true
		c, u, balErr := w.Balance(ctx)
		if balErr != nil {
			qErr = fmt.Errorf("btcwallet balance: %w", balErr)

			return
		}
		confirmed, unconfirmed = int64(c), int64(u)
	})

	if qErr != nil {
		return 0, 0, qErr
	}
	if !found {
		return 0, 0, fmt.Errorf("no wallet backend available")
	}

	return confirmed, unconfirmed, nil
}

// metricsBlockHeight returns the best block height seen by the client's
// chain backend for the scrape-driven block-height gauge. It returns an
// error (so the scrape skips the gauge) when no chain backend is wired
// yet.
func (r *RPCServer) metricsBlockHeight(ctx context.Context) (int64, error) {
	var (
		height int64
		found  bool
		qErr   error
	)
	r.server.lnd.WhenSome(func(lndSvc *lndclient.GrpcLndServices) {
		found = true
		_, h, hErr := lndSvc.ChainKit.GetBestBlock(ctx)
		if hErr != nil {
			qErr = fmt.Errorf("lnd best block: %w", hErr)

			return
		}
		height = int64(h)
	})

	if !found && r.server.chainBackend != nil {
		found = true
		h, _, hErr := r.server.chainBackend.BestBlock(ctx)
		if hErr != nil {
			return 0, fmt.Errorf("chain backend best block: %w",
				hErr)
		}
		height = int64(h)
	}

	if qErr != nil {
		return 0, qErr
	}
	if !found {
		return 0, fmt.Errorf("no chain backend available")
	}

	return height, nil
}

// liveOORSessionsByState returns a count of currently-tracked OOR
// sessions grouped by a short state label, for the scrape-driven
// oor_sessions_by_state gauge. It reads the live OOR actor only (the
// cumulative lifetime totals live in the oor_transfers_* counters), so
// the query stays bounded and cheap at scrape time.
func (r *RPCServer) liveOORSessionsByState(ctx context.Context) (
	map[string]int64, error) {

	infos, err := r.queryOORSessionSummaries(
		ctx, &waverpc.ListOORSessionsRequest{},
	)
	if err != nil {
		return nil, err
	}

	byState := make(map[string]int64)
	for _, info := range infos {
		byState[oorStateLabel(info.GetStatus())]++
	}

	return byState, nil
}

// liveRoundsByStatus returns a count of currently-live rounds grouped by
// a short status label, for the scrape-driven rounds_by_status gauge. It
// reads the live round actor only; lifetime totals live in the
// rounds_joined_total / rounds_completed_total counters.
func (r *RPCServer) liveRoundsByStatus(ctx context.Context) (map[string]int64,
	error) {

	infos, err := r.queryRoundStates(ctx)
	if err != nil {
		return nil, err
	}

	byStatus := make(map[string]int64)
	for _, info := range infos {
		byStatus[roundStateLabel(info.GetState())]++
	}

	return byStatus, nil
}

// oorStateLabel maps an OORSessionStatus enum to a short, stable,
// dashboard-friendly label (e.g. "pending") by stripping the proto enum
// prefix and lowercasing.
func oorStateLabel(s waverpc.OORSessionStatus) string {
	return strings.ToLower(
		strings.TrimPrefix(
			s.String(),
			"OOR_SESSION_STATUS_",
		),
	)
}

// roundStateLabel maps a RoundState enum to a short, stable label (e.g.
// "confirmed") by stripping the proto enum prefix and lowercasing.
func roundStateLabel(s waverpc.RoundState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "ROUND_STATE_"))
}

// ListVTXOs returns the set of VTXOs known to the wallet, optionally
// filtered by status and minimum amount. The VTXO_STATUS_PENDING_ROUND
// filter is special: it bypasses the on-disk store and projects each
// upcoming VTXO from the live round actor as a synthetic VTXO entry,
// giving callers a way to see the outputs they have signed for but
// which have not yet been created by an on-chain commitment.
func (r *RPCServer) ListVTXOs(ctx context.Context,
	req *waverpc.ListVTXOsRequest) (*waverpc.ListVTXOsResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req.StatusFilter ==
		waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND {
		return r.listPendingRoundVTXOs(ctx, req)
	}

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "vtxo store not "+
			"initialized")
	}

	// Fetch VTXOs from the store. When a specific status filter
	// is provided, query the DB directly for that status so
	// terminal states (spent, forfeited) are reachable. When
	// unspecified, return all non-terminal (live) VTXOs.
	var (
		dbVTXOs []*vtxo.Descriptor
		err     error
	)

	if req.StatusFilter !=
		waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED {

		domainStatus, sErr := protoStatusToDomain(
			req.StatusFilter,
		)
		if sErr != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid status filter: %v", sErr)
		}

		// The listing response never carries ancestry, so the light
		// variants skip the ancestry side-table join (whose TLV tree
		// fragments grow with OOR chain depth) entirely.
		dbVTXOs, err = r.server.vtxoStore.ListVTXOsByStatusLight(
			ctx, domainStatus,
		)
	} else {
		dbVTXOs, err = r.server.vtxoStore.ListLiveVTXOsLight(ctx)
	}

	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to list "+
			"VTXOs: %v", err)
	}

	// Apply any remaining in-memory filters (min amount) via
	// the package-level FilterDescriptors function so this
	// logic is reusable by a future SDK.
	filterOpts := vtxo.FilterOptions{
		MinAmount: btcutil.Amount(req.MinAmountSat),
	}

	filtered := vtxo.FilterDescriptors(dbVTXOs, filterOpts)

	// Resolving the OOR package for an outpoint costs one artifact-store
	// read per VTXO, so listing-only callers can opt out of checkpoint
	// PSBT population entirely.
	var packageStore *db.OORArtifactPersistenceStore
	if !req.ExcludeCheckpointPsbts {
		for i := range filtered {
			if filtered[i].Status == vtxo.VTXOStatusSpent ||
				filtered[i].ChainDepth > 0 {

				packageStore = r.newLocalOORArtifactStore()

				break
			}
		}
	}

	// Convert to proto.
	protoVTXOs := make(
		[]*waverpc.VTXO, 0, len(filtered),
	)
	for _, v := range filtered {
		protoVTXO := descriptorToProto(v)
		if packageStore != nil &&
			(v.Status == vtxo.VTXOStatusSpent ||
				v.ChainDepth > 0) {

			err := populatePackageCheckpointPSBTs(
				ctx, packageStore, v, protoVTXO,
			)
			if err != nil {
				return nil, status.Errorf(codes.Internal,
					"populate package checkpoint psbts: %v",
					err)
			}
		}

		protoVTXOs = append(protoVTXOs, protoVTXO)
	}

	return &waverpc.ListVTXOsResponse{
		Vtxos: protoVTXOs,
	}, nil
}

// listPendingRoundVTXOs projects the upcoming VTXOs from every live
// round FSM as synthetic VTXO entries. Each entry carries the
// expected amount, the originating round id (once assigned), and the
// commitment txid (once the round actor has received the commitment
// transaction). The outpoint is intentionally empty because the
// precise leaf outpoint inside the VTXO tree is not finalised until
// the commitment transaction confirms.
func (r *RPCServer) listPendingRoundVTXOs(ctx context.Context,
	req *waverpc.ListVTXOsRequest) (*waverpc.ListVTXOsResponse, error) {

	rounds, err := r.queryRoundStates(ctx)
	if err != nil {
		return nil, err
	}

	minAmount := btcutil.Amount(req.MinAmountSat)
	pendingRoundStatus := waverpc.VTXOStatus_VTXO_STATUS_PENDING_ROUND

	protoVTXOs := make([]*waverpc.VTXO, 0)
	for _, info := range rounds {
		// Skip rounds whose commitment tx has already confirmed.
		// Their VTXOs are written to the on-disk store as
		// VTXO_STATUS_LIVE so they surface there; including them
		// here too would cause a brief double-report in the window
		// between commitment confirmation and the actor cleaning
		// the round out of its in-memory map.
		if info.State == waverpc.RoundState_ROUND_STATE_CONFIRMED {
			continue
		}

		for _, v := range info.Vtxos {
			amount := btcutil.Amount(v.AmountSat)
			if amount < minAmount {
				continue
			}

			protoVTXOs = append(protoVTXOs, &waverpc.VTXO{
				AmountSat:      v.AmountSat,
				Status:         pendingRoundStatus,
				RoundId:        info.RoundId,
				CommitmentTxid: info.CommitmentTxid,
				Outpoint:       v.Outpoint,
			})
		}
	}

	return &waverpc.ListVTXOsResponse{
		Vtxos: protoVTXOs,
	}, nil
}

// protoStatusToDomain converts a proto VTXOStatus enum to the domain
// vtxo.VTXOStatus type for use with vtxo.FilterDescriptors. An error
// is returned for unknown status values to surface proto/domain drift
// early rather than silently defaulting.
func protoStatusToDomain(s waverpc.VTXOStatus) (vtxo.VTXOStatus, error) {
	switch s {
	case waverpc.VTXOStatus_VTXO_STATUS_LIVE:
		return vtxo.VTXOStatusLive, nil

	case waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return vtxo.VTXOStatusPendingForfeit, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return vtxo.VTXOStatusForfeiting, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return vtxo.VTXOStatusForfeited, nil

	case waverpc.VTXOStatus_VTXO_STATUS_SPENT:
		return vtxo.VTXOStatusSpent, nil

	case waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return vtxo.VTXOStatusUnilateralExit, nil

	case waverpc.VTXOStatus_VTXO_STATUS_FAILED:
		return vtxo.VTXOStatusFailed, nil

	default:
		return 0, fmt.Errorf("unknown VTXO status: %v", s)
	}
}

// vtxoStatusToProto converts a domain VTXOStatus to the proto enum.
func vtxoStatusToProto(s vtxo.VTXOStatus) waverpc.VTXOStatus {
	switch s {
	case vtxo.VTXOStatusLive:
		return waverpc.VTXOStatus_VTXO_STATUS_LIVE

	case vtxo.VTXOStatusPendingForfeit:
		return waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT

	case vtxo.VTXOStatusForfeiting:
		return waverpc.VTXOStatus_VTXO_STATUS_FORFEITING

	case vtxo.VTXOStatusForfeited:
		return waverpc.VTXOStatus_VTXO_STATUS_FORFEITED

	case vtxo.VTXOStatusSpent:
		return waverpc.VTXOStatus_VTXO_STATUS_SPENT

	case vtxo.VTXOStatusUnilateralExit:
		return waverpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT

	case vtxo.VTXOStatusFailed:
		return waverpc.VTXOStatus_VTXO_STATUS_FAILED

	case vtxo.VTXOStatusSpending:
		return waverpc.VTXOStatus_VTXO_STATUS_SPENDING

	default:
		return waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED
	}
}

// descriptorToProto converts a vtxo.Descriptor to the proto VTXO message.
func descriptorToProto(v *vtxo.Descriptor) *waverpc.VTXO {
	proto := &waverpc.VTXO{
		Outpoint: fmt.Sprintf(
			"%s:%d", v.Outpoint.Hash, v.Outpoint.Index,
		),
		AmountSat:      int64(v.Amount),
		Status:         vtxoStatusToProto(v.Status),
		BatchExpiry:    v.BatchExpiry,
		RoundId:        v.RoundID,
		CreatedHeight:  v.CreatedHeight,
		RelativeExpiry: v.RelativeExpiry,
		PkScript:       hex.EncodeToString(v.PkScript),
		CommitmentTxid: v.CommitmentTxID.String(),
		ChainDepth:     uint32(v.ChainDepth),
	}

	// Settlement is Some only for FORFEITED VTXOs whose forfeit round row
	// was found by the by-status join; for every other VTXO it is None and
	// the proto settlement message stays nil, so absence is explicit on the
	// wire.
	v.Settlement.WhenSome(func(s vtxo.Settlement) {
		proto.Settlement = &waverpc.VTXOSettlement{
			Txid:   s.TxID.String(),
			Height: s.Height,
		}
	})

	return proto
}

// newLocalOORArtifactStore returns the local artifact store used to resolve
// persisted OOR package checkpoints for spent VTXOs.
func (r *RPCServer) newLocalOORArtifactStore() *db.OORArtifactPersistenceStore {
	if r.server.db == nil {
		return nil
	}

	dbStore := db.NewStore(
		r.server.db.DB, r.server.db.Queries, r.server.db.Backend(),
		r.server.log,
	)

	return dbStore.NewOORArtifactStore(clock.NewDefaultClock())
}

// populatePackageCheckpointPSBTs attaches locally persisted finalized
// checkpoint PSBTs to one VTXO response when an OOR package is available for
// its outpoint.
func populatePackageCheckpointPSBTs(ctx context.Context,
	store *db.OORArtifactPersistenceStore, desc *vtxo.Descriptor,
	protoVTXO *waverpc.VTXO) error {

	if store == nil || desc == nil || protoVTXO == nil {
		return nil
	}

	bundle, err := store.GetPackageForOutpoint(ctx, desc.Outpoint)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil

	case err != nil:
		return err
	}

	checkpoints := make(
		[][]byte, 0, len(bundle.FinalCheckpointPSBTs),
	)
	for i := range bundle.FinalCheckpointPSBTs {
		raw, err := psbtutil.Serialize(
			bundle.FinalCheckpointPSBTs[i],
		)
		if err != nil {
			return fmt.Errorf("serialize checkpoint %d: %w", i, err)
		}

		checkpoints = append(checkpoints, raw)
	}

	protoVTXO.OorFinalCheckpointPsbts = checkpoints

	return nil
}

// NewAddress generates a new boarding address that can receive
// on-chain funds for use in the Ark protocol.
func (r *RPCServer) NewAddress(ctx context.Context,
	_ *waverpc.NewAddressRequest) (*waverpc.NewAddressResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	// Fetch operator terms to get the operator key and exit delay
	// needed for the boarding address tapscript.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	addrReq := &wallet.CreateBoardingAddressRequest{
		OperatorKey: terms.PubKey,
		ExitDelay:   terms.BoardingExitDelay,
	}
	future := wRef.Ask(ctx, addrReq)
	result := future.Await(ctx)

	addrResp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to create "+
			"boarding address: %v", err)
	}

	resp, ok := addrResp.(*wallet.CreateBoardingAddressResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", addrResp)
	}

	return &waverpc.NewAddressResponse{
		Address: resp.Address.String(),
	}, nil
}

// RefreshVTXOs queues one or more VTXOs for refresh in the next round.
// This extends their expiry without changing ownership. If the all flag
// is set, every live VTXO is queued for refresh.
func (r *RPCServer) RefreshVTXOs(ctx context.Context,
	req *waverpc.RefreshVTXOsRequest) (*waverpc.RefreshVTXOsResponse,
	error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	// Build the list of target outpoints. The wallet actor only
	// refreshes outpoints it receives in TargetOutpoints (despite a
	// stale docstring suggesting empty means "all"), so when the
	// caller asks for --all we enumerate live VTXOs here and pass
	// them as explicit targets. Without this expansion the wallet
	// gets target_count=0, registers an empty refresh package, and
	// nothing actually happens — but the response previously
	// synthesised `queued_outpoints` from a separate ListLiveVTXOs
	// call below, which made the caller believe the refresh
	// succeeded.
	var targets []wire.OutPoint

	switch sel := req.Selection.(type) {
	case *waverpc.RefreshVTXOsRequest_All:
		if !sel.All {
			return nil, status.Errorf(codes.InvalidArgument,
				"refresh selection 'all' must be true when set")
		}

		if r.server.vtxoStore == nil {
			return nil, status.Errorf(codes.Internal, "VTXO "+
				"store not available for refresh --all")
		}

		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list live "+
				"VTXOs: %v", err)
		}

		// ListLiveVTXOs returns every VTXO not in a terminal
		// state — which includes PendingForfeit. Without this
		// filter, a second `refresh --all` immediately after a
		// first would re-queue the same outpoints that are
		// already on their way through the round, double-
		// registering the same forfeit intent. Restrict --all
		// to outpoints actually in LiveState; explicit
		// --outpoint callers retain the existing per-outpoint
		// error path via the wallet's Errors map.
		for _, v := range liveVTXOs {
			if v.Status != vtxo.VTXOStatusLive {
				continue
			}
			targets = append(targets, v.Outpoint)
		}

	case *waverpc.RefreshVTXOsRequest_Outpoints:
		if sel.Outpoints == nil ||
			len(sel.Outpoints.Outpoints) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"outpoints list is empty")
		}

		for _, opStr := range sel.Outpoints.Outpoints {
			op, err := parseOutpointString(opStr)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"invalid outpoint %q: %v", opStr, err)
			}

			targets = append(targets, op)
		}

	default:
		return nil, status.Errorf(codes.InvalidArgument, "selection "+
			"is required (outpoints or all)")
	}

	// `--all` against a wallet with no live VTXOs is not an error —
	// return an empty result so callers and scripts don't have to
	// special-case the "nothing to refresh" path.
	if len(targets) == 0 {
		return &waverpc.RefreshVTXOsResponse{
			QueuedOutpoints: nil,
			Status:          "queued",
		}, nil
	}

	// For dry_run, validate inputs and return a preview without
	// actually queuing anything.
	if req.DryRun {
		outpointStrs := make([]string, 0, len(targets))
		for _, op := range targets {
			outpointStrs = append(
				outpointStrs,
				fmt.Sprintf("%s:%d", op.Hash, op.Index),
			)
		}

		return &waverpc.RefreshVTXOsResponse{
			QueuedOutpoints: outpointStrs,
			Status:          "preview",
		}, nil
	}

	// Send the refresh request to the wallet actor and await its
	// response.
	refreshReq := &wallet.RefreshVTXOsRequest{
		TargetOutpoints: targets,
		ForceRefresh:    true,
	}
	future := wRef.Ask(ctx, refreshReq)
	result := future.Await(ctx)

	refreshResp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "refresh request "+
			"failed: %v", err)
	}

	resp, ok := refreshResp.(*wallet.RefreshVTXOsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", refreshResp)
	}

	// Log any per-outpoint errors but don't fail the overall
	// request.
	for op, opErr := range resp.Errors {
		r.server.log.WarnS(ctx, "VTXO refresh error", opErr,
			"outpoint", op.String())
	}

	// Build the list of outpoints that were successfully queued by
	// the wallet — i.e. those in `targets` that did NOT appear in
	// `resp.Errors`. Iterating `targets` keeps this honest: it
	// reflects what the wallet was actually asked to do, not a
	// post-hoc snapshot from ListLiveVTXOs.
	queued := make([]string, 0, resp.RefreshingCount)
	for _, op := range targets {
		if _, hasErr := resp.Errors[op]; !hasErr {
			queued = append(
				queued, fmt.Sprintf("%s:%d", op.Hash, op.Index),
			)
		}
	}

	r.server.log.InfoS(ctx, "VTXOs queued for refresh",
		slog.Int("queued_count", len(queued)),
		slog.Int("error_count", len(resp.Errors)),
	)

	return &waverpc.RefreshVTXOsResponse{
		QueuedOutpoints: queued,
		Status:          "queued",
	}, nil
}

// RefreshCustomVTXOs queues caller-supplied custom-policy VTXOs for refresh in
// the next round. The inputs are not looked up from the wallet-managed VTXO set
// and are not selected from wallet live balance; callers must own the
// higher-level protocol state that prevents double-use.
func (r *RPCServer) RefreshCustomVTXOs(ctx context.Context,
	req *waverpc.RefreshCustomVTXOsRequest) (
	*waverpc.RefreshCustomVTXOsResponse, error) {

	inputs, outputs, queued, err := buildCustomRefreshRequest(req)
	if err != nil {
		return nil, err
	}
	signingContexts, err := parseCustomRefreshSigningContexts(req)
	if err != nil {
		return nil, err
	}

	if req.GetDryRun() {
		return &waverpc.RefreshCustomVTXOsResponse{
			QueuedOutpoints: queued,
			Status:          "preview",
		}, nil
	}

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "fetch operator "+
			"terms: %v", err)
	}
	if err := r.enrichCustomRefreshInputs(
		ctx, inputs, r.server.clientKeyDesc, terms.PubKey,
	); err != nil {
		return nil, err
	}
	var unregister []func()
	for outpoint, signingCtx := range signingContexts {
		unregister = append(
			unregister, r.server.forfeitSignatures.registerContext(
				outpoint, signingCtx,
			),
		)
	}

	if !r.server.walletRef.IsSome() {
		for _, fn := range unregister {
			fn()
		}

		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()
	future := wRef.Ask(ctx, &wallet.RefreshCustomVTXOsRequest{
		Inputs:  inputs,
		Outputs: outputs,
	})
	result := future.Await(ctx)

	refreshResp, err := result.Unpack()
	if err != nil {
		for _, fn := range unregister {
			fn()
		}

		return nil, status.Errorf(codes.Internal, "custom refresh "+
			"request failed: %v", err)
	}

	resp, ok := refreshResp.(*wallet.RefreshCustomVTXOsResponse)
	if !ok {
		for _, fn := range unregister {
			fn()
		}

		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", refreshResp)
	}
	if resp.RefreshingCount != len(queued) {
		for _, fn := range unregister {
			fn()
		}
		r.dropCustomRefreshVTXOs(ctx, wRef, inputs)

		return nil, status.Errorf(codes.Internal, "custom refresh "+
			"queued %d VTXOs, expected %d", resp.RefreshingCount,
			len(queued))
	}

	r.server.log.InfoS(ctx, "Custom VTXOs queued for refresh",
		slog.Int("queued_count", len(queued)),
	)

	if err := r.server.TriggerRoundRegistration(ctx); err != nil {
		for _, fn := range unregister {
			fn()
		}
		r.dropCustomRefreshVTXOs(ctx, wRef, inputs)

		return nil, status.Errorf(codes.Internal, "trigger custom "+
			"refresh round registration: %v", err)
	}

	return &waverpc.RefreshCustomVTXOsResponse{
		QueuedOutpoints: queued,
		Status:          "queued",
	}, nil
}

func (r *RPCServer) dropCustomRefreshVTXOs(ctx context.Context,
	wRef actor.ActorRef[wallet.WalletMsg, wallet.WalletResp],
	inputs []wallet.CustomRefreshInput) {

	outpoints := make([]wire.OutPoint, 0, len(inputs))
	for _, input := range inputs {
		outpoints = append(outpoints, input.Outpoint)
	}

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), 10*time.Second,
	)
	defer cancel()

	future := wRef.Ask(cleanupCtx, &wallet.DropCustomRefreshVTXOsRequest{
		Outpoints: outpoints,
	})
	result := future.Await(cleanupCtx)
	resp, err := result.Unpack()
	if err != nil {
		r.server.log.WarnS(ctx, "Failed to drop custom refresh VTXOs",
			err,
			slog.Int("count", len(outpoints)),
		)

		return
	}

	dropResp, ok := resp.(*wallet.DropCustomRefreshVTXOsResponse)
	if !ok {
		r.server.log.WarnS(ctx, "Unexpected custom refresh drop "+
			"response type", fmt.Errorf("got %T", resp))

		return
	}

	r.server.log.InfoS(ctx, "Dropped custom refresh VTXOs",
		slog.Int("dropped_count", dropResp.DroppedCount),
	)
}

func (r *RPCServer) ListPendingForfeitParticipantSignatureRequests(
	_ context.Context,
	req *waverpc.ListPendingForfeitParticipantSignatureRequestsRequest) (
	*waverpc.ListPendingForfeitParticipantSignatureRequestsResponse,
	error) {

	after := uint64(0)
	limit := uint32(0)
	if req != nil {
		after = req.GetAfterSequence()
		limit = req.GetLimit()
	}

	requests, next := r.server.forfeitSignatures.list(after, limit)

	resp := new(
		waverpc.
			ListPendingForfeitParticipantSignatureRequestsResponse,
	)
	resp.Requests = requests
	resp.NextSequence = next

	return resp, nil
}

func (r *RPCServer) SubmitForfeitParticipantSignatures(ctx context.Context,
	req *waverpc.SubmitForfeitParticipantSignaturesRequest) (
	*waverpc.SubmitForfeitParticipantSignaturesResponse, error) {

	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request is required",
		)
	}

	err := r.server.forfeitSignatures.submit(
		req.GetRequestId(), req.GetSignatures(),
	)
	if err != nil {
		return nil, err
	}

	r.server.log.DebugS(ctx, "Accepted forfeit participant signatures",
		slog.Int("signature_count", len(req.GetSignatures())),
	)

	return &waverpc.SubmitForfeitParticipantSignaturesResponse{}, nil
}

func buildCustomRefreshRequest(req *waverpc.RefreshCustomVTXOsRequest) (
	[]wallet.CustomRefreshInput, []wallet.CustomRefreshOutput, []string,
	error) {

	if req == nil {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"request is required")
	}

	rpcInputs := req.GetInputs()
	rpcOutputs := req.GetOutputs()
	if len(rpcInputs) == 0 {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"custom refresh inputs are empty")
	}
	if len(rpcInputs) != len(rpcOutputs) {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"custom refresh inputs/output count mismatch: %d "+
				"inputs, %d outputs", len(rpcInputs), len(
				rpcOutputs,
			))
	}

	inputs := make([]wallet.CustomRefreshInput, 0, len(rpcInputs))
	outputs := make([]wallet.CustomRefreshOutput, 0, len(rpcOutputs))
	queued := make([]string, 0, len(rpcInputs))
	seen := make(map[wire.OutPoint]struct{}, len(rpcInputs))

	for i := range rpcInputs {
		input, err := parseCustomRefreshInput(i, rpcInputs[i])
		if err != nil {
			return nil, nil, nil, err
		}
		if _, ok := seen[input.Outpoint]; ok {
			msg := "custom refresh input %d duplicate outpoint %s"

			return nil, nil, nil, status.Errorf(
				codes.InvalidArgument, msg, i, input.Outpoint)
		}
		seen[input.Outpoint] = struct{}{}

		output, err := parseCustomRefreshOutput(i, rpcOutputs[i])
		if err != nil {
			return nil, nil, nil, err
		}

		inputs = append(inputs, input)
		outputs = append(outputs, output)
		queued = append(queued, input.Outpoint.String())
	}

	return inputs, outputs, queued, nil
}

func parseCustomRefreshSigningContexts(req *waverpc.RefreshCustomVTXOsRequest) (
	map[string]forfeitSigningContext, error) {

	contexts := make(map[string]forfeitSigningContext)
	if req == nil {
		return contexts, nil
	}

	for i, input := range req.GetInputs() {
		if input == nil || input.GetForfeitSigningContext() == nil {
			continue
		}

		outpoint, err := parseOutpointString(input.GetOutpoint())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"parse custom refresh input %d outpoint %q: %v",
				i, input.GetOutpoint(), err)
		}

		ctx := input.GetForfeitSigningContext()
		hash := ctx.GetPaymentHash()
		if len(hash) != 32 {
			return nil, status.Errorf(codes.InvalidArgument,
				"custom refresh input %d signing context "+
					"payment_hash must be 32 bytes", i)
		}
		if ctx.GetSigningRoute() == unspecifiedForfeitSigningRoute() {
			return nil, status.Errorf(codes.InvalidArgument,
				"custom refresh input %d signing context "+
					"signing_route is required", i)
		}

		contexts[outpoint.String()] = forfeitSigningContext{
			paymentHash: append([]byte(nil), hash...),
			route:       ctx.GetSigningRoute(),
		}
	}

	return contexts, nil
}

func parseCustomRefreshInput(index int,
	input *waverpc.CustomRefreshVTXOInput) (wallet.CustomRefreshInput,
	error) {

	if input == nil {
		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh input %d is nil",
			index)
	}
	if input.GetAmountSat() <= 0 {
		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh input %d "+
				"amount_sat must be positive", index)
	}
	if len(input.GetPkScript()) == 0 {
		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh input %d "+
				"pk_script is required", index)
	}
	if len(input.GetVtxoPolicyTemplate()) == 0 {
		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh input %d "+
				"vtxo_policy_template is required", index)
	}
	if len(input.GetAuthSpendPath()) == 0 {
		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh input %d "+
				"auth_spend_path is required", index)
	}
	if len(input.GetForfeitSpendPath()) == 0 {
		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh input %d "+
				"forfeit_spend_path is required", index)
	}

	outpoint, err := parseOutpointString(input.GetOutpoint())
	if err != nil {
		msg := "parse custom refresh input %d outpoint %q: %v"

		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, msg, index, input.GetOutpoint(),
			err)
	}

	template, err := arkscript.DecodePolicyTemplate(
		input.GetVtxoPolicyTemplate(),
	)
	if err != nil {
		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, "decode custom refresh input "+
				"%d policy template: %v", index, err)
	}
	if !template.MatchesPkScript(input.GetPkScript()) {
		msg := "custom refresh input %d policy template does not " +
			"match pk_script"

		return wallet.CustomRefreshInput{}, status.Errorf(
			codes.InvalidArgument, msg, index)
	}

	authSpend, err := decodeBoundCustomRefreshSpend(
		index, "auth_spend_path", input.GetAuthSpendPath(),
		input.GetPkScript(),
	)
	if err != nil {
		return wallet.CustomRefreshInput{}, err
	}
	forfeitSpend, err := decodeBoundCustomRefreshSpend(
		index, "forfeit_spend_path", input.GetForfeitSpendPath(),
		input.GetPkScript(),
	)
	if err != nil {
		return wallet.CustomRefreshInput{}, err
	}

	return wallet.CustomRefreshInput{
		Outpoint: outpoint,
		Amount:   btcutil.Amount(input.GetAmountSat()),
		PkScript: append([]byte(nil), input.GetPkScript()...),
		PolicyTemplate: append(
			[]byte(nil), input.GetVtxoPolicyTemplate()...,
		),
		RelativeExpiry: maxCustomRefreshSequence(
			authSpend, forfeitSpend,
		),
		AuthSpend:    authSpend,
		ForfeitSpend: forfeitSpend,
	}, nil
}

func maxCustomRefreshSequence(paths ...*arkscript.SpendPath) uint32 {
	var maxSequence uint32
	for _, path := range paths {
		if path == nil {
			continue
		}
		if path.RequiredSequence > maxSequence {
			maxSequence = path.RequiredSequence
		}
	}

	return maxSequence
}

func parseCustomRefreshOutput(index int,
	output *waverpc.CustomRefreshVTXOOutput) (wallet.CustomRefreshOutput,
	error) {

	if output == nil {
		return wallet.CustomRefreshOutput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh output "+
				"%d is nil", index)
	}
	if output.GetAmountSat() <= 0 {
		return wallet.CustomRefreshOutput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh output %d "+
				"amount_sat must be positive", index)
	}
	if len(output.GetVtxoPolicyTemplate()) == 0 {
		return wallet.CustomRefreshOutput{}, status.Errorf(
			codes.InvalidArgument, "custom refresh output %d "+
				"vtxo_policy_template is required", index)
	}

	template, err := arkscript.DecodePolicyTemplate(
		output.GetVtxoPolicyTemplate(),
	)
	if err != nil {
		return wallet.CustomRefreshOutput{}, status.Errorf(
			codes.InvalidArgument, "decode custom refresh output "+
				"%d policy template: %v", index, err)
	}

	pkScript := output.GetPkScript()
	if len(pkScript) == 0 {
		pkScript, err = template.PkScript()
		if err != nil {
			return wallet.CustomRefreshOutput{}, status.Errorf(
				codes.InvalidArgument, "derive custom refresh "+
					"output %d pk_script: %v", index, err)
		}
	} else if !template.MatchesPkScript(pkScript) {
		msg := "custom refresh output %d policy template does not " +
			"match pk_script"

		return wallet.CustomRefreshOutput{}, status.Errorf(
			codes.InvalidArgument, msg, index)
	}

	return wallet.CustomRefreshOutput{
		Amount: btcutil.Amount(output.GetAmountSat()),
		PolicyTemplate: append(
			[]byte(nil), output.GetVtxoPolicyTemplate()...,
		),
		PkScript:    append([]byte(nil), pkScript...),
		FixedAmount: output.GetFixedAmount(),
	}, nil
}

func decodeBoundCustomRefreshSpend(index int, field string, raw,
	pkScript []byte) (*arkscript.SpendPath, error) {

	spendPath, err := arkscript.DecodeSpendPath(raw)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode "+
			"custom refresh input %d %s: %v", index, field, err)
	}
	if err := spendPath.VerifyBindsToPkScript(pkScript); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "custom "+
			"refresh input %d %s does not bind to pk_script: %v",
			index, field, err)
	}

	return spendPath, nil
}

// LeaveVTXOs queues one or more VTXOs for cooperative leave
// (offboard) in the next round. Each VTXO is forfeited and the
// forfeited amount lands on-chain at the caller's destination —
// either the default one or a per-outpoint override from the
// destinations map. Under the #270 seal-time fee handshake the
// server stamps the residual (Σin − Σ(fixed) − fee) onto the
// wallet's IsChange=true leave output at seal time, so the RPC
// layer does not need to pre-quote any per-input operator fee.
//
// Shape mirrors RefreshVTXOs: OutpointSelection oneof + dry_run +
// {queued_outpoints, status}.
func (r *RPCServer) LeaveVTXOs(ctx context.Context,
	req *waverpc.LeaveVTXOsRequest) (*waverpc.LeaveVTXOsResponse, error) {

	// Pure-argument validation (selection / destinations / dry_run)
	// runs before the wallet-ready gate so a malformed request
	// surfaces InvalidArgument regardless of wallet state. This
	// matches the API-correctness ordering rule: client bugs should
	// always look like client bugs.

	// Parse selection. "All" cannot be combined with per-outpoint
	// destination overrides because we don't know the outpoint set
	// up front — callers that want distinct destinations must
	// enumerate the outpoints themselves.
	var (
		targets  []wire.OutPoint
		leaveAll bool
	)
	switch sel := req.Selection.(type) {
	case *waverpc.LeaveVTXOsRequest_All:
		leaveAll = sel.All
		if len(req.Destinations) > 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"per-outpoint destinations not supported "+
					"with selection=all")
		}

	case *waverpc.LeaveVTXOsRequest_Outpoints:
		if sel.Outpoints == nil ||
			len(sel.Outpoints.Outpoints) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"outpoints list is empty")
		}

		for _, opStr := range sel.Outpoints.Outpoints {
			op, err := parseOutpointString(opStr)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"invalid outpoint %q: %v", opStr, err)
			}

			targets = append(targets, op)
		}

	default:
		return nil, status.Errorf(codes.InvalidArgument, "selection "+
			"is required (outpoints or all)")
	}

	// Resolve the default destination (if set). It becomes the
	// wallet's req.DestOutput, i.e. the fallback for any outpoint
	// not overridden in destinations.
	var defaultOutput *wire.TxOut
	if req.DefaultDestination != nil {
		pkScript, err := r.resolveLeaveDestination(
			req.DefaultDestination,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"default_destination: %v", err)
		}

		// Under the seal-time fee handshake the server stamps
		// the binding amount on the IsChange=true output, so the
		// Value field is unused here; we leave it zero.
		defaultOutput = &wire.TxOut{PkScript: pkScript}
	}

	// Build the target set up front so the destinations loop can
	// reject keys that aren't in the selection. selection=all
	// already rejected non-empty Destinations above, so the set is
	// only consulted on the explicit-outpoints branch.
	targetSet := make(map[wire.OutPoint]struct{}, len(targets))
	for _, op := range targets {
		targetSet[op] = struct{}{}
	}

	// Resolve per-outpoint overrides. Each destination key must be
	// a valid outpoint AND must appear in the selection — a stray
	// key (typo'd index, copy-paste from another VTXO list) routes
	// silently to default_destination on a funds-moving call, so we
	// fail closed here and surface the typo as InvalidArgument
	// before we dispatch anything to the wallet.
	destOutputs := make(map[wire.OutPoint]*wire.TxOut)
	for opStr, dest := range req.Destinations {
		op, err := parseOutpointString(opStr)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"destinations: invalid outpoint %q: %v", opStr,
				err)
		}

		if _, inTargets := targetSet[op]; !inTargets {
			return nil, status.Errorf(codes.InvalidArgument,
				"destinations[%s]: outpoint not in selection",
				opStr)
		}

		pkScript, err := r.resolveLeaveDestination(dest)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"destinations[%s]: %v", opStr, err)
		}

		destOutputs[op] = &wire.TxOut{PkScript: pkScript}
	}

	// Guarantee every explicit target has some destination before
	// dispatch — the wallet surfaces a per-outpoint error for a
	// missing destination, but catching it at the RPC layer gives
	// the caller a single clean InvalidArgument instead of a
	// partially-accepted batch. For selection=all we can't
	// enumerate targets here (the wallet enumerates via
	// vtxoStore), so we just require a default destination.
	if defaultOutput == nil {
		if leaveAll {
			return nil, status.Errorf(codes.InvalidArgument,
				"default_destination is required with "+
					"selection=all")
		}

		for _, op := range targets {
			if _, ok := destOutputs[op]; !ok {
				return nil, status.Errorf(codes.InvalidArgument,
					"outpoint %s has no destination; set "+
						"default_destination or "+
						"destinations[%s]", op, op)
			}
		}
	}

	// When selection=all, enumerate live VTXOs so both the dry_run
	// preview AND the queued list below cover the full set. The
	// vtxoStore is populated independently of the wallet actor
	// (it's a SQL view), so this runs before the wallet-ready gate
	// — including under dry_run, which is the path users reach for
	// to preview a "leave all" before committing. Doing the
	// enumeration *after* the dry-run echo is the bug the H-5 fix
	// closes: it returned an empty queued_outpoints list and a
	// confused user (or LLM) reading the empty preview as a no-op
	// would re-run without --dry_run and drain every VTXO.
	if leaveAll && r.server.vtxoStore != nil {
		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list live "+
				"VTXOs: %v", err)
		}

		for _, v := range liveVTXOs {
			targets = append(targets, v.Outpoint)
		}
	}

	// For dry_run, echo the outpoints without touching the
	// wallet or the operator. Matches RefreshVTXOs semantics; the
	// short-circuit stays before the wallet-ready gate so callers
	// can validate a request (and preview the live target set
	// under selection=all) without a live wallet.
	if req.DryRun {
		outpointStrs := make([]string, 0, len(targets))
		for _, op := range targets {
			outpointStrs = append(
				outpointStrs,
				fmt.Sprintf("%s:%d", op.Hash, op.Index),
			)
		}

		return &waverpc.LeaveVTXOsResponse{
			QueuedOutpoints: outpointStrs,
			Status:          "preview",
		}, nil
	}

	// Every path below touches the wallet, so gate on it now.
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	leaveReq := &wallet.LeaveVTXOsRequest{
		TargetOutpoints: targets,
		DestOutput:      defaultOutput,
		DestOutputs:     destOutputs,
	}
	future := wRef.Ask(ctx, leaveReq)
	result := future.Await(ctx)

	leaveResp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "leave request "+
			"failed: %v", err)
	}

	resp, ok := leaveResp.(*wallet.LeaveVTXOsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", leaveResp)
	}

	// Log per-outpoint errors but don't fail the overall request.
	for op, opErr := range resp.Errors {
		r.server.log.WarnS(ctx, "VTXO leave error",
			opErr,
			slog.String("outpoint", op.String()),
		)
	}

	// Build the list of outpoints that were successfully queued.
	// For selection=all we already populated targets from the
	// vtxoStore above, so the same iteration works for both
	// selection modes.
	queued := make([]string, 0, resp.LeavingCount)
	for _, op := range targets {
		if _, hasErr := resp.Errors[op]; !hasErr {
			queued = append(
				queued, fmt.Sprintf("%s:%d", op.Hash, op.Index),
			)
		}
	}

	r.server.log.InfoS(ctx, "VTXOs queued for leave",
		slog.Int("queued_count", len(queued)),
		slog.Int("error_count", len(resp.Errors)),
	)

	return &waverpc.LeaveVTXOsResponse{
		QueuedOutpoints: queued,
		Status:          "queued",
	}, nil
}

// SendOnChain plans and submits an atomic onchain payment from VTXOs.
// The RPC enforces the wallet-API invariants on the request shape
// (exactly one of amount_sat / sweep_all set, destination present,
// destination resolves to a standard pkScript) and then dispatches to
// the wallet actor's handleSendOnChain. For sweep_all the RPC layer
// also enumerates the live VTXO set via vtxoStore so the wallet does
// not have to bypass the manager's admission gate to drain the wallet.
func (r *RPCServer) SendOnChain(ctx context.Context,
	req *waverpc.SendOnChainRequest) (*waverpc.SendOnChainResponse, error) {

	// Pure-argument validation first so a malformed request surfaces
	// InvalidArgument before any wallet-state check.
	if req.GetDestination() == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"destination is required")
	}

	pkScript, err := r.resolveLeaveDestination(req.GetDestination())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"destination: %v", err)
	}

	var (
		targetAmount btcutil.Amount
		sweepAll     bool
	)
	switch amt := req.Amount.(type) {
	case *waverpc.SendOnChainRequest_AmountSat:
		if amt.AmountSat <= 0 ||
			amt.AmountSat > int64(btcutil.MaxSatoshi) {
			return nil, status.Errorf(codes.InvalidArgument,
				"amount_sat must be between 1 and %d",
				int64(btcutil.MaxSatoshi))
		}
		targetAmount = btcutil.Amount(amt.AmountSat)

	case *waverpc.SendOnChainRequest_SweepAll:
		if !amt.SweepAll {
			return nil, status.Errorf(codes.InvalidArgument,
				"sweep_all must be true when selected")
		}
		sweepAll = true

	default:
		return nil, status.Errorf(codes.InvalidArgument, "amount "+
			"mode is required (amount_sat or sweep_all)")
	}

	// Fetch operator terms for the change VTXO descriptor + the
	// coin-selection headroom hint. Under #270 the binding fee is
	// stamped server-side; MinOperatorFee is advisory only.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch operator "+
			"terms: %v", err)
	}

	// For sweep_all, enumerate live VTXOs up front. We do this at
	// the RPC layer (not in the wallet actor) so the dry_run path
	// can echo the planned outpoint set without reserving anything,
	// and so the wallet does not need its own live-set listing seam.
	var sweepOutpoints []wire.OutPoint
	if sweepAll {
		if r.server.vtxoStore == nil {
			return nil, status.Errorf(codes.Internal, "vtxo "+
				"store not initialized")
		}

		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list live "+
				"VTXOs: %v", err)
		}
		if len(liveVTXOs) == 0 {
			return nil, status.Errorf(codes.FailedPrecondition,
				"no live VTXOs to sweep")
		}

		for _, v := range liveVTXOs {
			sweepOutpoints = append(sweepOutpoints, v.Outpoint)
		}
	}

	// Every path below touches the wallet, so gate on it now.
	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	sendReq := &wallet.SendOnChainRequest{
		DestinationPkScript: pkScript,
		TargetAmountSat:     targetAmount,
		SweepOutpoints:      sweepOutpoints,
		OperatorFee:         terms.MinOperatorFee,
		DustLimit:           terms.MinVTXOAmountFloor(),
		OperatorKey:         terms.PubKey,
		VTXOExitDelay:       terms.VTXOExitDelay,
		DryRun:              req.DryRun,
	}

	// Strip caller cancellation from the wallet Ask: SendOnChain is
	// the wallet-shaped onchain-payment one-shot, and the CLI client
	// typically disconnects as soon as this RPC returns "submitted".
	// The downstream pipeline (wallet → client-side round actor →
	// outbound mailbox publish → operator-side join validation →
	// forfeit-VTXO lookup) all inherits the actor-system per-message
	// ctx from this Ask; without WithoutCancel here the CLI exit
	// races the operator's admission lookup and the round fails with
	// "context canceled". The Await keeps the original ctx so the
	// caller can still abort waiting for the wallet response. Mirrors
	// JoinNextRound → TriggerRoundRegistration.
	askCtx := context.WithoutCancel(ctx)
	future := wRef.Ask(askCtx, sendReq)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "send on-chain "+
			"failed: %v", err)
	}

	sendResp, ok := resp.(*wallet.SendOnChainResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", resp)
	}

	// Trigger round registration on a fresh Ask boundary so the
	// IntentRequested → outbound JoinRoundRequest publish → operator
	// admission lookup chain runs under TriggerRoundRegistration's
	// detached context. This mirrors the JoinNextRound RPC pattern:
	// SendOnChain is a one-shot, so the wallet host must not be
	// asked to also call JoinNextRound separately. Skip for dry_run
	// since the wallet handler released the reservation rather than
	// committing the intent.
	if !req.DryRun {
		if err := r.server.TriggerRoundRegistration(ctx); err != nil {
			return nil, status.Errorf(codes.Internal, "trigger "+
				"round registration: %v", err)
		}
	}

	outpointStrs := make([]string, 0, len(sendResp.SelectedOutpoints))
	for _, op := range sendResp.SelectedOutpoints {
		outpointStrs = append(
			outpointStrs, fmt.Sprintf("%s:%d", op.Hash, op.Index),
		)
	}

	r.server.log.InfoS(ctx, "SendOnChain completed",
		slog.String("status", sendResp.Status.String()),
		slog.Bool("sweep_all", sweepAll),
		slog.Int64("actual_amount_sat",
			int64(sendResp.ActualAmountSat)),
		slog.Int("selected_count",
			len(sendResp.SelectedOutpoints)),
		slog.Int64("change_amount",
			int64(sendResp.ChangeAmount)),
	)

	// send_job_id is set only for a submitted intent; a dry-run preview
	// persists no intent, so its zero id maps to an empty string.
	var sendJobID string
	if sendResp.IntentID != (wallet.PendingIntentID{}) {
		sendJobID = hex.EncodeToString(sendResp.IntentID[:])
	}

	return &waverpc.SendOnChainResponse{
		ActualAmountSat:   int64(sendResp.ActualAmountSat),
		SelectedOutpoints: outpointStrs,
		Status:            sendResp.Status.String(),
		SendJobId:         sendJobID,

		// FeeSat and ChangeOutpoint are populated once the round
		// seals and the seal-time quote becomes known. The fast-
		// path response returns zero / empty here; callers that
		// need the final values look them up via the wallet
		// history once the round confirms.
		FeeSat:         0,
		ChangeOutpoint: "",
	}, nil
}

// Board triggers the client to join the next round with any confirmed
// boarding UTXOs. The RPC delegates the full flow to the wallet actor:
// balance check, VTXO amount computation, and round registration. It
// returns immediately after the wallet accepts the request; use
// ListRounds/WatchRounds to observe round progress.
func (r *RPCServer) Board(ctx context.Context, req *waverpc.BoardRequest) (
	*waverpc.BoardResponse, error) {

	if req == nil {
		req = &waverpc.BoardRequest{}
	}

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	// Fetch operator terms so the wallet can compute the VTXO
	// output amount after deducting fees.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to fetch "+
			"operator terms: %v", err)
	}

	// Delegate to the wallet actor which handles balance
	// checking, VTXO amount computation, and forwarding the
	// TriggerBoardMsg to the round actor.
	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}
	wRef := r.server.walletRef.UnsafeFromSome()

	// Under the #270 seal-time fee handshake the server is the
	// fee authority — the client no longer pre-computes or pre-
	// deducts an operator fee at submit time. The wallet ships
	// the full confirmed boarding balance; the server stamps the
	// residual into the boarding VTXO output when the round
	// seals. Any CLI / UX fee preview is produced by the
	// EstimateFee RPC, not by the Board admission path.
	_ = terms

	boardReq := &wallet.BoardRequest{
		TargetVTXOCount: req.GetTargetVtxoCount(),
		NoPersist:       req.GetNoPersist(),
	}

	future := wRef.Ask(ctx, boardReq)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		// Expected conditions map to a success response with
		// a descriptive status rather than a gRPC error.
		errStr := err.Error()
		switch {
		case strings.Contains(errStr, "no confirmed boarding"),
			strings.Contains(errStr, "no inputs to register"):

			r.server.log.InfoS(
				ctx, "Board skipped: no boarding UTXOs",
			)

			r.server.emitMetric(ctx, &metrics.BoardingEventMsg{
				Status: "skipped",
			})

			return &waverpc.BoardResponse{
				Status: "no_boarding_utxos",
			}, nil

		case strings.Contains(errStr, "too small after"):
			r.server.emitMetric(ctx, &metrics.BoardingEventMsg{
				Status: "failed",
			})

			return nil, status.Errorf(codes.FailedPrecondition,
				"boarding balance too small: %v", err)
		}

		r.server.emitMetric(ctx, &metrics.BoardingEventMsg{
			Status: "failed",
		})

		return nil, status.Errorf(codes.Internal, "board failed: %v",
			err)
	}

	boardResp, ok := resp.(*wallet.BoardResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected board "+
			"response type: %T", resp)
	}

	r.server.log.InfoS(ctx, "Board request accepted",
		btclog.Fmt("boarding_balance", "%v",
			boardResp.BoardingBalance),
		btclog.Fmt("vtxo_amount", "%v",
			boardResp.VTXOAmount),
		slog.Int("vtxo_count", len(boardResp.VTXOAmounts)))

	r.server.emitMetric(ctx, &metrics.BoardingEventMsg{
		Status: "submitted",
	})

	return &waverpc.BoardResponse{
		Status:    "registered",
		VtxoCount: uint32(len(boardResp.VTXOAmounts)),
	}, nil
}

// JoinNextRound asks the client round actor to commit any queued round
// intents by injecting IntentRequested into the active assembling FSM. The
// FSM then emits a JoinRoundRequest to the operator and drives the
// registration handshake to completion on its own turn loop.
func (r *RPCServer) JoinNextRound(ctx context.Context,
	_ *waverpc.JoinNextRoundRequest) (*waverpc.JoinNextRoundResponse,
	error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if err := r.server.TriggerRoundRegistration(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "join next round: %v",
			err)
	}

	r.server.log.InfoS(ctx, "JoinNextRound accepted")

	// rounds_joined_total is emitted by the round actor in
	// createNewRound (symmetric with rounds_completed_total), which
	// covers both this manual trigger and eager/automatic joins.
	// Emitting here too would double-count manual joins.

	return &waverpc.JoinNextRoundResponse{
		Status: "joined",
	}, nil
}

// SendVTXO initiates an in-round directed transfer by forfeiting
// existing VTXOs and creating new recipient VTXOs in the same round.
// Coin selection, reservation, and round registration are handled
// atomically by the wallet actor.
func (r *RPCServer) SendVTXO(ctx context.Context,
	req *waverpc.SendVTXORequest) (*waverpc.SendVTXOResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	// TODO(#241): Tune this cap based on round tree constraints
	// and consider making it configurable.
	const maxRecipients = 256

	if len(req.Recipients) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "at least "+
			"one recipient is required")
	}

	if len(req.Recipients) > maxRecipients {
		return nil, status.Errorf(codes.InvalidArgument, "too many "+
			"recipients: %d (max %d)", len(req.Recipients),
			maxRecipients)
	}

	// Resolve each recipient's pkScript and client pubkey from
	// the proto Output destination.
	recipients := make(
		[]wallet.SendRecipient, 0, len(req.Recipients),
	)
	var totalAmount int64

	for i, out := range req.Recipients {
		if out.GetDestination() == nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"recipient %d: destination is required", i)
		}

		if out.AmountSat <= 0 ||
			out.AmountSat > int64(btcutil.MaxSatoshi) {
			return nil, status.Errorf(codes.InvalidArgument,
				"recipient %d: amount must be between 1 and %d",
				i, int64(btcutil.MaxSatoshi))
		}

		// Overflow-safe addition.
		if totalAmount > int64(btcutil.MaxSatoshi)-out.AmountSat {
			return nil, status.Errorf(codes.InvalidArgument,
				"total amount overflows max supply")
		}

		pkScript, clientKey, err := r.resolveRecipientOutput(
			out,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"recipient %d: %v", i, err)
		}

		recipients = append(recipients, wallet.SendRecipient{
			PkScript:  pkScript,
			Amount:    btcutil.Amount(out.AmountSat),
			ClientKey: clientKey,
		})

		totalAmount += out.AmountSat
	}

	// Fetch operator terms for fee, dust limit, exit delay, and
	// operator key.
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	// Quote the dynamic operator fee for this send. SendVTXO
	// Under the #270 seal-time fee handshake the operator fee is
	// decided by the server when the round seals, not at submit
	// time. Use the operator's stated MinOperatorFee as a
	// conservative coin-selection hint so the wallet reserves
	// enough input value to leave room for the seal-time residual
	// to land on the self-change output. A stale / high hint
	// merely over-selects (excess lands as change); it cannot
	// reject the send on a dust threshold because the wallet path
	// treats OperatorFee as advisory.
	sendReq := &wallet.SendVTXOsRequest{
		Recipients:    recipients,
		OperatorFee:   terms.MinOperatorFee,
		DustLimit:     terms.MinVTXOAmountFloor(),
		OperatorKey:   terms.PubKey,
		VTXOExitDelay: terms.VTXOExitDelay,
		DryRun:        req.DryRun,
	}

	future := wRef.Ask(ctx, sendReq)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(vtxoAdmissionCode(err), "send "+
			"failed: %v", err)
	}

	sendResp, ok := resp.(*wallet.SendVTXOsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", resp)
	}

	r.server.log.InfoS(ctx, "SendVTXO completed",
		slog.String("status", sendResp.Status),
		slog.Int("selected_count",
			sendResp.SelectedCount),
		slog.Int64("total_selected",
			int64(sendResp.TotalSelected)),
		slog.Int64("change", int64(sendResp.ChangeAmount)))

	return &waverpc.SendVTXOResponse{
		Status:          sendResp.Status,
		TotalAmountSat:  totalAmount,
		ChangeAmountSat: int64(sendResp.ChangeAmount),
		SelectedCount:   int32(sendResp.SelectedCount),
	}, nil
}

// SendOOR initiates an out-of-round transfer directly between the
// client and operator, without waiting for a round. The transfer
// completes asynchronously via the OOR protocol.
//
//nolint:funlen
func (r *RPCServer) SendOOR(ctx context.Context, req *waverpc.SendOORRequest) (
	*waverpc.SendOORResponse, error) {

	startTime := time.Now()
	var (
		idempotencyDuration   time.Duration
		resolveScriptDuration time.Duration
		operatorTermsDuration time.Duration
		inputSelectDuration   time.Duration
		buildInputsDuration   time.Duration
		changeOutputDuration  time.Duration
		oorActorDuration      time.Duration
	)

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	requestRecipients, err := sendOORRequestRecipients(req)
	if err != nil {
		return nil, err
	}

	targetAmt, err := sumSendOORRecipientAmounts(requestRecipients)
	if err != nil {
		return nil, err
	}

	if req.GetIdempotencyKey() != "" && !req.DryRun {
		phaseStart := time.Now()
		key := req.GetIdempotencyKey()
		sessionID, found, err :=
			r.findOutgoingOORSessionByIdempotencyKey(ctx, key)
		idempotencyDuration = time.Since(phaseStart)
		if err != nil {
			return nil, err
		}

		if found {
			r.server.log.InfoS(
				ctx,
				"Returning existing OOR transfer",
				slog.String("session_id", sessionID.String()),
				slog.String("idempotency_key", key),
			)

			// This is an idempotent replay: the original SendOOR
			// already counted the submission, so we deliberately do
			// NOT emit OORTransferSentMsg here. Counting replays
			// would inflate oor_transfers_sent_total under any
			// client retry loop even though no new transfer was
			// initiated.
			//
			// This fast path returns before resolving the current
			// request, so the recipient outpoint is intentionally
			// omitted. Full request replay below can recompute it.
			return &waverpc.SendOORResponse{
				Status:    "submitted",
				SessionId: sessionID.String(),
			}, nil
		}
	}

	// Fetch operator terms for the checkpoint policy.
	phaseStart := time.Now()
	terms, err := r.server.fetchOperatorTerms(ctx)
	operatorTermsDuration = time.Since(phaseStart)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	// Resolve the recipients' pkScripts from the destination oneofs
	// and bind any supplied semantic policy templates to those scripts.
	phaseStart = time.Now()
	oorRecipients, err := r.buildSendOORRecipients(
		ctx, requestRecipients, terms,
	)
	resolveScriptDuration = time.Since(phaseStart)
	if err != nil {
		return nil, err
	}

	if len(req.CustomInputs) > 0 && len(oorRecipients) != 1 {
		return nil, status.Errorf(codes.InvalidArgument, "custom "+
			"inputs require exactly one recipient")
	}

	// For dry_run, return a preview before selecting wallet inputs or
	// submitting to the actor. The operator is still queried above so the
	// preview enforces the current dust limit.
	if req.DryRun {
		return &waverpc.SendOORResponse{
			Status: "preview",
		}, nil
	}

	if r.server.actorSystem == nil {
		return nil, status.Errorf(codes.Internal, "actor system not "+
			"initialized")
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "VTXO store not "+
			"initialized")
	}

	policy := arkscript.CheckpointPolicy{
		OperatorKey: terms.PubKey,
		CSVDelay:    terms.VTXOExitDelay,
	}

	var (
		selectedInputs      []oor.TransferInput
		locked              *wallet.SelectAndLockVTXOsResponse
		customInputsRelease func()
		releaseCustomInputs bool
	)

	if len(req.CustomInputs) > 0 {
		phaseStart = time.Now()
		// Custom inputs provided — bypass wallet selection and
		// build TransferInputs from the specified VTXOs.
		//
		// Custom inputs typically refer to non-standard VTXOs
		// (e.g. vHTLC claim outpoints) that live outside the
		// wallet's managed set and therefore are not reserved
		// through the VTXO manager's PendingForfeit flow.
		// Without any dedup, two concurrent SendOOR callers
		// supplying the same custom input could each succeed
		// through BuildCustomTransferInputs and double-sign the
		// same outpoint. We gate that race here with an
		// in-memory reservation keyed by outpoint; the
		// reservation is released regardless of success or
		// failure via defer, unless the detached OOR actor submit has
		// already been accepted and the caller only stopped waiting for
		// its response. In that case the release is deferred until the
		// actor future actually completes.
		customOutpoints := make(
			[]wire.OutPoint, 0, len(req.CustomInputs),
		)
		for _, ci := range req.CustomInputs {
			op, err := parseOutpointString(ci.Outpoint)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"parse custom input outpoint %q: %v",
					ci.Outpoint, err)
			}

			customOutpoints = append(customOutpoints, op)
		}

		release, err := r.reserveCustomInputs(customOutpoints)
		if err != nil {
			return nil, status.Errorf(codes.Aborted, "custom "+
				"input double-use: %v", err)
		}
		customInputsRelease = release
		releaseCustomInputs = true
		defer func() {
			if releaseCustomInputs && customInputsRelease != nil {
				customInputsRelease()
			}
		}()
		inputSelectDuration = time.Since(phaseStart)

		phaseStart = time.Now()
		selectedInputs, err = BuildCustomTransferInputs(
			ctx, r.server.vtxoStore, req.CustomInputs,
			r.server.clientKeyDesc, terms.PubKey,
			terms.VTXOExitDelay,
		)
		buildInputsDuration = time.Since(phaseStart)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "build "+
				"custom inputs: %v", err)
		}
		err = r.requireCustomSpendsMature(ctx, selectedInputs)
		if err != nil {
			return nil, err
		}
	} else {
		// Standard path: select and lock VTXOs from wallet.
		phaseStart = time.Now()
		wRef := r.server.walletRef.UnsafeFromSome()

		selectReq := &wallet.SelectAndLockVTXOsRequest{
			TargetAmount:    targetAmt,
			MinChangeAmount: terms.MinVTXOAmountFloor(),
		}
		selectFuture := wRef.Ask(ctx, selectReq)
		selectResult := selectFuture.Await(ctx)

		selectResp, err := selectResult.Unpack()
		if err != nil {
			return nil, status.Errorf(vtxoAdmissionCode(err),
				"VTXO selection failed: %v", err)
		}

		var ok bool
		locked, ok = selectResp.(*wallet.SelectAndLockVTXOsResponse)
		if !ok {
			return nil, status.Errorf(codes.Internal, "unexpected "+
				"response type: %T", selectResp)
		}
		inputSelectDuration = time.Since(phaseStart)

		outpoints := make(
			[]wire.OutPoint, 0, len(locked.SelectedVTXOs),
		)
		for _, sv := range locked.SelectedVTXOs {
			outpoints = append(
				outpoints, sv.Outpoint,
			)
		}

		phaseStart = time.Now()
		selectedInputs, err = BuildTransferInputs(
			ctx, r.server.vtxoStore, outpoints,
		)
		buildInputsDuration = time.Since(phaseStart)
		if err != nil {
			r.unlockSelectedVTXOsBestEffort(ctx, locked)

			return nil, status.Errorf(codes.Internal, "build "+
				"transfer inputs: %v", err)
		}
	}

	requestOORRecipients := append(
		[]oortx.RecipientOutput(nil), oorRecipients...,
	)
	// Keep the request copy change-free so response outpoint resolution
	// maps only caller-requested recipients back into the finalized Ark tx.
	recipients := requestOORRecipients

	phaseStart = time.Now()
	inputTotal, err := sumOORInputAmounts(selectedInputs)
	if err != nil {
		r.unlockSelectedVTXOsBestEffort(ctx, locked)

		return nil, status.Errorf(codes.Internal, "sum OOR input "+
			"amounts: %v", err)
	}

	recipients, changeAmt, err := appendOORChangeRecipient(
		ctx, recipients, inputTotal, terms.MinVTXOAmountFloor(),
		func(ctx context.Context, change btcutil.Amount) (
			oortx.RecipientOutput, error) {

			return r.buildOORChangeRecipient(
				ctx, terms.PubKey, terms.VTXOExitDelay, change,
			)
		},
	)
	if err != nil {
		r.unlockSelectedVTXOsBestEffort(ctx, locked)

		return nil, err
	}
	changeOutputDuration = time.Since(phaseStart)

	// Resolve the OOR actor via the service key registered in the
	// actor system's receptionist. This avoids holding a direct
	// reference and is the canonical way to interact with
	// service-registered actors.
	oorKey := oor.NewServiceKey()
	oorRef := oorKey.Ref(r.server.actorSystem)

	oorReq := &oor.StartTransferRequest{
		Policy:         policy,
		Inputs:         selectedInputs,
		Recipients:     recipients,
		IdempotencyKey: req.GetIdempotencyKey(),
	}

	phaseStart = time.Now()
	oorCtx := actor.WithoutTx(context.WithoutCancel(ctx))
	future := oorRef.Ask(oorCtx, oorReq)
	oorResult := future.Await(ctx)
	oorActorDuration = time.Since(phaseStart)

	oorResp, err := oorResult.Unpack()
	if err != nil {
		if isAwaitContextError(ctx, err) {
			releaseCustomInputs = false
			r.cleanupSubmittedOORStart(
				ctx, future, locked, customInputsRelease,
			)

			code := codes.Canceled
			if errors.Is(err, context.DeadlineExceeded) {
				code = codes.DeadlineExceeded
			}

			return nil, status.Errorf(code, "OOR transfer "+
				"response wait: %v", err)
		}

		// Unlock VTXOs on OOR failure so they can be
		// reused (only for wallet-selected inputs).
		r.unlockSelectedVTXOsBestEffort(ctx, locked)

		r.server.emitMetric(ctx, &metrics.OORTransferSentMsg{
			Status:   "failed",
			Duration: time.Since(startTime),
		})

		return nil, status.Errorf(codes.Internal, "OOR transfer "+
			"failed: %v", err)
	}

	resp, ok := oorResp.(*oor.StartTransferResponse)
	if !ok {
		r.unlockSelectedVTXOsBestEffort(ctx, locked)

		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type: %T", oorResp)
	}

	if resp.Existing {
		r.unlockSelectedVTXOsBestEffort(ctx, locked)
	}

	recipientOutpoints := r.resolveOORRecipientOutpoints(
		ctx, resp.SessionID, recipients, requestOORRecipients,
	)
	r.server.log.InfoS(ctx, "OOR transfer submitted",
		slog.String("session_id", resp.SessionID.String()),
		slog.Bool("existing_session", resp.Existing),
		slog.Int("recipient_outpoint_count", len(recipientOutpoints)),
		slog.Int64("amount_sat", int64(targetAmt)),
		slog.Int64("input_total_sat", int64(inputTotal)),
		slog.Int64("change_sat", int64(changeAmt)),
		slog.Int("recipient_count", len(requestOORRecipients)),
		slog.Int("output_count", len(recipients)),
		slog.Duration("duration", time.Since(startTime)),
		slog.Duration("idempotency_duration", idempotencyDuration),
		slog.Duration("resolve_script_duration",
			resolveScriptDuration),
		slog.Duration("operator_terms_duration",
			operatorTermsDuration),
		slog.Duration("input_select_duration",
			inputSelectDuration),
		slog.Duration("build_inputs_duration",
			buildInputsDuration),
		slog.Duration("change_output_duration",
			changeOutputDuration),
		slog.Duration("oor_actor_duration", oorActorDuration))

	r.server.emitMetric(ctx, &metrics.OORTransferSentMsg{
		SessionID: resp.SessionID.String(),
		Status:    "submitted",
		Duration:  time.Since(startTime),
	})

	return &waverpc.SendOORResponse{
		Status:             "submitted",
		SessionId:          resp.SessionID.String(),
		RecipientOutpoints: recipientOutpoints,
	}, nil
}

// requireCustomSpendsMature rejects custom OOR inputs whose absolute
// block-height locktime is still in the future. Timestamp locktimes are not
// currently supported at this RPC boundary because the daemon only has the
// current best height in this path.
func (r *RPCServer) requireCustomSpendsMature(ctx context.Context,
	inputs []oor.TransferInput) error {

	var requiredLockTime uint32
	for _, input := range inputs {
		if input.CustomSpend == nil {
			continue
		}

		if input.CustomSpend.RequiredLockTime > requiredLockTime {
			requiredLockTime = input.CustomSpend.RequiredLockTime
		}
	}

	if requiredLockTime == 0 || r.server.chainBackend == nil {
		return nil
	}

	if requiredLockTime >= txscript.LockTimeThreshold {
		return status.Errorf(codes.InvalidArgument, "custom input "+
			"timestamp locktime %d is not supported",
			requiredLockTime)
	}

	height, _, err := r.server.chainBackend.BestBlock(ctx)
	if err != nil {
		return status.Errorf(codes.Unavailable, "fetch block "+
			"height: %v", err)
	}

	if uint32(height) < requiredLockTime {
		return status.Errorf(codes.FailedPrecondition, "custom input "+
			"spend locktime %d is not mature at height %d",
			requiredLockTime, height)
	}

	return nil
}

// PrepareOOR builds a deterministic OOR package without submitting it.
func (r *RPCServer) PrepareOOR(ctx context.Context,
	req *waverpc.PrepareOORRequest) (*waverpc.PrepareOORResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req.GetRecipient() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"is required")
	}

	if req.GetRecipient().GetAmountSat() <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount "+
			"must be positive")
	}

	if len(req.GetCustomInputs()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "custom "+
			"inputs are required")
	}

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "VTXO store not "+
			"initialized")
	}

	pkScript, err := r.resolveOutputPkScript(ctx, req.GetRecipient())
	if err != nil {
		return nil, err
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	if err := validateOORRecipientFloor(
		req.GetRecipient().GetAmountSat(), terms.MinVTXOAmountFloor(),
	); err != nil {
		return nil, err
	}

	recipientPolicyTemplate, err := r.resolveOutputPolicyTemplate(
		ctx, req.GetRecipient(), pkScript, terms.PubKey,
		terms.VTXOExitDelay,
	)
	if err != nil {
		return nil, err
	}

	selectedInputs, err := BuildCustomTransferInputs(
		ctx, r.server.vtxoStore, req.GetCustomInputs(),
		r.server.clientKeyDesc, terms.PubKey, terms.VTXOExitDelay,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build custom "+
			"inputs: %v", err)
	}

	inputTotal, err := sumOORInputAmounts(selectedInputs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sum OOR input "+
			"amounts: %v", err)
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: pkScript,
		Value: btcutil.Amount(
			req.GetRecipient().GetAmountSat(),
		),
		VTXOPolicyTemplate: recipientPolicyTemplate,
	}}

	recipients, _, err = appendOORChangeRecipient(
		ctx, recipients, inputTotal, terms.MinVTXOAmountFloor(),
		func(ctx context.Context, change btcutil.Amount) (
			oortx.RecipientOutput, error) {

			return r.buildOORChangeRecipient(
				ctx, terms.PubKey, terms.VTXOExitDelay, change,
			)
		},
	)
	if err != nil {
		return nil, err
	}

	policy := arkscript.CheckpointPolicy{
		OperatorKey: terms.PubKey,
		CSVDelay:    terms.VTXOExitDelay,
	}
	arkPSBT, checkpointPSBTs, err := oor.BuildSubmitPackage(
		policy, selectedInputs, recipients,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build OOR "+
			"package: %v", err)
	}

	arkRaw, err := psbtutil.Serialize(arkPSBT)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "serialize ark "+
			"psbt: %v", err)
	}

	checkpointRaw := make([][]byte, 0, len(checkpointPSBTs))
	preparedInputs := make(
		[]*waverpc.PreparedOORCustomInput, 0, len(selectedInputs),
	)
	for i := range checkpointPSBTs {
		raw, err := psbtutil.Serialize(checkpointPSBTs[i])
		if err != nil {
			return nil, status.Errorf(codes.Internal, "serialize "+
				"checkpoint psbt %d: %v", i, err)
		}
		checkpointRaw = append(checkpointRaw, raw)

		input := selectedInputs[i]
		if input.CustomSpend == nil {
			continue
		}

		signingPubKeys := make([][]byte, 0, len(input.CustomSpendKeys))
		for _, key := range input.CustomSpendKeys {
			if key == nil {
				return nil, status.Errorf(codes.Internal,
					"custom spend key is nil")
			}

			signingPubKeys = append(
				signingPubKeys, key.SerializeCompressed(),
			)
		}

		preparedInputs = append(
			preparedInputs, &waverpc.PreparedOORCustomInput{
				Outpoint:       input.VTXO.Outpoint.String(),
				CheckpointPsbt: raw,
				WitnessScript: input.CustomSpend.SpendInfo.
					WitnessScript,
				SigningPubkeys: signingPubKeys,
			},
		)
	}

	return &waverpc.PrepareOORResponse{
		ArkPsbt:         arkRaw,
		CheckpointPsbts: checkpointRaw,
		CustomInputs:    preparedInputs,
		SessionId:       arkPSBT.UnsignedTx.TxHash().String(),
	}, nil
}

// SignOORCustomInput signs one prepared custom OOR checkpoint input.
func (r *RPCServer) SignOORCustomInput(ctx context.Context,
	req *waverpc.SignOORCustomInputRequest) (
	*waverpc.SignOORCustomInputResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req.GetCustomInput() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "custom "+
			"input is required")
	}

	if r.server.vtxoStore == nil {
		return nil, status.Errorf(codes.Internal, "VTXO store not "+
			"initialized")
	}

	checkpoint, err := psbtutil.Parse(req.GetCheckpointPsbt())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse "+
			"checkpoint psbt: %v", err)
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	inputs, err := BuildCustomTransferInputs(
		ctx, r.server.vtxoStore,
		[]*waverpc.CustomOORInput{req.GetCustomInput()},
		r.server.clientKeyDesc, terms.PubKey, terms.VTXOExitDelay,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build custom "+
			"input: %v", err)
	}
	if len(inputs) != 1 {
		return nil, status.Errorf(codes.Internal, "expected one "+
			"custom input, got %d", len(inputs))
	}

	signer, err := r.oorSigner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve signer: %v",
			err)
	}

	sig, err := oor.SignCustomCheckpointInput(
		signer, &inputs[0], checkpoint,
	)
	if err != nil {
		if errors.Is(err, oor.ErrCustomCheckpointInputSigning) {
			return nil, status.Errorf(codes.Internal, "sign "+
				"custom input: %v", err)
		}

		return nil, status.Errorf(codes.InvalidArgument, "sign custom "+
			"input: %v", err)
	}

	return &waverpc.SignOORCustomInputResponse{
		Signature: &waverpc.TaprootScriptSignature{
			Pubkey:        sig.PubKey.SerializeCompressed(),
			WitnessScript: sig.WitnessScript,
			Signature:     sig.Signature,
			Sighash:       uint32(sig.SigHash),
		},
	}, nil
}

// SignVTXOForfeit signs the VTXO input of an exact round forfeit transaction.
func (r *RPCServer) SignVTXOForfeit(ctx context.Context,
	req *waverpc.SignVTXOForfeitRequest) (*waverpc.SignVTXOForfeitResponse,
	error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	vtxoOutpoint, err := parseOutpointString(req.GetVtxoOutpoint())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse "+
			"outpoint: %v", err)
	}

	connectorOutpoint, err := parseOutpointString(
		req.GetConnectorOutpoint(),
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse "+
			"connector outpoint: %v", err)
	}

	if req.GetVtxoAmountSat() <= 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"vtxo_amount_sat must be positive")
	}
	if req.GetConnectorAmountSat() < 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"connector_amount_sat must be non-negative")
	}
	if req.GetConnectorAmountSat() > 0 &&
		req.GetVtxoAmountSat() >
			(1<<63-1)-req.GetConnectorAmountSat() {
		return nil, status.Errorf(codes.InvalidArgument, "forfeit "+
			"amount overflow")
	}
	if len(req.GetVtxoPkScript()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"vtxo_pk_script is required")
	}
	if len(req.GetConnectorPkScript()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"connector_pk_script is required")
	}
	if len(req.GetServerForfeitPkScript()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"server_forfeit_pk_script is required")
	}
	if len(req.GetVtxoPolicyTemplate()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"vtxo_policy_template is required")
	}
	if len(req.GetSpendPath()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "spend_path "+
			"is required")
	}
	if len(req.GetUnsignedForfeitTx()) == 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"unsigned_forfeit_tx is required")
	}

	if err := r.validateSignVTXOForfeitLocalVTXOIfPresent(
		ctx, vtxoOutpoint, req,
	); err != nil {
		return nil, err
	}

	template, err := arkscript.DecodePolicyTemplate(
		req.GetVtxoPolicyTemplate(),
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode "+
			"policy template: %v", err)
	}

	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}
	// vHTLC policies have swap-specific CSV delays that can be lower than
	// the operator's standard VTXO exit delay. This RPC validates the
	// structural Ark policy invariants and the operator key, but leaves the
	// delay policy to the swap-specific authorization layer.
	if err := template.ValidateArkPolicy(arkscript.PolicyValidationOpts{
		OperatorKey: terms.PubKey,
	}); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "validate "+
			"policy template: %v", err)
	}
	if !template.MatchesPkScript(req.GetVtxoPkScript()) {
		return nil, status.Errorf(codes.InvalidArgument, "policy "+
			"template does not match vtxo_pk_script")
	}

	spendPath, err := arkscript.DecodeSpendPath(req.GetSpendPath())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode "+
			"spend path: %v", err)
	}
	err = spendPath.VerifyBindsToPkScript(req.GetVtxoPkScript())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "spend path "+
			"does not bind to vtxo_pk_script: %v", err)
	}

	signingKeys, err := arkscript.SigningKeysForSpendPath(
		template, spendPath,
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "resolve "+
			"signing keys: %v", err)
	}
	if !containsSigningKey(signingKeys, r.server.clientKeyDesc.PubKey) {
		return nil, status.Errorf(codes.InvalidArgument, "daemon "+
			"identity key is not required by spend path")
	}

	forfeitTx, err := parseUnsignedForfeitTx(req.GetUnsignedForfeitTx())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse "+
			"forfeit tx: %v", err)
	}
	if err := arktx.ValidateForfeitTx(forfeitTx, arktx.ForfeitTxParams{
		VTXOOutpoint:        vtxoOutpoint,
		ConnectorOutpoint:   connectorOutpoint,
		ServerForfeitScript: req.GetServerForfeitPkScript(),
		ExpectedAmount: btcutil.Amount(
			req.GetVtxoAmountSat() + req.GetConnectorAmountSat(),
		),
		ExpectedSequence: spendPath.RequiredSequence,
		ExpectedLockTime: spendPath.RequiredLockTime,
	}); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "validate "+
			"forfeit tx: %v", err)
	}

	vtxoOutput := &wire.TxOut{
		Value:    req.GetVtxoAmountSat(),
		PkScript: bytes.Clone(req.GetVtxoPkScript()),
	}
	connectorOutput := &wire.TxOut{
		Value:    req.GetConnectorAmountSat(),
		PkScript: bytes.Clone(req.GetConnectorPkScript()),
	}
	prevFetcher, err := arktx.NewForfeitPrevOutFetcher(
		&arktx.VTXOSpendContext{
			Outpoint: vtxoOutpoint,
			Output:   vtxoOutput,
		},
		&arktx.ConnectorSpendContext{
			Outpoint: connectorOutpoint,
			Output:   connectorOutput,
		},
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "prevout "+
			"fetcher: %v", err)
	}

	signer, err := r.oorSigner()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve signer: %v",
			err)
	}

	sigHashes := txscript.NewTxSigHashes(forfeitTx, prevFetcher)
	signDesc := spendPath.SpendInfo.BuildSignDescriptor(
		r.server.clientKeyDesc, vtxoOutput, sigHashes, prevFetcher,
		arktx.ForfeitVTXOInputIndex,
	)
	sig, err := signer.SignOutputRaw(forfeitTx, signDesc)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign forfeit tx: %v",
			err)
	}
	if sig == nil {
		return nil, status.Errorf(codes.Internal, "sign forfeit tx: "+
			"empty signature")
	}

	return &waverpc.SignVTXOForfeitResponse{
		Pubkey:    r.server.clientKeyDesc.PubKey.SerializeCompressed(),
		Signature: sig.Serialize(),
	}, nil
}

// validateSignVTXOForfeitLocalVTXOIfPresent checks the request transcript
// against local VTXO state when this daemon owns the VTXO. A missing local row
// is allowed because this RPC is also used for external participants in a
// multi-sig spend path; those calls are still bound by the policy template,
// spend path, exact forfeit transaction, and local-key requirement below.
func (r *RPCServer) validateSignVTXOForfeitLocalVTXOIfPresent(
	ctx context.Context, outpoint wire.OutPoint,
	req *waverpc.SignVTXOForfeitRequest) error {

	if r.server.vtxoStore == nil {
		return status.Errorf(codes.Internal, "vtxo store not "+
			"initialized")
	}

	desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil

	case err != nil:
		return status.Errorf(codes.Internal, "load local vtxo %s: %v",
			outpoint, err)

	case desc == nil:
		return nil
	}

	if int64(desc.Amount) != req.GetVtxoAmountSat() {
		return status.Errorf(codes.InvalidArgument, "vtxo_amount_sat "+
			"does not match local vtxo")
	}
	if !bytes.Equal(desc.PkScript, req.GetVtxoPkScript()) {
		return status.Errorf(codes.InvalidArgument, "vtxo_pk_script "+
			"does not match local vtxo")
	}
	if !bytes.Equal(
		desc.PolicyTemplate, req.GetVtxoPolicyTemplate(),
	) {
		return status.Errorf(codes.InvalidArgument,
			"vtxo_policy_template does not match local vtxo")
	}

	return nil
}

func parseUnsignedForfeitTx(raw []byte) (*wire.MsgTx, error) {
	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}

	for i, txIn := range tx.TxIn {
		if len(txIn.SignatureScript) > 0 {
			return nil, fmt.Errorf("input %d has signature script",
				i)
		}
		if len(txIn.Witness) > 0 {
			return nil, fmt.Errorf("input %d has witness", i)
		}
	}

	return tx, nil
}

func containsSigningKey(keys []*btcec.PublicKey, target *btcec.PublicKey) bool {
	if target == nil {
		return false
	}

	targetX := schnorr.SerializePubKey(target)
	for _, key := range keys {
		if key == nil {
			continue
		}

		if bytes.Equal(schnorr.SerializePubKey(key), targetX) {
			return true
		}
	}

	return false
}

// oorSigner returns the wallet signer used for OOR checkpoint inputs.
func (r *RPCServer) oorSigner() (input.Signer, error) {
	if r.oorSignerOverride != nil {
		return r.oorSignerOverride, nil
	}

	return r.server.oorSigner()
}

// findOutgoingOORSessionByIdempotencyKey checks the durable session registry
// for a live keyed outgoing session before acquiring wallet or custom inputs
// for the retry. The lookup is a direct store read: the registry actor owns
// all writes, while failed sessions are excluded so a retry after a failure
// proceeds to a fresh admission.
func (r *RPCServer) findOutgoingOORSessionByIdempotencyKey(ctx context.Context,
	idempotencyKey string) (oor.SessionID, bool, error) {

	store := r.server.oorSessionStore
	if store == nil {
		return oor.SessionID{}, false, status.Errorf(codes.Internal,
			"OOR session store not initialized")
	}

	record, err := store.LookupActiveSessionByIdempotencyKey(
		ctx, idempotencyKey,
	)
	switch {
	case errors.Is(err, db.ErrOORSessionNotFound):
		return oor.SessionID{}, false, nil

	case err != nil:
		return oor.SessionID{}, false, status.Errorf(codes.Internal,
			"OOR idempotency lookup failed: %v", err)
	}

	return oor.SessionID(record.SessionID), true, nil
}

// buildOORChangeRecipient allocates and registers a wallet-owned receive
// script for an OOR change output.
func (r *RPCServer) buildOORChangeRecipient(ctx context.Context,
	operatorKey *btcec.PublicKey, exitDelay uint32, change btcutil.Amount) (
	oortx.RecipientOutput, error) {

	if r.server.indexer == nil {
		return oortx.RecipientOutput{}, status.Errorf(codes.Internal,
			"indexer client not initialized")
	}

	store, err := r.newOORReceiveScriptStore()
	if err != nil {
		return oortx.RecipientOutput{}, status.Errorf(codes.Internal,
			"unable to initialize OOR receive-script store: %v",
			err)
	}

	deriveNextKey, signerFactory, err := r.oorReceiveKeyOps()
	if err != nil {
		return oortx.RecipientOutput{}, status.Errorf(codes.Internal,
			"unable to initialize OOR receive key ops: %v", err)
	}

	keyDesc, pkScript, err := CreateOORReceiveScript(
		ctx, r.server.indexer, store, deriveNextKey, signerFactory,
		operatorKey, exitDelay, defaultOORChangeScriptLabel,
	)
	if err != nil {
		return oortx.RecipientOutput{}, status.Errorf(codes.Internal,
			"unable to create OOR change script: %v", err)
	}

	if keyDesc == nil || keyDesc.PubKey == nil {
		return oortx.RecipientOutput{}, status.Errorf(codes.Internal,
			"missing OOR change key descriptor")
	}

	policyTemplate, err := encodeStandardRecipientPolicy(
		keyDesc.PubKey, operatorKey, exitDelay, pkScript,
	)
	if err != nil {
		return oortx.RecipientOutput{}, err
	}

	return oortx.RecipientOutput{
		PkScript:           pkScript,
		Value:              change,
		VTXOPolicyTemplate: policyTemplate,
	}, nil
}

// unlockSelectedVTXOsBestEffort releases wallet-selected inputs after an OOR
// setup failure. Custom inputs are released by reserveCustomInputs' defer path.
func (r *RPCServer) unlockSelectedVTXOsBestEffort(ctx context.Context,
	locked *wallet.SelectAndLockVTXOsResponse) {

	if locked == nil || len(locked.SelectedVTXOs) == 0 {
		return
	}

	unlockErr := r.unlockVTXOs(ctx, locked.SelectedVTXOs)
	if unlockErr != nil {
		r.server.log.ErrorS(ctx, "Unable to unlock VTXOs", unlockErr)
	}
}

// isAwaitContextError returns true when a future await stopped because the
// caller's wait context ended, rather than because the submitted actor work
// completed with a real result.
func isAwaitContextError(ctx context.Context, err error) bool {
	if ctx.Err() == nil {
		return false
	}

	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// cleanupSubmittedOORStart waits for a detached OOR start future to finish
// after the RPC caller stopped waiting. Once the actor has actually completed,
// cleanup follows the same rules as the synchronous path: wallet-selected
// inputs are unlocked only on actor failure or duplicate/existing sessions,
// while custom input reservations are released when the in-flight actor start
// is no longer using the RPC-level double-use guard.
func (r *RPCServer) cleanupSubmittedOORStart(ctx context.Context,
	future actor.Future[oor.ActorResp],
	locked *wallet.SelectAndLockVTXOsResponse, releaseCustomInputs func()) {

	r.cleanupSubmittedOORStartWithTimeout(
		ctx, future, locked, releaseCustomInputs,
		submittedOORCleanupTimeout,
	)
}

func (r *RPCServer) cleanupSubmittedOORStartWithTimeout(ctx context.Context,
	future actor.Future[oor.ActorResp],
	locked *wallet.SelectAndLockVTXOsResponse, releaseCustomInputs func(),
	timeout time.Duration) {

	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), timeout,
	)

	go func() {
		defer cancel()

		oorResult := future.Await(cleanupCtx)
		oorResp, err := oorResult.Unpack()
		if err != nil {
			// The unlock context defaults to cleanupCtx, which is
			// still live on a real actor failure. If the await
			// instead ended because cleanupCtx hit its deadline,
			// that same context is now expired and the wallet
			// actor's mailbox would reject the unlock Tell before
			// enqueue, silently pinning the wallet-selected VTXOs.
			// Derive a fresh bounded context from the detached base
			// in that case so the unlock still lands.
			unlockCtx := cleanupCtx
			if cleanupCtx.Err() != nil {
				r.server.log.ErrorS(
					cleanupCtx,
					"Timed out waiting for detached OOR "+
						"submit cleanup",
					err,
					slog.Duration("timeout", timeout),
				)

				freshCtx, freshCancel := context.WithTimeout(
					context.WithoutCancel(ctx),
					submittedOORUnlockTimeout,
				)
				defer freshCancel()

				unlockCtx = freshCtx
			}

			r.unlockSelectedVTXOsBestEffort(unlockCtx, locked)
			if releaseCustomInputs != nil {
				releaseCustomInputs()
			}

			return
		}

		resp, ok := oorResp.(*oor.StartTransferResponse)
		if !ok {
			r.unlockSelectedVTXOsBestEffort(cleanupCtx, locked)
			if releaseCustomInputs != nil {
				releaseCustomInputs()
			}

			return
		}

		if resp.Existing {
			r.unlockSelectedVTXOsBestEffort(cleanupCtx, locked)
		}

		if releaseCustomInputs != nil {
			releaseCustomInputs()
		}
	}()
}

// unlockVTXOs sends an UnlockVTXOsRequest to the wallet actor for the
// given set of selected VTXOs. This is used for cleanup when an OOR
// transfer fails.
func (r *RPCServer) unlockVTXOs(ctx context.Context,
	vtxos []wallet.SelectedVTXO) error {

	if !r.server.walletRef.IsSome() {
		return nil
	}

	outpoints := make([]wire.OutPoint, 0, len(vtxos))
	for _, sv := range vtxos {
		outpoints = append(outpoints, sv.Outpoint)
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	return wRef.Tell(ctx, &wallet.UnlockVTXOsRequest{
		Outpoints: outpoints,
	})
}

// addrNetName returns a best-effort network name for a decoded address so
// cross-network error messages can be specific. Testnet3 and testnet4 share
// address encodings, so a single address can honestly match both networks.
// Report both instead of pretending the address proves one over the other.
func addrNetName(addr btcaddr.Address) string {
	var matches []string
	for _, p := range []*chaincfg.Params{
		&chaincfg.MainNetParams,
		&chaincfg.TestNet3Params,
		&chaincfg.TestNet4Params,
		&chaincfg.SigNetParams,
		&chaincfg.RegressionNetParams,
	} {
		if addr.IsForNet(p) {
			matches = append(matches, p.Name)
		}
	}

	if len(matches) == 0 {
		return "unknown"
	}

	return strings.Join(matches, "/")
}

// resolveLeaveDestination extracts the on-chain pkScript from a
// LeaveDestination oneof. Unlike resolveRecipientOutput — which is
// VTXO-centric and requires a taproot shape plus a client public key
// — leave destinations are plain on-chain outputs, so we accept any
// standard address (taproot, segwit, legacy) decoded against the
// daemon's chainParams, or a raw pkScript the caller has already
// resolved. Raw pkScripts are length-capped and class-whitelisted
// here because no downstream layer re-validates the bytes, and a
// typo'd / hostile pkScript on a funds-moving call would otherwise
// land coins on an unspendable script.
func (r *RPCServer) resolveLeaveDestination(d *waverpc.LeaveDestination) (
	[]byte, error) {

	if d == nil {
		return nil, fmt.Errorf("leave destination is required")
	}

	switch t := d.Target.(type) {
	case *waverpc.LeaveDestination_Address:
		if t.Address == "" {
			return nil, fmt.Errorf("leave destination address is " +
				"empty")
		}

		addr, err := btcaddr.DecodeAddress(
			t.Address, r.server.chainParams,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid leave address: %w", err)
		}

		// DecodeAddress honors the HRP for bech32 addresses and
		// uses defaultNet only as a hint, so a mainnet address
		// decoded under regtest still succeeds. The explicit
		// IsForNet check below blocks the cross-network footgun:
		// leave is funds-moving and a mismatched net would send
		// real coins to an unintended script.
		if !addr.IsForNet(r.server.chainParams) {
			return nil, fmt.Errorf("invalid leave address: "+
				"address is for %q, daemon is on %q",
				addrNetName(addr), r.server.chainParams.Name)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, fmt.Errorf("derive leave pkScript: %w", err)
		}

		return pkScript, nil

	case *waverpc.LeaveDestination_PkScript:
		if err := validateLeavePkScript(t.PkScript); err != nil {
			return nil, err
		}

		return t.PkScript, nil

	default:
		return nil, fmt.Errorf("leave destination target is required")
	}
}

// p2aPkScript is the canonical Pay-to-Anchor (BIP 431) script that
// CPFP packages use as their ephemeral anchor (`OP_1 <0x4e73>`). A
// caller pointing a leave at this script would be assigning
// real-value coins to a 240-sat anyone-can-spend output, which is
// almost always a bug — we reject it explicitly so the typo surfaces
// as InvalidArgument rather than a silently-burned VTXO.
var p2aPkScript = []byte{
	txscript.OP_1, txscript.OP_DATA_2, 0x4e, 0x73,
}

// validateLeavePkScript enforces the structural guard rails on the
// raw `pk_script` branch of a LeaveDestination: a non-empty, ≤
// MaxScriptSize byte string of a recognised standard class, with
// witness-unknown and the BIP 431 anchor pattern explicitly rejected.
// An address-typed destination cannot fail any of these (DecodeAddress
// + PayToAddrScript only ever produce P2*KH / P2*SH / P2TR), so this
// helper is only invoked on caller-supplied raw bytes.
func validateLeavePkScript(pkScript []byte) error {
	if len(pkScript) == 0 {
		return fmt.Errorf("leave destination pk_script is empty")
	}

	if len(pkScript) > txscript.MaxScriptSize {
		return fmt.Errorf("leave destination pk_script too large: "+
			"%d > %d", len(pkScript), txscript.MaxScriptSize)
	}

	// Reject the BIP 431 P2A anchor pattern. P2A is intentionally
	// anyone-can-spend and only meaningful as a CPFP hook; a leave
	// that lands on it is effectively a burn.
	if bytes.Equal(pkScript, p2aPkScript) {
		return fmt.Errorf("leave destination pk_script is the P2A " +
			"anchor pattern; reject as funds-burn")
	}

	// Whitelist standard classes. Witness-unknown / non-standard
	// scripts may relay-reject or land at an unspendable output;
	// requiring a recognised class catches typo'd bytes that would
	// otherwise silently ship to the round actor.
	class := txscript.GetScriptClass(pkScript)
	switch class {
	case txscript.PubKeyTy,
		txscript.PubKeyHashTy,
		txscript.ScriptHashTy,
		txscript.WitnessV0PubKeyHashTy,
		txscript.WitnessV0ScriptHashTy,
		txscript.WitnessV1TaprootTy,
		txscript.MultiSigTy,
		txscript.NullDataTy:
		return nil

	default:
		return fmt.Errorf("leave destination pk_script class %s is "+
			"not supported; use a standard "+
			"P2PKH/P2SH/P2WPKH/P2WSH/P2TR/OP_RETURN script", class)
	}
}

// resolveRecipientOutput extracts both the pkScript and the client
// public key from an Output proto. The client key is required for
// constructing VTXO descriptors in directed sends, so policy-template
// destinations must decode to the standard Ark VTXO shape with an
// explicit owner key.
func (r *RPCServer) resolveRecipientOutput(out *waverpc.Output) ([]byte,
	*btcec.PublicKey, error) {

	switch d := out.Destination.(type) {
	case *waverpc.Output_Pubkey:
		if len(d.Pubkey) != schnorr.PubKeyBytesLen {
			return nil, nil, fmt.Errorf("pubkey must be %d "+
				"bytes, got %d", schnorr.PubKeyBytesLen,
				len(d.Pubkey))
		}

		clientKey, err := schnorr.ParsePubKey(d.Pubkey)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid pubkey: %w", err)
		}

		// Derive the BIP-86 taproot pkScript from the
		// x-only pubkey.
		addr, err := btcaddr.NewAddressTaproot(
			d.Pubkey, r.server.chainParams,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("derive taproot "+
				"address: %w", err)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, nil, fmt.Errorf("derive pkScript: %w", err)
		}

		return pkScript, clientKey, nil

	case *waverpc.Output_Address:
		addr, err := btcaddr.DecodeAddress(
			d.Address, r.server.chainParams,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid address: %w", err)
		}

		// Only taproot addresses carry the x-only pubkey
		// needed for VTXO construction.
		tapAddr, ok := addr.(*btcaddr.AddressTaproot)
		if !ok {
			return nil, nil, fmt.Errorf("directed sends require a "+
				"taproot address, got %T", addr)
		}

		clientKey, err := schnorr.ParsePubKey(
			tapAddr.ScriptAddress(),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("extract pubkey from "+
				"address: %w", err)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, nil, fmt.Errorf("derive pkScript: %w", err)
		}

		return pkScript, clientKey, nil

	case *waverpc.Output_PolicyTemplate:
		if len(d.PolicyTemplate) == 0 {
			return nil, nil, fmt.Errorf("policy_template is empty")
		}

		template, err := arkscript.DecodePolicyTemplate(
			d.PolicyTemplate,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("decode "+
				"policy_template: %w", err)
		}

		params, err := arkscript.DecodeStandardVTXOParams(
			template,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("directed sends require a "+
				"standard policy_template: %w", err)
		}

		pkScript, err := template.PkScript()
		if err != nil {
			return nil, nil, fmt.Errorf("derive pkScript from "+
				"policy_template: %w", err)
		}

		return pkScript, params.OwnerKey, nil

	default:
		return nil, nil, fmt.Errorf("directed sends require pubkey, "+
			"taproot address, or standard policy_template "+
			"destination, got %T", d)
	}
}

// resolveOutputPolicyTemplate returns the semantic policy template to persist
// for one OOR recipient output when it is known locally.
func (r *RPCServer) resolveOutputPolicyTemplate(_ context.Context,
	out *waverpc.Output, pkScript []byte, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]byte, error) {

	if out == nil {
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"must be provided")
	}

	switch d := out.Destination.(type) {
	case *waverpc.Output_PolicyTemplate:
		if len(d.PolicyTemplate) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"policy_template is empty")
		}

		if len(out.VtxoPolicyTemplate) > 0 &&
			!bytes.Equal(
				out.VtxoPolicyTemplate, d.PolicyTemplate,
			) {
			return nil, status.Errorf(codes.InvalidArgument,
				"destination policy_template does not match "+
					"vtxo_policy_template")
		}

		return validateOutputPolicyTemplate(
			pkScript, d.PolicyTemplate, operatorKey, exitDelay,
		)

	case nil:
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"destination is required")
	}

	if len(out.VtxoPolicyTemplate) > 0 {
		return validateOutputPolicyTemplate(
			pkScript, out.VtxoPolicyTemplate, operatorKey,
			exitDelay,
		)
	}

	switch d := out.Destination.(type) {
	case *waverpc.Output_Pubkey:
		ownerKey, err := schnorr.ParsePubKey(d.Pubkey)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid pubkey: %v", err)
		}

		return encodeStandardRecipientPolicy(
			ownerKey, operatorKey, exitDelay, pkScript,
		)

	default:
		return nil, nil
	}
}

// validateOutputPolicyTemplate verifies one provided semantic policy against
// the derived recipient script, checks structural Ark invariants, and
// returns a defensive copy on success.
//
// The per-leaf exit-delay minimum is intentionally NOT enforced here:
// non-standard recipient policies (e.g. vHTLC) legitimately carry
// protocol-specific unilateral delays that may fall below the operator's
// standard VTXOExitDelay. What IS enforced:
//
//  1. DecodePolicyTemplate — well-formed encoding.
//  2. MatchesPkScript — the semantic policy compiles bit-for-bit to the
//     on-chain output; this is the core binding that prevents quoting
//     one policy on the wire while committing under another.
//  3. ValidatePolicy structural invariants — at least one collab leaf
//     containing the operator, at least one CSV-gated non-operator exit
//     leaf, and no operator-unilateral spend paths anywhere in the tree.
//  4. A non-zero operator exit delay — the zero-delay fail-closed gate
//     inherited from the H-4/H-5 hardening: a zero operator minimum
//     would invite callers to produce policies the operator cannot
//     actually enforce. Standard-shape recipients are checked against
//     their declared exit delay at the server; custom shapes carry
//     their own per-path delays that pkScript binding transitively
//     verifies.
//
// Standard-shape recipients (SendVTXO-directed sends) are pre-filtered by
// DecodeStandardVTXOParams in resolveRecipientOutput before reaching this
// helper, so the weaker structural check here does not relax the
// standard-VTXO contract — it lets the OOR recipient path accept vHTLC
// and other custom shapes, which is the intent of the arkscript policy
// layer.
func validateOutputPolicyTemplate(pkScript, policyTemplate []byte,
	operatorKey *btcec.PublicKey, minExitDelay uint32) ([]byte, error) {

	template, err := arkscript.DecodePolicyTemplate(
		policyTemplate,
	)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode "+
			"policy_template: %v", err)
	}

	if !template.MatchesPkScript(pkScript) {
		return nil, status.Errorf(codes.InvalidArgument,
			"policy_template does not match output script")
	}

	if operatorKey != nil {
		// Fail closed on a zero operator minimum: this value is
		// supplied by the server's operator terms and a zero here
		// indicates an unconfigured or degraded operator rather
		// than a legitimate shape choice.
		if minExitDelay == 0 {
			return nil, status.Errorf(codes.FailedPrecondition,
				"operator exit delay must be non-zero")
		}

		nodes := make(
			[]arkscript.Node, len(template.Leaves),
		)
		for i, leaf := range template.Leaves {
			nodes[i] = leaf.Node
		}

		// Structural invariants only — MinExitDelay is left unset
		// so per-leaf CSV values are not forced to meet the
		// operator's standard-VTXO minimum. See the docstring
		// above for why this is safe.
		if err := arkscript.ValidatePolicy(
			nodes, arkscript.PolicyValidationOpts{
				OperatorKey: operatorKey,
			},
		); err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"policy violates Ark invariants: %v", err)
		}
	}

	return append([]byte(nil), policyTemplate...), nil
}

// encodeStandardRecipientPolicy derives the canonical standard Ark VTXO policy
// and verifies it compiles back to the target output script. Fails closed
// when any required term is missing rather than silently returning a nil
// policy: a zero exit delay or a missing operator key would otherwise
// produce a "policyless" VTXO and silently bypass admission validation.
func encodeStandardRecipientPolicy(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32, pkScript []byte) ([]byte, error) {

	switch {
	case ownerKey == nil:
		return nil, status.Errorf(codes.InvalidArgument, "owner key "+
			"must be provided")

	case operatorKey == nil:
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator key must be fetched before encoding "+
				"standard recipient policy")

	case exitDelay == 0:
		return nil, status.Errorf(codes.FailedPrecondition,
			"operator exit delay must be non-zero")
	}

	policyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode standard "+
			"recipient policy: %v", err)
	}

	template, err := arkscript.DecodePolicyTemplate(policyTemplate)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decode derived "+
			"recipient policy: %v", err)
	}

	if !template.MatchesPkScript(pkScript) {
		return nil, status.Errorf(codes.Internal, "derived recipient "+
			"policy does not match pk_script")
	}

	return policyTemplate, nil
}

// resolveOutputPkScript derives a pkScript from the Output's destination
// oneof. It supports address, pubkey, and semantic policy destinations. For
// pubkey destinations, operator terms are fetched to derive a VTXO-compatible
// taproot output.
func (r *RPCServer) resolveOutputPkScript(ctx context.Context,
	out *waverpc.Output) ([]byte, error) {

	switch d := out.Destination.(type) {
	case *waverpc.Output_Address:
		addr, err := btcaddr.DecodeAddress(
			d.Address, r.server.chainParams,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid address: %v", err)
		}

		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to "+
				"derive pkScript: %v", err)
		}

		return pkScript, nil

	case *waverpc.Output_Pubkey:
		if len(d.Pubkey) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"pubkey is empty")
		}

		recipientKey, err := schnorr.ParsePubKey(d.Pubkey)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid pubkey: %v", err)
		}

		// For OOR sends, a pubkey destination creates a
		// VTXO-compatible taproot output with the operator's
		// collab key and exit delay (not a simple BIP-86
		// key-path-only output). This ensures the recipient
		// gets a standard Ark VTXO they can spend
		// collaboratively or exit unilaterally.
		terms, err := r.server.fetchOperatorTerms(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fetch "+
				"operator terms for pubkey destination: %v",
				err)
		}

		pkScript, err := BuildPubKeyVTXOReceiveScript(
			recipientKey, terms.PubKey, terms.VTXOExitDelay,
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "unable to "+
				"derive VTXO receive script: %v", err)
		}

		return pkScript, nil

	case *waverpc.Output_PolicyTemplate:
		if len(d.PolicyTemplate) == 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"policy_template is empty")
		}

		template, err := arkscript.DecodePolicyTemplate(
			d.PolicyTemplate,
		)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"decode policy_template: %v", err)
		}

		pkScript, err := template.PkScript()
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"derive pkScript from policy_template: %v", err)
		}

		return pkScript, nil

	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported "+
			"destination type: %T", d)
	}
}

// Unroll triggers a unilateral exit for the specified VTXO outpoint.
// The request is routed through the VTXO manager so the VTXO actor
// transitions cleanly to UnilateralExitState before the unroll job is
// spawned. The unroll job creation happens asynchronously through the
// chain resolver seam.
func (r *RPCServer) Unroll(ctx context.Context, req *waverpc.UnrollRequest) (
	*waverpc.UnrollResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.vtxoMgrRef.IsSome() {
		return nil, status.Errorf(codes.Unavailable, "VTXO manager "+
			"not initialized")
	}

	outpoint, err := parseOutpointString(req.Outpoint)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"outpoint: %v", err)
	}

	// Detach admission from the RPC stream: a CLI/RPC disconnect must
	// not cancel the unilateral-exit handoff once the user has asked
	// to unroll. The WithoutCancel() strips ctx cancellation; the
	// WithTimeout cap then keeps a wedged daemon-local admission
	// (e.g. a stuck VTXO manager Ask) from hanging forever.
	admissionCtx, cancelAdmission := context.WithTimeout(
		context.WithoutCancel(ctx), manualUnrollAdmissionTimeout,
	)
	defer cancelAdmission()

	var vtxoMgrRef actor.ActorRef[
		vtxo.ManagerMsg, vtxo.ManagerResp,
	]
	r.server.vtxoMgrRef.WhenSome(
		func(ref actor.ActorRef[
			vtxo.ManagerMsg, vtxo.ManagerResp,
		]) {

			vtxoMgrRef = ref
		},
	)

	// Check if this VTXO is already in unilateral exit. If so, the
	// transition already happened and we just return Created=false.
	//
	// Also load the descriptor (when available) so we can pre-flight
	// the wallet's CPFP fee-input budget against the VTXO's ancestry
	// shape. Resolving the descriptor here saves a second round-trip
	// to the store further down.
	var desc *vtxo.Descriptor
	if r.server.vtxoStore != nil {
		d, err := r.server.vtxoStore.GetVTXO(admissionCtx, outpoint)
		if err == nil && d != nil {
			desc = d

			if desc.Status == vtxo.VTXOStatusUnilateralExit {
				return &waverpc.UnrollResponse{
					Created: false,
					ActorId: unroll.ActorIDForTarget(
						outpoint,
					),
				}, nil
			}
		}
	}

	// Pre-flight the whole-picture feasibility of the exit before
	// committing the VTXO to UnilateralExitState. This refuses an exit
	// that can never succeed (the swept output would be dust and the
	// sweep tx unrelayable — wavelength #608), that is economically
	// irrational (it burns more in fees than the coin is worth), or
	// that the wallet cannot fund (insufficient balance or too few
	// distinct CPFP fee inputs). Failing closed here keeps the VTXO out
	// of an exit state the user can't easily back out of.
	if err := r.preflightUnrollFeasibility(
		admissionCtx, desc,
	); err != nil {
		return nil, err
	}

	// Route through the VTXO manager which tells the VTXO actor to
	// transition to UnilateralExitState. The actor's outbox handler
	// emits ExpiringNotification through the chain resolver seam,
	// which triggers the unroll manager to create the job.
	resp, askErr := vtxoMgrRef.Ask(
		admissionCtx, &actormsg.ForceUnrollRequest{
			Outpoint: outpoint,
			Reason:   "manual RPC request",
		},
	).Await(admissionCtx).Unpack()
	if askErr != nil {
		return nil, status.Errorf(codes.Internal, "force unroll: %v",
			askErr)
	}

	unrollResp, ok := resp.(*actormsg.ForceUnrollResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected "+
			"response type %T", resp)
	}

	// The unroll job is created asynchronously. Return that the
	// request was accepted. Use GetUnrollStatus to track progress.
	actorID := unroll.ActorIDForTarget(outpoint)

	return &waverpc.UnrollResponse{
		Created: unrollResp.Accepted,
		ActorId: actorID,
	}, nil
}

const (
	// unrollFeeConfTarget is the confirmation target used to estimate
	// the fee rate for the unroll feasibility pre-flight. Six blocks
	// matches the sweep estimator so the pre-check and the sweep the
	// unroll actor later builds agree on the rate.
	unrollFeeConfTarget = 6

	// unrollFallbackFeeRateSatPerVByte is used when fee estimation is
	// temporarily unavailable (cold backend, regtest) so the pre-flight
	// can still produce a plausible estimate rather than blocking. Two
	// sat/vB matches the sweep estimator's fallback.
	unrollFallbackFeeRateSatPerVByte = btcutil.Amount(2)

	// unrollMaxFeeRateSatPerVByte clamps a pathological fee estimate so
	// a transient spike can't make every exit look uneconomical. Matches
	// the sweep estimator's clamp.
	unrollMaxFeeRateSatPerVByte = btcutil.Amount(100)
)

// preflightUnrollFeasibility assesses the whole-picture feasibility of a
// unilateral exit before admission and returns a gRPC error when it is
// infeasible. It gathers the recovery-transaction count from the VTXO
// descriptor, the current fee rate from the chain backend, and the
// wallet's confirmed UTXO set, then defers the verdict to the pure
// unroll.AssessExitFeasibility model. Returns nil (allow admission) when
// the descriptor is unavailable, since we can't assess without it and the
// downstream admission path will surface its own error.
func (r *RPCServer) preflightUnrollFeasibility(ctx context.Context,
	desc *vtxo.Descriptor) error {

	if desc == nil {

		// The descriptor lookup failed earlier — let the existing
		// admission path produce its own error rather than guess at
		// the feasibility here.
		return nil
	}

	walletSnapshot, err := r.walletExitFundingSnapshot(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "preflight wallet "+
			"unspent: %v", err)
	}
	mat := r.resolveExitLineage(ctx, desc.Outpoint, desc)
	plan := unroll.PlanExitFunding(
		desc, mat, r.estimateUnrollFeeRate(ctx), walletSnapshot,
	)
	verdict := plan.Feasibility
	if verdict.Feasible {
		return nil
	}

	return unrollInfeasibleError(verdict)
}

// resolveExitLineage resolves the OOR lineage material for an exit target so
// the funding estimate can size the checkpoint/ark transactions of each OOR hop
// exactly. It returns nil for a round-direct VTXO (ChainDepth == 0), where
// there are no OOR transactions and the descriptor's extracted tree paths
// already size the recovery cost exactly, and nil on any resolution failure, so
// a missing or partial artifact store degrades to the descriptor-only
// ChainDepth estimate rather than blocking the exit.
func (r *RPCServer) resolveExitLineage(ctx context.Context,
	target wire.OutPoint, desc *vtxo.Descriptor) *unroll.LineageMaterial {

	if desc == nil || desc.ChainDepth == 0 {
		return nil
	}

	resolver := &unroll.DescriptorLineageResolver{
		VTXOStore:     r.server.vtxoStore,
		ArtifactStore: r.newLocalOORArtifactStore(),
	}

	mat, err := resolver.ResolveLineage(ctx, target)
	if err != nil {
		// The exact OOR sizing is a refinement, not a gate: fall back
		// to the ChainDepth approximation so a resolution hiccup can
		// never make an otherwise-viable exit look infeasible.
		r.server.log.DebugS(ctx, "Exit-plan lineage resolve failed; "+
			"falling back to ChainDepth funding estimate",
			slog.String("target", target.String()),
			btclog.Fmt("err", "%v", err))

		return nil
	}

	return mat
}

// estimateUnrollFeeRate returns the current fee rate (sat/vByte) for the
// unroll feasibility estimate, clamped to a sane range. It falls back to
// a small fixed rate when the chain backend is missing or estimation
// fails, so a cold backend never blocks an otherwise-viable exit.
func (r *RPCServer) estimateUnrollFeeRate(ctx context.Context) btcutil.Amount {
	if r.server.chainBackend == nil {
		return unrollFallbackFeeRateSatPerVByte
	}

	feeRate, err := r.server.chainBackend.EstimateFee(
		ctx, unrollFeeConfTarget,
	)
	if err != nil || feeRate <= 0 {
		return unrollFallbackFeeRateSatPerVByte
	}

	if feeRate > unrollMaxFeeRateSatPerVByte {
		return unrollMaxFeeRateSatPerVByte
	}

	return feeRate
}

// unrollInfeasibleError maps an infeasible exit verdict to an actionable
// gRPC error. Every case is codes.FailedPrecondition: the request is
// well-formed but the current VTXO/wallet/fee state forbids it. The
// messages name the concrete numbers (cost, value, dust floor) so the
// caller can act without re-deriving them, and point at cooperative leave
// where unilateral exit is not the right tool.
func unrollInfeasibleError(f unroll.ExitFeasibility) error {
	switch f.Reason {
	case unroll.ExitSweepBelowDust:
		return status.Errorf(codes.FailedPrecondition, "unilateral "+
			"exit not viable: VTXO is worth %d sat but after the "+
			"~%d sat sweep fee (at %d sat/vB) only %d sat would "+
			"remain, below the %d sat dust limit, so the exit "+
			"transaction cannot be broadcast. Use a cooperative "+
			"leave instead.", int64(f.VTXOAmountSat),
			int64(f.SweepFeeSat), int64(f.FeeRateSatPerVByte),
			int64(f.NetRecoveredSat), int64(f.DustLimitSat))

	case unroll.ExitUneconomical:
		return status.Errorf(codes.FailedPrecondition, "unilateral "+
			"exit uneconomical: recovering this VTXO requires "+
			"broadcasting %d transaction(s) costing ~%d sat in "+
			"on-chain fees, but the VTXO is only worth %d sat. "+
			"Use a cooperative leave instead.", f.NumRecoveryTxs,
			int64(f.TotalRecoveryCostSat), int64(f.VTXOAmountSat))

	case unroll.ExitWalletUnderfunded:
		return status.Errorf(codes.FailedPrecondition, "on-chain "+
			"wallet balance too low for unroll: need ~%d sat to "+
			"fund CPFP fees for %d recovery transaction(s), but "+
			"only %d sat confirmed. Call GetExitPlan for a "+
			"funding address and the recommended deposit, "+
			"then retry.", int64(f.CPFPFeeTotalSat),
			f.NumRecoveryTxs, int64(f.WalletConfirmedSat))

	case unroll.ExitWalletTooFewInputs:
		return status.Errorf(codes.FailedPrecondition, "insufficient "+
			"wallet UTXOs to fund unroll CPFP: need at least %d "+
			"confirmed wallet UTXO(s) of >= %d sat each (one per "+
			"ancestry path), have %d usable. Call GetExitPlan for "+
			"funding details.", f.RequiredWalletInputs,
			int64(unroll.DefaultFeeInputMinAmountSat),
			f.WalletUsableInputs)

	default:
		return status.Errorf(codes.FailedPrecondition, "unilateral "+
			"exit infeasible: %s", f.Reason)
	}
}

// GetUnrollStatus returns the current status of an unroll job for the
// given VTXO outpoint.
func (r *RPCServer) GetUnrollStatus(ctx context.Context,
	req *waverpc.GetUnrollStatusRequest) (*waverpc.GetUnrollStatusResponse,
	error) {

	outpoint, err := parseOutpointString(req.Outpoint)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"outpoint: %v", err)
	}

	if r.server.unrollRegistryRef.IsSome() {
		resp, found, err := r.queryUnrollRegistry(
			ctx, outpoint, req.Detailed,
		)
		if err != nil {
			return nil, err
		}
		if found {
			return resp, nil
		}
	}

	if r.server.ueStore == nil {
		return &waverpc.GetUnrollStatusResponse{
			Found: false,
		}, nil
	}

	job, err := r.server.ueStore.GetJob(ctx, outpoint)
	if errors.Is(err, db.ErrUnilateralExitJobNotFound) {
		return &waverpc.GetUnrollStatusResponse{
			Found: false,
		}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query "+
			"unroll job: %v", err)
	}

	// Bitcoin txids are canonically rendered in reversed byte order
	// (chainhash.Hash.String). The live-registry path uses Hash.String
	// so we must match it here; a raw hex.EncodeToString on the
	// stored bytes would return a string that clients cannot look up
	// and would disagree with the live-registry response for the same
	// job.
	var sweepTxid string
	if len(job.SweepTxid) == chainhash.HashSize {
		var hash chainhash.Hash
		copy(hash[:], job.SweepTxid)
		sweepTxid = hash.String()
	}

	resp := &waverpc.GetUnrollStatusResponse{
		Found:     true,
		Status:    unrollJobStatusToProto(job.Status),
		SweepTxid: sweepTxid,
		LastError: job.LastError,
	}

	// The registry no longer has a live actor for this target (it was
	// evicted after reaching a terminal phase), so tree/CSV progress is
	// gone. A detailed probe can still project the fee breakdown from the
	// persisted descriptor and a human phase line, which keeps a completed
	// or failed exit inspectable via this same endpoint.
	if req.Detailed {
		resp.PhaseDetail = unrollPhaseDetail(resp.Status, nil)
		r.enrichExitFees(ctx, resp, outpoint, fn.None[int64](), nil)
	}

	return resp, nil
}

// queryUnrollRegistry asks the live unroll registry actor for the
// status of a target outpoint. It returns the proto response, whether
// the job was found, and any RPC error.
func (r *RPCServer) queryUnrollRegistry(ctx context.Context,
	outpoint wire.OutPoint, detailed bool) (
	*waverpc.GetUnrollStatusResponse, bool, error) {

	var registryRef actor.ActorRef[
		unroll.RegistryMsg, unroll.RegistryResp,
	]
	r.server.unrollRegistryRef.WhenSome(
		func(ref actor.ActorRef[
			unroll.RegistryMsg, unroll.RegistryResp,
		]) {

			registryRef = ref
		},
	)

	resp, askErr := registryRef.Ask(
		ctx, &unroll.GetStatusRequest{
			Outpoint: outpoint,
			Detailed: detailed,
		},
	).Await(ctx).Unpack()
	if askErr != nil {
		return nil, false, status.Errorf(codes.Internal, "query "+
			"unroll status: %v", askErr)
	}

	statusResp, ok := resp.(*unroll.GetStatusResp)
	if !ok {
		return nil, false, status.Errorf(codes.Internal, "unexpected "+
			"unroll status response %T", resp)
	}

	if !statusResp.Found {
		return nil, false, nil
	}

	var sweepTxid string
	if statusResp.Active && statusResp.State != nil &&
		statusResp.State.SweepTxid != nil {

		sweepTxid = statusResp.State.SweepTxid.String()
	} else if statusResp.SweepTxid != nil {
		sweepTxid = statusResp.SweepTxid.String()
	}

	lastError := statusResp.FailReason
	if statusResp.Active && statusResp.State != nil &&
		statusResp.State.FailReason != "" {

		lastError = statusResp.State.FailReason
	}

	phase := statusResp.Phase
	if statusResp.Active && statusResp.State != nil {
		phase = statusResp.State.Phase
	}

	out := &waverpc.GetUnrollStatusResponse{
		Found:     true,
		Status:    unrollPhaseToProto(phase),
		SweepTxid: sweepTxid,
		LastError: lastError,
	}
	if detailed {
		r.enrichUnrollDetail(ctx, out, outpoint, statusResp)
	}

	return out, true, nil
}

// enrichUnrollDetail fills the detailed status fields (phase line, tree/CSV
// progress, best-case block countdown, current height, and fee breakdown) from
// the live child's planner-derived state. It is best-effort: any missing piece
// (no live actor, planner not yet loaded, descriptor lookup failure) is left
// unset rather than failing the probe.
func (r *RPCServer) enrichUnrollDetail(ctx context.Context,
	out *waverpc.GetUnrollStatusResponse, outpoint wire.OutPoint,
	statusResp *unroll.GetStatusResp) {

	// progress is the live planner projection, present only when the target
	// has an active child that has loaded its proof and planner.
	var progress *unroll.ExitProgress
	var actualSweepFee fn.Option[int64]
	if statusResp.Active && statusResp.State != nil {
		out.CurrentHeight = statusResp.State.Height
		progress = statusResp.State.Progress
	}

	if progress != nil {
		out.Progress = unrollProgressToProto(progress)
		out.Csv = unrollCSVToProto(progress)
		out.BestCaseBlocksRemaining = progress.BestCaseBlocksRemaining
		actualSweepFee = progress.ActualSweepFeeSat
	}

	out.PhaseDetail = unrollPhaseDetail(out.Status, progress)

	r.enrichExitFees(ctx, out, outpoint, actualSweepFee, progress)
}

// unrollProgressToProto maps the actor's derived progress summary onto the
// proto progress message.
func unrollProgressToProto(p *unroll.ExitProgress) *waverpc.UnrollProgress {
	if p == nil {
		return nil
	}

	return &waverpc.UnrollProgress{
		ConfirmedTxs:      uint32(p.ConfirmedTxs),
		InFlightTxs:       uint32(p.InFlightTxs),
		ReadyTxs:          uint32(p.ReadyTxs),
		BlockedTxs:        uint32(p.BlockedTxs),
		TotalTxs:          uint32(p.TotalTxs),
		CurrentLayer:      uint32(p.CurrentLayer),
		TotalLayers:       uint32(p.TotalLayers),
		TargetConfirmed:   p.TargetConfirmed,
		AllProofConfirmed: p.AllProofConfirmed,
	}
}

// unrollCSVToProto maps the planner CSV maturity view onto the proto CSV
// message. It returns nil until the target confirms (CSV is None), so a caller
// can distinguish "not yet in the CSV wait" from a zeroed countdown.
func unrollCSVToProto(p *unroll.ExitProgress) *waverpc.UnrollCSV {
	if p == nil {
		return nil
	}

	var out *waverpc.UnrollCSV
	p.CSV.WhenSome(func(csv unrollplan.CSVInfo) {
		out = &waverpc.UnrollCSV{
			TargetConfirmHeight: csv.TargetConfirmHeight,
			MaturityHeight:      csv.MaturityHeight,
			BlocksRemaining:     csv.BlocksRemaining,
			Mature:              csv.Ready,
		}
	})

	return out
}

// unrollPhaseDetail renders a one-line human description of the current phase.
// When live progress is available it is folded into the materializing and
// CSV-pending lines; otherwise a coarse phase description is returned.
func unrollPhaseDetail(st waverpc.UnrollJobStatus,
	p *unroll.ExitProgress) string {

	switch st {
	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING:
		return "queued; recovery transactions not yet broadcast"

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING:
		if p == nil {
			return "materializing recovery transactions on-chain"
		}

		// The frontier layer collapses to TotalLayers once every proof
		// node confirms, so clamp the 1-based display so a job that
		// reads MATERIALIZING after the frontier collapsed cannot
		// render "layer N+1 of N".
		layer := p.CurrentLayer + 1
		if layer > p.TotalLayers {
			layer = p.TotalLayers
		}

		return fmt.Sprintf("materializing recovery tree: layer %d of "+
			"%d, %d/%d txs confirmed (%d in flight, %d ready, %d "+
			"blocked)", layer, p.TotalLayers, p.ConfirmedTxs,
			p.TotalTxs, p.InFlightTxs, p.ReadyTxs, p.BlockedTxs)

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING:
		detail := "recovery transactions confirmed; waiting for CSV " +
			"maturity"
		if p != nil {
			p.CSV.WhenSome(func(csv unrollplan.CSVInfo) {
				detail = fmt.Sprintf("all recovery txs "+
					"confirmed; waiting for CSV maturity "+
					"(%d blocks remaining, matures at "+
					"height %d)", csv.BlocksRemaining,
					csv.MaturityHeight)
			})
		}

		return detail

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING:
		return "CSV matured; sweep in flight, awaiting confirmation"

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED:
		return "exit complete; funds swept to the backing wallet"

	case waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED:
		return "exit failed; see last_error"

	default:
		return ""
	}
}

// enrichExitFees projects the exit's on-chain cost breakdown onto the response
// from the persisted VTXO descriptor and the current fee estimate, reusing the
// same cost model as unroll admission and GetExitPlan. The CPFP total and the
// default sweep fee are estimates; when the caller supplies the real
// built-sweep fee it overrides the sweep leg and marks it actual. When live
// progress is available, spent_so_far is estimated from the CPFP children
// already broadcast (confirmed + in-flight proof txs) plus the sweep fee once
// the sweep is built. It is best-effort: a descriptor or fee-estimate failure
// leaves fees unset rather than failing the probe.
func (r *RPCServer) enrichExitFees(ctx context.Context,
	out *waverpc.GetUnrollStatusResponse, outpoint wire.OutPoint,
	actualSweepFee fn.Option[int64], progress *unroll.ExitProgress) {

	if r.server.vtxoStore == nil {
		return
	}

	desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		r.server.log.DebugS(ctx, "exit fee estimate skipped",
			slog.String("outpoint", outpoint.String()),
			slog.String("err", err.Error()),
		)

		return
	}

	feeRate, err := r.estimateWalletFeeRate(ctx, 0)
	if err != nil {
		return
	}

	// Resolve the lineage material so the CPFP estimate sizes the OOR
	// checkpoint/ark transactions exactly, keeping the detailed status fee
	// in agreement with `exit plan`. It is best-effort: a resolve failure
	// returns nil, which PlanExitFunding approximates from the descriptor's
	// ChainDepth.
	mat := r.resolveExitLineage(ctx, outpoint, desc)

	// A zero wallet snapshot is intentional: the cost fields
	// (CPFP/sweep/total/net) depend only on the fee rate, recovery-tx
	// count, and VTXO value, not on wallet funding, so the feasibility
	// verdict is ignored here.
	plan := unroll.PlanExitFunding(
		desc, mat, btcutil.Amount(feeRate),
		unroll.ExitFundingSnapshot{},
	)
	f := plan.Feasibility

	fees := &waverpc.UnrollFees{
		CpfpFeeSat:      int64(f.CPFPFeeTotalSat),
		SweepFeeSat:     int64(f.SweepFeeSat),
		VtxoAmountSat:   int64(f.VTXOAmountSat),
		FeeRateSatVbyte: int64(f.FeeRateSatPerVByte),
	}
	sweepBuilt := false
	actualSweepFee.WhenSome(func(sweepFee int64) {
		fees.SweepFeeSat = sweepFee
		fees.SweepFeeActual = true
		sweepBuilt = true
	})
	fees.TotalCostSat = fees.CpfpFeeSat + fees.SweepFeeSat
	fees.NetRecoveredSat = fees.VtxoAmountSat - fees.SweepFeeSat
	fees.SpentSoFarSat = spentSoFarSat(
		fees, progress, sweepBuilt, out.Status,
	)

	out.Fees = fees
}

// spentSoFarSat estimates the on-chain fee already committed at this point in
// the exit. During materialization it prorates the CPFP total over the proof
// transactions already broadcast (confirmed plus in-flight) and adds the sweep
// fee once the sweep is built. With no live progress a completed exit has spent
// the whole projected total; any other terminal or coarse state reports zero
// (nothing known to be committed).
//
// The proration divides by progress.TotalTxs (the deduped proof-graph node
// count) so numerator and denominator count the same universe: the confirmed
// and in-flight counts are drawn from that same graph. This is an estimate, not
// accounting — a confirmed proof tx may be a shared ancestor that another party
// (or admission) paid the CPFP for, which would make spent_so_far run high.
func spentSoFarSat(fees *waverpc.UnrollFees, progress *unroll.ExitProgress,
	sweepBuilt bool, st waverpc.UnrollJobStatus) int64 {

	if progress == nil {
		if st == waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED {
			return fees.TotalCostSat
		}

		return 0
	}

	var spent int64
	if progress.TotalTxs > 0 {
		broadcast := progress.ConfirmedTxs + progress.InFlightTxs
		if broadcast > progress.TotalTxs {
			broadcast = progress.TotalTxs
		}

		// Multiply before dividing so the proration keeps precision: a
		// per-child divide first would truncate to zero whenever the
		// CPFP total is smaller than the transaction count.
		spent = fees.CpfpFeeSat * int64(broadcast) /
			int64(progress.TotalTxs)
	}

	// The sweep fee is committed only once the sweep transaction has been
	// built and broadcast.
	if sweepBuilt {
		spent += fees.SweepFeeSat
	}

	return spent
}

// unrollPhaseToProto maps the new unroll phase enum to the proto enum.
func unrollPhaseToProto(phase unroll.Phase) waverpc.UnrollJobStatus {
	switch phase {
	case unroll.PhasePending:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING

	case unroll.PhaseCSVPending:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING

	case unroll.PhaseSweepBroadcast, unroll.PhaseSweepConfirmation:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING

	case unroll.PhaseCompleted:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED

	case unroll.PhaseFailed:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED

	default:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING
	}
}

// unrollJobStatusToProto maps the internal job status to the proto
// enum.
func unrollJobStatusToProto(
	s db.UnilateralExitJobStatus) waverpc.UnrollJobStatus {

	switch s {
	case db.UnilateralExitJobStatusPending:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING

	case db.UnilateralExitJobStatusMaterializing:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING

	case db.UnilateralExitJobStatusCSVPending:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING

	// SweepBroadcasting is the pre-broadcast persist-before-send
	// window: the registry has built a sweep tx and written it to
	// disk but has not yet confirmed mempool acceptance. The client-
	// visible state collapses this onto SWEEPING so callers see a
	// single "the sweep is in flight" phase across both the broadcast
	// and confirmation halves of the sub-FSM.
	case db.UnilateralExitJobStatusSweepBroadcasting,
		db.UnilateralExitJobStatusSweeping:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING

	case db.UnilateralExitJobStatusCompleted:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED

	// Both terminal failure flavours surface as FAILED to clients. The
	// recoverable variant is an internal distinction used by boot-time
	// reconciliation to roll a no-footprint failure back to live; the
	// user-visible job still failed.
	case db.UnilateralExitJobStatusFailed,
		db.UnilateralExitJobStatusFailedRecoverable:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED

	default:
		return waverpc.UnrollJobStatus_UNROLL_JOB_STATUS_UNSPECIFIED
	}
}

// parseOutpointString parses a "txid:index" string into a
// wire.OutPoint.
func parseOutpointString(s string) (wire.OutPoint, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return wire.OutPoint{}, fmt.Errorf("expected txid:index format")
	}

	hash, err := chainhash.NewHashFromStr(parts[0])
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("invalid txid: %w", err)
	}

	idx, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return wire.OutPoint{}, fmt.Errorf("invalid index: %w", err)
	}

	return wire.OutPoint{
		Hash:  *hash,
		Index: uint32(idx),
	}, nil
}

// watchRoundsPollInterval is how often WatchRounds polls the round
// actor for state changes.
const watchRoundsPollInterval = 500 * time.Millisecond

// clientStateToProto maps a round.ClientState to the proto RoundState
// enum value.
func clientStateToProto(state round.ClientState) waverpc.RoundState {
	switch state.(type) {
	case *round.Idle:
		return waverpc.RoundState_ROUND_STATE_IDLE

	case *round.PendingRoundAssembly:
		return waverpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY

	case *round.IntentSentState:
		return waverpc.RoundState_ROUND_STATE_REGISTRATION_SENT

	case *round.QuoteReceivedState:
		return waverpc.RoundState_ROUND_STATE_QUOTE_RECEIVED

	case *round.RoundJoinedState:
		return waverpc.RoundState_ROUND_STATE_JOINED

	case *round.CommitmentTxReceivedState:
		return waverpc.RoundState_ROUND_STATE_COMMITMENT_RECEIVED

	case *round.CommitmentTxValidatedState:
		return waverpc.RoundState_ROUND_STATE_COMMITMENT_VALIDATED

	case *round.ForfeitSignaturesCollectingState:
		return waverpc.RoundState_ROUND_STATE_FORFEIT_COLLECTING

	case *round.NoncesSentState:
		return waverpc.RoundState_ROUND_STATE_NONCES_SENT

	case *round.NoncesAggregatedState:
		return waverpc.RoundState_ROUND_STATE_NONCES_AGGREGATED

	case *round.PartialSigsSentState:
		return waverpc.RoundState_ROUND_STATE_PARTIAL_SIGS_SENT

	case *round.InputSigSentState:
		return waverpc.RoundState_ROUND_STATE_INPUT_SIG_SENT

	case *round.ConfirmedState:
		return waverpc.RoundState_ROUND_STATE_CONFIRMED

	case *round.ClientFailedState:
		return waverpc.RoundState_ROUND_STATE_FAILED

	case *round.RecoveryInitiatedState:
		return waverpc.RoundState_ROUND_STATE_RECOVERY

	default:
		return waverpc.RoundState_ROUND_STATE_UNKNOWN
	}
}

// queryRoundStates fetches the current FSM states from the round actor
// and converts them to proto RoundInfo messages.
func (r *RPCServer) queryRoundStates(ctx context.Context) ([]*waverpc.RoundInfo,
	error) {

	if r.server.actorSystem == nil {
		return nil, status.Errorf(codes.Internal, "actor system not "+
			"initialized")
	}

	roundKey := round.NewServiceKey()
	roundRef := roundKey.Ref(r.server.actorSystem)

	stateMsg := &round.GetClientStateRequest{}
	future := roundRef.Ask(ctx, stateMsg)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to query "+
			"round state: %v", err)
	}

	stateResp, ok := resp.(*round.GetClientStateResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected state "+
			"response type: %T", resp)
	}

	rounds := make(
		[]*waverpc.RoundInfo, 0, len(stateResp.States),
	)
	for _, info := range stateResp.States {
		roundID := ""
		if !info.IsTemp {
			roundID = info.RoundID.String()
		}

		commitmentTxid, vtxos := liveRoundDetails(info.State)

		rounds = append(rounds, &waverpc.RoundInfo{
			RoundId:        roundID,
			State:          clientStateToProto(info.State),
			IsTemp:         info.IsTemp,
			Vtxos:          vtxos,
			CommitmentTxid: commitmentTxid,
			FailureReason:  roundFailureReason(info.State),
		})
	}

	return rounds, nil
}

// liveRoundDetails extracts the commitment transaction id and the
// upcoming VTXOs the local wallet is about to receive from a live
// (actor-served) round FSM state. The commitment txid is populated
// from the state CommitmentTxReceived onwards. VTXO entries from
// in-flight rounds carry only the amount because the precise leaf
// outpoint depends on tree finalisation; once the round is
// Confirmed the entries carry their real outpoints.
//
// Only VTXOs the wallet itself will own (HasLocalOwner=true) are
// reported — the round can contain outputs destined for other
// clients (e.g. in an in-round directed send), and surfacing those
// here would inflate the wallet's view of its own upcoming balance.
func liveRoundDetails(state round.ClientState) (string,
	[]*waverpc.RoundVTXOInfo) {

	upcomingFromVTXORequests := func(
		reqs []types.VTXORequest) []*waverpc.RoundVTXOInfo {

		out := make([]*waverpc.RoundVTXOInfo, 0, len(reqs))
		for i := range reqs {
			v := &reqs[i]
			if !v.HasLocalOwner() {
				continue
			}
			out = append(out, &waverpc.RoundVTXOInfo{
				AmountSat: int64(v.Amount),
			})
		}

		return out
	}

	switch s := state.(type) {
	case *round.PendingRoundAssembly:
		return "", upcomingFromVTXORequests(s.VTXOs)

	case *round.IntentSentState:
		return "", upcomingFromVTXORequests(s.Intents.VTXOs)

	case *round.QuoteReceivedState:
		return "", upcomingFromVTXORequests(s.Intents.VTXOs)

	case *round.RoundJoinedState:
		return "", upcomingFromVTXORequests(s.Intents.VTXOs)

	case *round.CommitmentTxReceivedState:
		return s.TxID.String(),
			upcomingFromVTXORequests(s.Intents.VTXOs)

	case *round.ConfirmedState:
		out := make([]*waverpc.RoundVTXOInfo, 0, len(s.VTXOs))
		for _, v := range s.VTXOs {
			out = append(out, &waverpc.RoundVTXOInfo{
				Outpoint: fmt.Sprintf(
					"%s:%d", v.Outpoint.Hash,
					v.Outpoint.Index,
				),
				AmountSat: int64(v.Amount),
			})
		}

		return s.TxID.String(), out
	}

	// The MuSig2-phase states (CommitmentTxValidated through
	// InputSigSent) all carry the same CommitmentTx + Intents pair
	// and project onto RoundInfo identically. Fold them into one
	// helper so the case list above does not have to grow every
	// time a new intermediate state is added between commitment
	// validation and input-sig-sent.
	if pkt, vtxos, ok := psbtPhaseDetails(state); ok {
		return commitmentPSBTTxid(pkt),
			upcomingFromVTXORequests(vtxos)
	}

	return "", nil
}

// psbtPhaseDetails returns the commitment PSBT and the in-flight
// VTXO intents from any MuSig2-phase state that carries them. The
// six concrete types share the field shape but are distinct named
// types; type-asserting against each is the price of not adding
// methods to the FSM state contract in the round package.
func psbtPhaseDetails(state round.ClientState) (*psbt.Packet,
	[]types.VTXORequest, bool) {

	switch s := state.(type) {
	case *round.CommitmentTxValidatedState:
		return s.CommitmentTx, s.Intents.VTXOs, true

	case *round.ForfeitSignaturesCollectingState:
		return s.CommitmentTx, s.Intents.VTXOs, true

	case *round.NoncesSentState:
		return s.CommitmentTx, s.Intents.VTXOs, true

	case *round.NoncesAggregatedState:
		return s.CommitmentTx, s.Intents.VTXOs, true

	case *round.PartialSigsSentState:
		return s.CommitmentTx, s.Intents.VTXOs, true

	case *round.InputSigSentState:
		return s.CommitmentTx, s.Intents.VTXOs, true
	}

	return nil, nil, false
}

// commitmentPSBTTxid returns the txid of the unsigned commitment
// transaction carried by an in-flight round PSBT. The MuSig2 phase
// states only carry the PSBT (not a pre-computed hash), so the txid
// is recomputed lazily here.
func commitmentPSBTTxid(p *psbt.Packet) string {
	if p == nil || p.UnsignedTx == nil {
		return ""
	}

	return p.UnsignedTx.TxHash().String()
}

// defaultListRoundsPageSize is the page size used when the client
// does not specify one.
const defaultListRoundsPageSize = 100

// dbStatusToProto maps a persisted round status string to the proto enum.
func dbStatusToProto(status string) waverpc.RoundState {
	switch status {
	case "input_sig_sent":
		return waverpc.RoundState_ROUND_STATE_INPUT_SIG_SENT

	case "confirmed":
		return waverpc.RoundState_ROUND_STATE_CONFIRMED

	case "failed":
		return waverpc.RoundState_ROUND_STATE_FAILED

	default:
		return waverpc.RoundState_ROUND_STATE_UNKNOWN
	}
}

// protoRoundStateToDBStatus maps a round state filter to persisted DB status.
func protoRoundStateToDBStatus(state waverpc.RoundState) (string, bool) {
	switch state {
	case waverpc.RoundState_ROUND_STATE_UNKNOWN:
		return "", true

	case waverpc.RoundState_ROUND_STATE_INPUT_SIG_SENT:
		return "input_sig_sent", true

	case waverpc.RoundState_ROUND_STATE_CONFIRMED:
		return "confirmed", true

	case waverpc.RoundState_ROUND_STATE_FAILED:
		return "failed", true

	default:
		return "", false
	}
}

// ListRounds returns round state information split into two categories:
//   - Pending rounds: live FSM instances from the round actor. Always
//     returned (unless persisted_only is set) and do not count against
//     page_size.
//   - Persisted rounds: rounds stored on disk (input_sig_sent, confirmed,
//     etc.) returned with cursor-based SQL pagination. Each persisted
//     round includes VTXOs created in that round.
//
// Pending rounds appear first in the response, followed by persisted rounds.
func (r *RPCServer) ListRounds(ctx context.Context,
	req *waverpc.ListRoundsRequest) (*waverpc.ListRoundsResponse, error) {

	if req == nil {
		req = &waverpc.ListRoundsRequest{}
	}

	var rounds []*waverpc.RoundInfo

	// Track in-memory round IDs so the persisted-rounds pass below
	// can skip any round that's already represented from the
	// pending pass. Rounds straddle the in-memory and persisted
	// stores during the brief window between server-side
	// confirmation and the round actor evicting the FSM, so without
	// this dedupe a single confirmed round appeared twice in
	// `ark rounds list`.
	seen := fn.NewSet[string]()

	// Always include pending (in-memory) rounds unless the caller
	// explicitly requested persisted-only results.
	if !req.PersistedOnly {
		pending, err := r.queryRoundStates(ctx)
		if err != nil {
			return nil, err
		}

		for _, info := range pending {
			if !roundInfoMatchesFilters(info, req) {
				continue
			}

			// Temp-keyed rounds don't have a persisted-side
			// counterpart yet (the operator hasn't assigned a
			// real round ID), so they can't collide. Only
			// register real round IDs in the dedupe set.
			if !info.IsTemp && info.RoundId != "" {
				seen.Add(info.RoundId)
			}
			rounds = append(rounds, info)
		}
	}

	// Query persisted rounds from SQL with cursor pagination.
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = defaultListRoundsPageSize
	}

	var nextToken string

	if r.server.roundStore != nil {
		statusFilter, includePersisted := protoRoundStateToDBStatus(
			req.GetStateFilter(),
		)
		if !includePersisted {
			return &waverpc.ListRoundsResponse{
				Rounds: rounds,
			}, nil
		}

		// Request one extra row to detect whether a next page
		// exists.
		dbRounds, err := r.server.roundStore.ListRoundsPaginated(
			ctx, db.ListRoundsQuery{
				Cursor:        req.PageToken,
				Limit:         pageSize + 1,
				Status:        statusFilter,
				CreatedAfter:  req.GetCreatedAfter(),
				CreatedBefore: req.GetCreatedBefore(),
			},
		)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to "+
				"list persisted rounds: %v", err)
		}

		// If we got more rows than pageSize, there's a next
		// page.
		if int32(len(dbRounds)) > pageSize {
			dbRounds = dbRounds[:pageSize]
			nextToken = dbRounds[len(dbRounds)-1].RoundID.String()
		}

		for _, s := range dbRounds {
			info := roundSummaryToProto(&s)
			if seen.Contains(info.RoundId) {
				// Already returned by the in-memory pass
				// above; skip the persisted copy.
				continue
			}
			rounds = append(rounds, info)
		}
	}

	return &waverpc.ListRoundsResponse{
		Rounds:        rounds,
		NextPageToken: nextToken,
	}, nil
}

// WatchRounds opens a server-streaming connection that pushes round
// state updates as they occur. The stream polls the round actor and
// sends updates whenever a round's state changes.
func (r *RPCServer) WatchRounds(_ *waverpc.WatchRoundsRequest,
	stream waverpc.DaemonService_WatchRoundsServer) error {

	ctx := stream.Context()

	// Track previous state snapshot to detect changes.
	prevStates := make(map[string]waverpc.RoundState)

	ticker := time.NewTicker(watchRoundsPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
		}

		rounds, err := r.queryRoundStates(ctx)
		if err != nil {
			r.server.log.WarnS(ctx, "WatchRounds poll failed", err)

			continue
		}

		// Diff against previous snapshot and send updates
		// for changed or new rounds.
		for _, info := range rounds {
			key := info.RoundId
			if info.IsTemp {
				key = "temp:" + key
			}

			prev, known := prevStates[key]
			if known && prev == info.State {
				continue
			}

			prevStates[key] = info.State

			if err := stream.Send(
				&waverpc.WatchRoundsResponse{
					Round: info,
				},
			); err != nil {
				return err
			}
		}
	}
}

// rpcMailboxAdapter wraps RPCServer to satisfy
// DaemonServiceMailboxServer. The mailbox transport is unary
// request/response, so server-streaming RPCs like WatchRounds
// return an error indicating they are unsupported over mailbox.
type rpcMailboxAdapter struct {
	*RPCServer
}

// WatchRounds is unsupported over the mailbox transport because it
// is a server-streaming RPC. Callers should use the gRPC transport
// for streaming endpoints.
func (a *rpcMailboxAdapter) WatchRounds(_ context.Context,
	_ *waverpc.WatchRoundsRequest) (*waverpc.WatchRoundsResponse, error) {

	return nil, fmt.Errorf("WatchRounds is a server-streaming RPC and is " +
		"not supported over mailbox transport")
}
