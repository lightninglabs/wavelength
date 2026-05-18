package darepod

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/btcwbackend"
	"github.com/lightninglabs/darepo-client/build"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightninglabs/darepo-client/lwwallet"
	"github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/round"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// manualUnrollAdmissionTimeout bounds the local daemon work needed
	// to accept a manual unroll. The context is detached from the RPC
	// stream so a CLI disconnect does not cancel the unilateral-exit
	// handoff.
	manualUnrollAdmissionTimeout = 30 * time.Second
)

// RPCServer implements the daemon's gRPC DaemonService interface.
type RPCServer struct {
	daemonrpc.UnimplementedDaemonServiceServer

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
}

// NewRPCServer creates a new RPCServer backed by the given Server.
func NewRPCServer(server *Server) *RPCServer {
	return &RPCServer{
		server:           server,
		customInputLocks: make(map[wire.OutPoint]struct{}),
	}
}

// SubLogger returns the daemon logger registered for a subsystem tag. Optional
// RPC subservers use this accessor to share darepod's log manager without
// depending on the private Server type.
func (r *RPCServer) SubLogger(tag string) btclog.Logger {
	if r == nil || r.server == nil {
		return btclog.Disabled
	}

	return r.server.subLogger(tag)
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
	inputTotal, dustLimit btcutil.Amount,
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

	if dustLimit > 0 && change < dustLimit {
		return nil, change, status.Errorf(codes.InvalidArgument, "OOR "+
			"change output %d sat is below dust limit %d sat; "+
			"choose exact inputs or a larger amount", change,
			dustLimit)
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

// walletStateToProto maps the daemon's in-process WalletState enum to
// the public daemonrpc.WalletState wire enum.
func walletStateToProto(s WalletState) daemonrpc.WalletState {
	switch s {
	case WalletStateNone:
		return daemonrpc.WalletState_WALLET_STATE_NONE

	case WalletStateLocked:
		return daemonrpc.WalletState_WALLET_STATE_LOCKED

	case WalletStateReady:
		return daemonrpc.WalletState_WALLET_STATE_READY

	default:
		return daemonrpc.WalletState_WALLET_STATE_UNSPECIFIED
	}
}

// GetInfo returns basic information about the running daemon instance,
// including version, network, and lnd connection state.
func (r *RPCServer) GetInfo(ctx context.Context, _ *daemonrpc.GetInfoRequest) (
	*daemonrpc.GetInfoResponse, error) {

	resp := &daemonrpc.GetInfoResponse{
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
			r.server.log.WarnS(ctx, "Cached operator terms missing "+
				"operator pubkey", nil)

			return resp, nil
		}

		resp.ServerInfo = &daemonrpc.ServerInfo{
			OperatorPubkey:    terms.PubKey.SerializeCompressed(),
			BoardingExitDelay: terms.BoardingExitDelay,
			VtxoExitDelay:     terms.VTXOExitDelay,
			ForfeitScript:     terms.ForfeitScript,
			SweepDelay:        terms.SweepDelay,
			DustLimit:         uint64(terms.DustLimit),
			MinBoardingAmount: uint64(terms.MinBoardingAmount),
			MaxBoardingAmount: uint64(terms.MaxBoardingAmount),
			FeeRate:           uint64(terms.FeeRate),
			MinOperatorFee:    uint64(terms.MinOperatorFee),
			MinConfirmations:  terms.MinConfirmations,
		}

		if terms.SweepKey != nil {
			resp.ServerInfo.SweepKey =
				terms.SweepKey.SerializeCompressed()
		}
	}

	return resp, nil
}

// requireWalletReady returns a gRPC error if the wallet is not yet
// ready. Callers use this to gate RPCs that need wallet access.
func (r *RPCServer) requireWalletReady() error {
	if !r.server.isWalletReady() {
		return status.Errorf(codes.FailedPrecondition, "wallet is "+
			"not ready (create or unlock first)")
	}

	return nil
}

// GetBalance returns the current balance of the wallet, broken down
// by boarding (on-chain) and VTXO (off-chain) balances.
func (r *RPCServer) GetBalance(ctx context.Context,
	_ *daemonrpc.GetBalanceRequest) (*daemonrpc.GetBalanceResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	resp := &daemonrpc.GetBalanceResponse{}

	// Fetch boarding balance from the wallet actor via Ask.
	if r.server.walletRef.IsSome() {
		wRef := r.server.walletRef.UnsafeFromSome()

		balReq := &wallet.GetBoardingBalanceRequest{}
		future := wRef.Ask(ctx, balReq)
		result := future.Await(ctx)

		balResp, err := result.Unpack()
		if err != nil {
			r.server.log.WarnS(
				ctx,
				"Unable to fetch boarding balance",
				err,
			)
		} else {
			br, ok := balResp.(*wallet.GetBoardingBalanceResponse)
			if ok {
				resp.BoardingConfirmedSat = int64(
					br.TotalBalance,
				)
				resp.BoardingPendingSweepSat = int64(
					br.PendingSweepBalance,
				)
				resp.BoardingSweptSat = int64(
					br.SweptBalance,
				)
			}
		}
	}

	// Fetch VTXO balance by summing all live VTXOs using the
	// package-level SumBalance helper.
	if r.server.vtxoStore != nil {
		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(
			ctx,
		)
		if err != nil {
			r.server.log.WarnS(ctx, "Unable to fetch VTXO balance",
				err,
			)
		} else {
			resp.VtxoBalanceSat = int64(
				vtxo.SumBalance(liveVTXOs),
			)
		}
	}

	// Fetch the confirmed balance of the backing on-chain wallet so
	// callers can observe sweep proceeds from unilateral exits.
	fetchers := r.walletBalanceFetchers()
	resp.OnchainWalletConfirmedSat = int64(
		sumOnchainWalletConfirmed(
			ctx, fetchers, func(err error) {
				r.server.log.WarnS(
					ctx,
					"Unable to fetch onchain wallet balance",
					err,
				)
			},
		),
	)

	resp.TotalConfirmedSat = resp.BoardingConfirmedSat +
		resp.VtxoBalanceSat

	return resp, nil
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
// the accumulated confirmed balance. A per-fetcher error is reported
// via onErr and treated as a zero contribution so that one failing
// backend does not mask the balance of another.
func sumOnchainWalletConfirmed(ctx context.Context,
	fetchers []onchainWalletConfirmedFetcher,
	onErr func(err error)) btcutil.Amount {

	var total btcutil.Amount
	for _, fetch := range fetchers {
		confirmed, err := fetch(ctx)
		if err != nil {
			if onErr != nil {
				onErr(err)
			}

			continue
		}

		total += confirmed
	}

	return total
}

// ListVTXOs returns the set of VTXOs known to the wallet, optionally
// filtered by status and minimum amount.
func (r *RPCServer) ListVTXOs(ctx context.Context,
	req *daemonrpc.ListVTXOsRequest) (*daemonrpc.ListVTXOsResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
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
		daemonrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED {

		domainStatus, sErr := protoStatusToDomain(
			req.StatusFilter,
		)
		if sErr != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"invalid status filter: %v", sErr)
		}

		dbVTXOs, err = r.server.vtxoStore.ListVTXOsByStatus(
			ctx, domainStatus,
		)
	} else {
		dbVTXOs, err = r.server.vtxoStore.ListLiveVTXOs(ctx)
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

	var packageStore *db.OORArtifactPersistenceStore
	for i := range filtered {
		if filtered[i].Status == vtxo.VTXOStatusSpent ||
			filtered[i].ChainDepth > 0 {

			packageStore = r.newLocalOORArtifactStore()

			break
		}
	}

	// Convert to proto.
	protoVTXOs := make(
		[]*daemonrpc.VTXO, 0, len(filtered),
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

	return &daemonrpc.ListVTXOsResponse{
		Vtxos: protoVTXOs,
	}, nil
}

// protoStatusToDomain converts a proto VTXOStatus enum to the domain
// vtxo.VTXOStatus type for use with vtxo.FilterDescriptors. An error
// is returned for unknown status values to surface proto/domain drift
// early rather than silently defaulting.
func protoStatusToDomain(s daemonrpc.VTXOStatus) (vtxo.VTXOStatus, error) {
	switch s {
	case daemonrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return vtxo.VTXOStatusLive, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT:
		return vtxo.VTXOStatusPendingForfeit, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return vtxo.VTXOStatusForfeiting, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return vtxo.VTXOStatusForfeited, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return vtxo.VTXOStatusSpent, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT:
		return vtxo.VTXOStatusUnilateralExit, nil

	case daemonrpc.VTXOStatus_VTXO_STATUS_FAILED:
		return vtxo.VTXOStatusFailed, nil

	default:
		return 0, fmt.Errorf("unknown VTXO status: %v", s)
	}
}

// vtxoStatusToProto converts a domain VTXOStatus to the proto enum.
func vtxoStatusToProto(s vtxo.VTXOStatus) daemonrpc.VTXOStatus {
	switch s {
	case vtxo.VTXOStatusLive:
		return daemonrpc.VTXOStatus_VTXO_STATUS_LIVE

	case vtxo.VTXOStatusPendingForfeit:
		return daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT

	case vtxo.VTXOStatusForfeiting:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING

	case vtxo.VTXOStatusForfeited:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED

	case vtxo.VTXOStatusSpent:
		return daemonrpc.VTXOStatus_VTXO_STATUS_SPENT

	case vtxo.VTXOStatusUnilateralExit:
		return daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT

	case vtxo.VTXOStatusFailed:
		return daemonrpc.VTXOStatus_VTXO_STATUS_FAILED

	case vtxo.VTXOStatusSpending:
		return daemonrpc.VTXOStatus_VTXO_STATUS_SPENDING

	default:
		return daemonrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED
	}
}

// descriptorToProto converts a vtxo.Descriptor to the proto VTXO message.
func descriptorToProto(v *vtxo.Descriptor) *daemonrpc.VTXO {
	return &daemonrpc.VTXO{
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
	protoVTXO *daemonrpc.VTXO) error {

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
	_ *daemonrpc.NewAddressRequest) (*daemonrpc.NewAddressResponse, error) {

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

	return &daemonrpc.NewAddressResponse{
		Address: resp.Address.String(),
	}, nil
}

// RefreshVTXOs queues one or more VTXOs for refresh in the next round.
// This extends their expiry without changing ownership. If the all flag
// is set, every live VTXO is queued for refresh.
func (r *RPCServer) RefreshVTXOs(ctx context.Context,
	req *daemonrpc.RefreshVTXOsRequest) (*daemonrpc.RefreshVTXOsResponse,
	error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Internal, "wallet actor not "+
			"initialized")
	}

	wRef := r.server.walletRef.UnsafeFromSome()

	// Build the list of target outpoints. If the "all" selection
	// is set, leave the slice empty so the wallet actor refreshes
	// everything approaching expiry.
	var (
		targets    []wire.OutPoint
		refreshAll bool
	)

	switch sel := req.Selection.(type) {
	case *daemonrpc.RefreshVTXOsRequest_All:
		refreshAll = sel.All

	case *daemonrpc.RefreshVTXOsRequest_Outpoints:
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

		return &daemonrpc.RefreshVTXOsResponse{
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

	// Build the list of outpoints that were successfully queued.
	queued := make([]string, 0, resp.RefreshingCount)
	if refreshAll && r.server.vtxoStore != nil {
		// When refreshing all, list the live VTXOs to
		// report which ones were actually queued.
		liveVTXOs, err := r.server.vtxoStore.ListLiveVTXOs(
			ctx,
		)
		if err == nil {
			for _, v := range liveVTXOs {
				_, hasErr := resp.Errors[v.Outpoint]
				if !hasErr {
					queued = append(
						queued, fmt.Sprintf("%s:%d",
							v.Outpoint.Hash,
							v.Outpoint.Index),
					)
				}
			}
		}
	} else {
		for _, op := range targets {
			if _, hasErr := resp.Errors[op]; !hasErr {
				queued = append(
					queued,
					fmt.Sprintf("%s:%d", op.Hash, op.Index),
				)
			}
		}
	}

	r.server.log.InfoS(ctx, "VTXOs queued for refresh",
		slog.Int("queued_count", len(queued)),
		slog.Int("error_count", len(resp.Errors)),
	)

	return &daemonrpc.RefreshVTXOsResponse{
		QueuedOutpoints: queued,
		Status:          "queued",
	}, nil
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
	req *daemonrpc.LeaveVTXOsRequest) (*daemonrpc.LeaveVTXOsResponse,
	error) {

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
	case *daemonrpc.LeaveVTXOsRequest_All:
		leaveAll = sel.All
		if len(req.Destinations) > 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"per-outpoint destinations not supported "+
					"with selection=all")
		}

	case *daemonrpc.LeaveVTXOsRequest_Outpoints:
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

		return &daemonrpc.LeaveVTXOsResponse{
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

	return &daemonrpc.LeaveVTXOsResponse{
		QueuedOutpoints: queued,
		Status:          "queued",
	}, nil
}

// Board triggers the client to join the next round with any confirmed
// boarding UTXOs. The RPC delegates the full flow to the wallet actor:
// balance check, VTXO amount computation, and round registration. It
// returns immediately after the wallet accepts the request; use
// ListRounds/WatchRounds to observe round progress.
func (r *RPCServer) Board(ctx context.Context, req *daemonrpc.BoardRequest) (
	*daemonrpc.BoardResponse, error) {

	if req == nil {
		req = &daemonrpc.BoardRequest{}
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

			return &daemonrpc.BoardResponse{
				Status: "no_boarding_utxos",
			}, nil

		case strings.Contains(errStr, "too small after"):
			return nil, status.Errorf(codes.FailedPrecondition,
				"boarding balance too small: %v", err)
		}

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

	return &daemonrpc.BoardResponse{
		Status:    "registered",
		VtxoCount: uint32(len(boardResp.VTXOAmounts)),
	}, nil
}

// SendVTXO initiates an in-round directed transfer by forfeiting
// existing VTXOs and creating new recipient VTXOs in the same round.
// Coin selection, reservation, and round registration are handled
// atomically by the wallet actor.
func (r *RPCServer) SendVTXO(ctx context.Context,
	req *daemonrpc.SendVTXORequest) (*daemonrpc.SendVTXOResponse, error) {

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
		DustLimit:     terms.DustLimit,
		OperatorKey:   terms.PubKey,
		VTXOExitDelay: terms.VTXOExitDelay,
		DryRun:        req.DryRun,
	}

	future := wRef.Ask(ctx, sendReq)
	result := future.Await(ctx)

	resp, err := result.Unpack()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "send failed: %v",
			err)
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

	return &daemonrpc.SendVTXOResponse{
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
func (r *RPCServer) SendOOR(ctx context.Context,
	req *daemonrpc.SendOORRequest) (*daemonrpc.SendOORResponse, error) {

	startTime := time.Now()
	var (
		idempotencyDuration    time.Duration
		resolveScriptDuration  time.Duration
		operatorTermsDuration  time.Duration
		policyResolveDuration  time.Duration
		inputSelectDuration    time.Duration
		buildInputsDuration    time.Duration
		changeOutputDuration   time.Duration
		oorCoordinatorDuration time.Duration
	)

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	if req.Recipient == nil {
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"is required")
	}

	if req.Recipient.AmountSat <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount "+
			"must be positive")
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

			return &daemonrpc.SendOORResponse{
				Status:    "submitted",
				SessionId: sessionID.String(),
			}, nil
		}
	}

	// Resolve the recipient's pkScript from the destination
	// oneof. Pubkey destinations need operator terms to derive
	// VTXO-compatible taproot outputs, so we pass a lazy fetcher.
	phaseStart := time.Now()
	pkScript, err := r.resolveOutputPkScript(ctx, req.Recipient)
	resolveScriptDuration = time.Since(phaseStart)
	if err != nil {
		return nil, err
	}

	// For dry_run, validate inputs and return a preview without
	// contacting the operator.
	if req.DryRun {
		return &daemonrpc.SendOORResponse{
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

	if r.server.oorCoordinator == nil {
		return nil, status.Errorf(codes.Internal, "OOR coordinator "+
			"not initialized")
	}

	// Fetch operator terms for the checkpoint policy.
	phaseStart = time.Now()
	terms, err := r.server.fetchOperatorTerms(ctx)
	operatorTermsDuration = time.Since(phaseStart)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "unable to fetch "+
			"operator terms: %v", err)
	}

	policy := arkscript.CheckpointPolicy{
		OperatorKey: terms.PubKey,
		CSVDelay:    terms.VTXOExitDelay,
	}

	phaseStart = time.Now()
	recipientPolicyTemplate, err := r.resolveOutputPolicyTemplate(
		ctx, req.Recipient, pkScript, terms.PubKey, terms.VTXOExitDelay,
	)
	policyResolveDuration = time.Since(phaseStart)
	if err != nil {
		return nil, err
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
		// Custom inputs typically refer to non-standard VTXOs (e.g.
		// vHTLC claim outpoints) that live outside the wallet's managed
		// set and therefore are not reserved through the VTXO manager's
		// PendingForfeit flow. Without any dedup, two concurrent
		// SendOOR callers supplying the same custom input could each
		// succeed through BuildCustomTransferInputs and double-sign the
		// same outpoint. We gate that race here with an in-memory
		// reservation keyed by outpoint; the reservation is released
		// regardless of success or failure via defer. The direct
		// coordinator call below is synchronous, so there is no
		// detached OOR work that can outlive the RPC handler.
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
	} else {
		// Standard path: select and lock VTXOs from wallet.
		phaseStart = time.Now()
		targetAmt := btcutil.Amount(req.Recipient.AmountSat)
		wRef := r.server.walletRef.UnsafeFromSome()

		selectReq := &wallet.SelectAndLockVTXOsRequest{
			TargetAmount: targetAmt,
		}
		selectFuture := wRef.Ask(ctx, selectReq)
		selectResult := selectFuture.Await(ctx)

		selectResp, err := selectResult.Unpack()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "VTXO "+
				"selection failed: %v", err)
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

	targetAmt := btcutil.Amount(req.Recipient.AmountSat)

	// Build the recipient output for the OOR transfer.
	recipients := []oortx.RecipientOutput{{
		PkScript:           pkScript,
		Value:              targetAmt,
		VTXOPolicyTemplate: recipientPolicyTemplate,
	}}

	phaseStart = time.Now()
	inputTotal, err := sumOORInputAmounts(selectedInputs)
	if err != nil {
		r.unlockSelectedVTXOsBestEffort(ctx, locked)

		return nil, status.Errorf(codes.Internal, "sum OOR input "+
			"amounts: %v", err)
	}

	recipients, changeAmt, err := appendOORChangeRecipient(
		ctx, recipients, inputTotal, terms.DustLimit,
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

	oorReq := &oor.StartTransferRequest{
		Policy:         policy,
		Inputs:         selectedInputs,
		Recipients:     recipients,
		IdempotencyKey: req.GetIdempotencyKey(),
	}

	phaseStart = time.Now()
	oorCtx := actor.WithoutTx(ctx)
	oorResp, oorErr := r.server.oorCoordinator.Receive(
		oorCtx, oorReq,
	).Unpack()
	oorCoordinatorDuration = time.Since(phaseStart)

	if oorErr != nil {
		// Unlock VTXOs on OOR failure so they can be
		// reused (only for wallet-selected inputs).
		r.unlockSelectedVTXOsBestEffort(ctx, locked)

		return nil, status.Errorf(codes.Internal, "OOR transfer "+
			"failed: %v", oorErr)
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

	r.server.log.InfoS(ctx, "OOR transfer submitted",
		slog.String("session_id", resp.SessionID.String()),
		slog.Bool("existing_session", resp.Existing),
		slog.Int64("amount_sat", req.Recipient.AmountSat),
		slog.Int64("input_total_sat", int64(inputTotal)),
		slog.Int64("change_sat", int64(changeAmt)),
		slog.Int("recipient_count", len(recipients)),
		slog.Duration("duration", time.Since(startTime)),
		slog.Duration("idempotency_duration", idempotencyDuration),
		slog.Duration("resolve_script_duration",
			resolveScriptDuration),
		slog.Duration("operator_terms_duration",
			operatorTermsDuration),
		slog.Duration("policy_resolve_duration",
			policyResolveDuration),
		slog.Duration("input_select_duration",
			inputSelectDuration),
		slog.Duration("build_inputs_duration",
			buildInputsDuration),
		slog.Duration("change_output_duration",
			changeOutputDuration),
		slog.Duration(
			"oor_coordinator_duration", oorCoordinatorDuration,
		))

	return &daemonrpc.SendOORResponse{
		Status:    "submitted",
		SessionId: resp.SessionID.String(),
	}, nil
}

// findOutgoingOORSessionByIdempotencyKey asks the OOR coordinator whether the
// daemon already knows a keyed outgoing session before acquiring wallet or
// custom inputs for the retry.
func (r *RPCServer) findOutgoingOORSessionByIdempotencyKey(ctx context.Context,
	idempotencyKey string) (oor.SessionID, bool, error) {

	findReq := &oor.FindOutgoingSessionByIdempotencyKeyRequest{
		IdempotencyKey: idempotencyKey,
	}

	if r.server.oorCoordinator == nil {
		return oor.SessionID{}, false, status.Errorf(codes.Internal,
			"OOR coordinator not initialized")
	}

	actorResp, err := r.server.oorCoordinator.Receive(
		ctx, findReq,
	).Unpack()
	if err != nil {
		return oor.SessionID{}, false, status.Errorf(codes.Internal,
			"OOR idempotency lookup failed: %v", err)
	}

	resp, ok := actorResp.(*oor.FindOutgoingSessionByIdempotencyKeyResponse)
	if !ok {
		return oor.SessionID{}, false, status.Errorf(codes.Internal,
			"unexpected response type: %T", actorResp)
	}

	return resp.SessionID, resp.Found, nil
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

// addrNetName returns a best-effort network name for a decoded
// address so cross-network error messages can be specific. We
// iterate the four standard nets and pick the one the address
// claims IsForNet on; if none match (structurally impossible for
// anything DecodeAddress returned successfully) we fall back to
// "unknown" rather than panic.
func addrNetName(addr btcutil.Address) string {
	for _, p := range []*chaincfg.Params{
		&chaincfg.MainNetParams,
		&chaincfg.TestNet3Params,
		&chaincfg.SigNetParams,
		&chaincfg.RegressionNetParams,
	} {
		if addr.IsForNet(p) {
			return p.Name
		}
	}

	return "unknown"
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
func (r *RPCServer) resolveLeaveDestination(d *daemonrpc.LeaveDestination) (
	[]byte, error) {

	if d == nil {
		return nil, fmt.Errorf("leave destination is required")
	}

	switch t := d.Target.(type) {
	case *daemonrpc.LeaveDestination_Address:
		if t.Address == "" {
			return nil, fmt.Errorf("leave destination address is " +
				"empty")
		}

		addr, err := btcutil.DecodeAddress(
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

	case *daemonrpc.LeaveDestination_PkScript:
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
func (r *RPCServer) resolveRecipientOutput(out *daemonrpc.Output) ([]byte,
	*btcec.PublicKey, error) {

	switch d := out.Destination.(type) {
	case *daemonrpc.Output_Pubkey:
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
		addr, err := btcutil.NewAddressTaproot(
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

	case *daemonrpc.Output_Address:
		addr, err := btcutil.DecodeAddress(
			d.Address, r.server.chainParams,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid address: %w", err)
		}

		// Only taproot addresses carry the x-only pubkey
		// needed for VTXO construction.
		tapAddr, ok := addr.(*btcutil.AddressTaproot)
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

	case *daemonrpc.Output_PolicyTemplate:
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
	out *daemonrpc.Output, pkScript []byte, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]byte, error) {

	if out == nil {
		return nil, status.Errorf(codes.InvalidArgument, "recipient "+
			"must be provided")
	}

	switch d := out.Destination.(type) {
	case *daemonrpc.Output_PolicyTemplate:
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
	case *daemonrpc.Output_Pubkey:
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
	out *daemonrpc.Output) ([]byte, error) {

	switch d := out.Destination.(type) {
	case *daemonrpc.Output_Address:
		addr, err := btcutil.DecodeAddress(
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

	case *daemonrpc.Output_Pubkey:
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

	case *daemonrpc.Output_PolicyTemplate:
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
func (r *RPCServer) Unroll(ctx context.Context, req *daemonrpc.UnrollRequest) (
	*daemonrpc.UnrollResponse, error) {

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
	if r.server.vtxoStore != nil {
		desc, err := r.server.vtxoStore.GetVTXO(admissionCtx, outpoint)
		if err == nil && desc != nil &&
			desc.Status == vtxo.VTXOStatusUnilateralExit {
			return &daemonrpc.UnrollResponse{
				Created: false,
				ActorId: unroll.ActorIDForTarget(outpoint),
			}, nil
		}
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

	return &daemonrpc.UnrollResponse{
		Created: unrollResp.Accepted,
		ActorId: actorID,
	}, nil
}

// GetUnrollStatus returns the current status of an unroll job for the
// given VTXO outpoint.
func (r *RPCServer) GetUnrollStatus(ctx context.Context,
	req *daemonrpc.GetUnrollStatusRequest) (
	*daemonrpc.GetUnrollStatusResponse, error) {

	outpoint, err := parseOutpointString(req.Outpoint)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"outpoint: %v", err)
	}

	if r.server.unrollRegistryRef.IsSome() {
		resp, found, err := r.queryUnrollRegistry(
			ctx, outpoint,
		)
		if err != nil {
			return nil, err
		}
		if found {
			return resp, nil
		}
	}

	if r.server.unrollJobStore == nil {
		return &daemonrpc.GetUnrollStatusResponse{
			Found: false,
		}, nil
	}

	job, err := r.server.unrollJobStore.GetJob(ctx, outpoint)
	if errors.Is(err, db.ErrUnrollJobNotFound) {
		return &daemonrpc.GetUnrollStatusResponse{
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

	return &daemonrpc.GetUnrollStatusResponse{
		Found:     true,
		Status:    unrollJobStateToProto(job.State),
		SweepTxid: sweepTxid,
		LastError: job.FailReason,
	}, nil
}

// queryUnrollRegistry asks the live unroll registry actor for the
// status of a target outpoint. It returns the proto response, whether
// the job was found, and any RPC error.
func (r *RPCServer) queryUnrollRegistry(ctx context.Context,
	outpoint wire.OutPoint) (*daemonrpc.GetUnrollStatusResponse, bool,
	error) {

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

	return &daemonrpc.GetUnrollStatusResponse{
		Found:     true,
		Status:    unrollPhaseToProto(phase),
		SweepTxid: sweepTxid,
		LastError: lastError,
	}, true, nil
}

// unrollPhaseToProto maps the new unroll phase enum to the proto enum.
func unrollPhaseToProto(phase unroll.Phase) daemonrpc.UnrollJobStatus {
	switch phase {
	case unroll.PhasePending:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING

	case unroll.PhaseCSVPending:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING

	case unroll.PhaseSweepBroadcast, unroll.PhaseSweepConfirmation:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING

	case unroll.PhaseCompleted:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED

	case unroll.PhaseFailed:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED

	default:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING
	}
}

// unrollJobStateToProto maps the internal SQL job state to the proto
// enum.
func unrollJobStateToProto(state string) daemonrpc.UnrollJobStatus {
	switch unroll.Phase(state) {
	case unroll.PhasePending:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING

	case unroll.PhaseMaterializing:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING

	case unroll.PhaseCSVPending:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING

	case unroll.PhaseSweepBroadcast, unroll.PhaseSweepConfirmation:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING

	case unroll.PhaseCompleted:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED

	case unroll.PhaseFailed:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED

	default:
		return daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_UNSPECIFIED
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
func clientStateToProto(state round.ClientState) daemonrpc.RoundState {
	switch state.(type) {
	case *round.Idle:
		return daemonrpc.RoundState_ROUND_STATE_IDLE

	case *round.PendingRoundAssembly:
		return daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY

	case *round.IntentSentState:
		return daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT

	case *round.QuoteReceivedState:
		return daemonrpc.RoundState_ROUND_STATE_QUOTE_RECEIVED

	case *round.RoundJoinedState:
		return daemonrpc.RoundState_ROUND_STATE_JOINED

	case *round.CommitmentTxReceivedState:
		return daemonrpc.RoundState_ROUND_STATE_COMMITMENT_RECEIVED

	case *round.CommitmentTxValidatedState:
		return daemonrpc.RoundState_ROUND_STATE_COMMITMENT_VALIDATED

	case *round.ForfeitSignaturesCollectingState:
		return daemonrpc.RoundState_ROUND_STATE_FORFEIT_COLLECTING

	case *round.NoncesSentState:
		return daemonrpc.RoundState_ROUND_STATE_NONCES_SENT

	case *round.NoncesAggregatedState:
		return daemonrpc.RoundState_ROUND_STATE_NONCES_AGGREGATED

	case *round.PartialSigsSentState:
		return daemonrpc.RoundState_ROUND_STATE_PARTIAL_SIGS_SENT

	case *round.InputSigSentState:
		return daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT

	case *round.ConfirmedState:
		return daemonrpc.RoundState_ROUND_STATE_CONFIRMED

	case *round.ClientFailedState:
		return daemonrpc.RoundState_ROUND_STATE_FAILED

	case *round.RecoveryInitiatedState:
		return daemonrpc.RoundState_ROUND_STATE_RECOVERY

	default:
		return daemonrpc.RoundState_ROUND_STATE_UNKNOWN
	}
}

// queryRoundStates fetches the current FSM states from the round actor
// and converts them to proto RoundInfo messages.
func (r *RPCServer) queryRoundStates(ctx context.Context) (
	[]*daemonrpc.RoundInfo, error) {

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
		[]*daemonrpc.RoundInfo, 0, len(stateResp.States),
	)
	for _, info := range stateResp.States {
		roundID := ""
		if !info.IsTemp {
			roundID = info.RoundID.String()
		}

		rounds = append(rounds, &daemonrpc.RoundInfo{
			RoundId:       roundID,
			State:         clientStateToProto(info.State),
			IsTemp:        info.IsTemp,
			FailureReason: roundFailureReason(info.State),
		})
	}

	return rounds, nil
}

// defaultListRoundsPageSize is the page size used when the client
// does not specify one.
const defaultListRoundsPageSize = 100

// dbStatusToProto maps a persisted round status string to the proto enum.
func dbStatusToProto(status string) daemonrpc.RoundState {
	switch status {
	case "input_sig_sent":
		return daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT

	case "confirmed":
		return daemonrpc.RoundState_ROUND_STATE_CONFIRMED

	case "failed":
		return daemonrpc.RoundState_ROUND_STATE_FAILED

	default:
		return daemonrpc.RoundState_ROUND_STATE_UNKNOWN
	}
}

// protoRoundStateToDBStatus maps a round state filter to persisted DB status.
func protoRoundStateToDBStatus(state daemonrpc.RoundState) (string, bool) {
	switch state {
	case daemonrpc.RoundState_ROUND_STATE_UNKNOWN:
		return "", true

	case daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT:
		return "input_sig_sent", true

	case daemonrpc.RoundState_ROUND_STATE_CONFIRMED:
		return "confirmed", true

	case daemonrpc.RoundState_ROUND_STATE_FAILED:
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
	req *daemonrpc.ListRoundsRequest) (*daemonrpc.ListRoundsResponse,
	error) {

	if req == nil {
		req = &daemonrpc.ListRoundsRequest{}
	}

	var rounds []*daemonrpc.RoundInfo

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
			return &daemonrpc.ListRoundsResponse{
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
			rounds = append(rounds, info)
		}
	}

	return &daemonrpc.ListRoundsResponse{
		Rounds:        rounds,
		NextPageToken: nextToken,
	}, nil
}

// WatchRounds opens a server-streaming connection that pushes round
// state updates as they occur. The stream polls the round actor and
// sends updates whenever a round's state changes.
func (r *RPCServer) WatchRounds(_ *daemonrpc.WatchRoundsRequest,
	stream daemonrpc.DaemonService_WatchRoundsServer) error {

	ctx := stream.Context()

	// Track previous state snapshot to detect changes.
	prevStates := make(map[string]daemonrpc.RoundState)

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
				&daemonrpc.WatchRoundsResponse{
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
	_ *daemonrpc.WatchRoundsRequest) (*daemonrpc.WatchRoundsResponse,
	error) {

	return nil, fmt.Errorf("WatchRounds is a server-streaming RPC and is " +
		"not supported over mailbox transport")
}
