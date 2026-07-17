package wallet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/walletcore"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
)

const (
	// walletSweepTxVersion is the transaction version used for the single
	// aggregate backing-wallet sweep transaction. It must be the TRUC (v3)
	// version because the signed sweep is broadcast through txconfirm,
	// whose CPFPBroadcaster.Submit rejects any non-v3 parent with
	// ErrNonTRUCParent before it ever reaches the chain backend. The sweep
	// carries no anchor output and pays its own fee, so the broadcaster
	// takes the direct (no CPFP child) path -- but the v3 version gate
	// still applies. v3 retains the relative-locktime-enabled sequence
	// semantics the v2 builder relied on, so nothing about the input
	// sequences changes.
	walletSweepTxVersion int32 = arktx.TxVersion

	// walletSweepDustFailureReason is the human-readable reason returned
	// when the sweep's net amount (gross inputs minus fee) does not clear
	// the dust limit, so there is nothing meaningful to broadcast.
	walletSweepDustFailureReason = "sweep amount is dust after fees"

	// walletSweepBroadcastLabel is attached to broadcasts of the general
	// backing-wallet sweep transaction for chain-backend-side log
	// correlation.
	walletSweepBroadcastLabel = "ark wallet backing sweep"

	// defaultWalletSweepConfTarget is the confirmation target used when the
	// caller does not specify an explicit fee rate or conf target. It
	// mirrors the exit-plan default the legacy RPC used.
	defaultWalletSweepConfTarget uint32 = 6
)

// walletSweepLockID is the caller-scoped output lease identifier under which
// the wallet actor reserves the UTXOs that feed a general backing-wallet
// sweep. Deriving it from a stable, human-readable prefix keeps the lease
// namespace distinct from txconfirm's CPFP fee-input leases and from the
// boarding-sweep flow, so the subsystems never release each other's leases.
var walletSweepLockID = func() walletcore.LockID {
	var id walletcore.LockID
	copy(id[:], "wavelength:sweepwallet")

	return id
}()

// WalletBackingSweeper is the narrow backend surface the wallet actor needs to
// preview, sign, and broadcast a general backing-wallet sweep. The concrete
// per-backend adapters (lndUnrollWallet / lwUnrollWallet / btcwUnrollWallet)
// already satisfy this set of methods because they implement txconfirm.Wallet;
// the interface is declared here with the exact same method signatures so the
// adapter types satisfy it structurally without modification.
type WalletBackingSweeper interface {
	// ListUnspent returns confirmed wallet UTXOs in the requested
	// confirmation range. The general sweep asks for everything with at
	// least one confirmation.
	ListUnspent(ctx context.Context, minConfs,
		maxConfs int32) ([]*Utxo, error)

	// FinalizePsbt signs and finalizes a PSBT packet. The wallet signs all
	// inputs it owns and returns the finalized wire tx.
	FinalizePsbt(ctx context.Context, packet []byte) (*wire.MsgTx, error)

	// LeaseOutput locks the named outpoint against the caller's LockID for
	// at least the supplied expiry, returning the absolute time at which
	// the lock will auto-release.
	LeaseOutput(ctx context.Context, id LockID, op wire.OutPoint,
		expiry time.Duration) (time.Time, error)

	// ReleaseOutput drops the caller's lease on the named outpoint.
	ReleaseOutput(ctx context.Context, id LockID, op wire.OutPoint) error
}

// WalletSweepInputInfo describes one backing-wallet UTXO selected for an
// aggregate wallet sweep.
type WalletSweepInputInfo struct {
	// Outpoint uniquely identifies the swept UTXO.
	Outpoint wire.OutPoint

	// AmountSat is the value of the swept UTXO in satoshis.
	AmountSat int64
}

// SweepWalletFundsRequest asks the wallet actor to preview, and optionally
// broadcast, a general backing-wallet sweep that drains every confirmed wallet
// UTXO (excluding boarding outputs, which the backend's ListUnspent does not
// surface) to a single destination address.
type SweepWalletFundsRequest struct {
	actor.BaseMessage

	// DestinationAddress is the address the swept funds are sent to. It is
	// validated against the wallet's configured chain network.
	DestinationAddress string

	// Broadcast controls whether to publish the sweep. When false the
	// actor returns a preview without leasing inputs, signing, or
	// broadcasting.
	Broadcast bool

	// FeeRateSatPerVByte is the explicit fee rate. When zero the actor
	// estimates the fee rate via the chainsource actor at ConfTarget. The
	// resolved rate is always capped (see applyWalletSweepFeeCap).
	FeeRateSatPerVByte int64

	// ConfTarget is the confirmation target used when estimating fees. Zero
	// falls back to defaultWalletSweepConfTarget.
	ConfTarget uint32
}

// MessageType returns the message type identifier.
func (m *SweepWalletFundsRequest) MessageType() string {
	return "SweepWalletFundsRequest"
}

func (m *SweepWalletFundsRequest) walletMsgSealed() {}

// SweepWalletFundsResponse is the wallet actor's reply to a
// SweepWalletFundsRequest. It carries the selected inputs and aggregate
// preview math, and — on a successful broadcast — the resulting txid.
type SweepWalletFundsResponse struct {
	actor.BaseMessage

	// Inputs are the confirmed backing-wallet UTXOs selected for the sweep.
	Inputs []WalletSweepInputInfo

	// TotalInputSat is the gross value of every selected input.
	TotalInputSat int64

	// EstimatedFeeSat is the absolute miner fee for the aggregate sweep tx
	// at the resolved fee rate.
	EstimatedFeeSat int64

	// NetAmountSat is TotalInputSat - EstimatedFeeSat: the amount paid to
	// the destination.
	NetAmountSat int64

	// FeeRateSatPerVByte is the (capped) fee rate used to build the tx.
	FeeRateSatPerVByte int64

	// CanBroadcast is true when the preview cleared the dust floor and the
	// sweep can be published.
	CanBroadcast bool

	// Txid is the broadcast sweep transaction id, when one was published.
	Txid chainhash.Hash

	// HasTxid is true when Txid is meaningful (i.e. the sweep was
	// broadcast).
	HasTxid bool

	// FailureReason is populated on a preview that cannot broadcast (no
	// inputs, dust) or a broadcast path that failed after preview.
	FailureReason string
}

// MessageType returns the message type identifier.
func (m *SweepWalletFundsResponse) MessageType() string {
	return "SweepWalletFundsResponse"
}

func (m *SweepWalletFundsResponse) walletRespSealed() {}

// WalletSweepTxNotification is a Tell carrying a txconfirm terminal
// notification (confirmation or failure) for a general backing-wallet sweep
// tx, re-wrapped from txconfirm.TxConfirmed / txconfirm.TxFailed via
// txconfirm.MapNotification. Unlike the boarding-sweep equivalent it carries no
// store-reconciliation semantics: general wallet sweeps are not persisted (the
// lean decision — an interrupted manual sweep is simply re-run), so the
// handler only logs.
type WalletSweepTxNotification struct {
	actor.BaseMessage

	// Confirmed is true when the underlying txconfirm.TxConfirmed event
	// fired; false when it was txconfirm.TxFailed.
	Confirmed bool

	// Txid identifies the tracked sweep transaction.
	Txid chainhash.Hash

	// BlockHeight is the height at which the sweep confirmed when
	// Confirmed=true; zero otherwise.
	BlockHeight int32

	// NumConfs is the confirmation count when Confirmed=true; zero
	// otherwise.
	NumConfs uint32

	// Reason is the human-readable failure reason when Confirmed=false.
	Reason string
}

// MessageType returns the message type identifier.
func (m WalletSweepTxNotification) MessageType() string {
	return "WalletSweepTxNotification"
}

func (m WalletSweepTxNotification) walletMsgSealed() {}

// WalletSweepNotificationAck is the empty reply to wallet-sweep tx
// notifications. The notification is Tell semantically; the wallet's generic
// Receive shape requires a typed response so we return this no-op ack.
type WalletSweepNotificationAck struct {
	actor.BaseMessage
}

// MessageType returns the message type identifier.
func (m *WalletSweepNotificationAck) MessageType() string {
	return "WalletSweepNotificationAck"
}

func (m *WalletSweepNotificationAck) walletRespSealed() {}

// walletSweepEnabled reports whether the general wallet-sweep backend was wired
// at NewArk time. When false, sweep requests return a clear error rather than
// silently no-oping.
func (a *Ark) walletSweepEnabled() bool {
	return a.walletSweeper != nil
}

// failedWalletSweepResponse builds a SweepWalletFundsResponse carrying a
// human-readable failure reason. The actor response itself remains successful
// so the RPC sees the failure as an application-level condition rather than a
// transport error.
func failedWalletSweepResponse(resp *SweepWalletFundsResponse,
	err error) fn.Result[WalletResp] {

	if err != nil {
		resp.FailureReason = err.Error()
	}
	resp.CanBroadcast = false

	return fn.Ok[WalletResp](resp)
}

// handleSweepWalletFunds is the wallet actor's entry point for a general
// backing-wallet sweep. It validates the destination, resolves and caps the
// fee rate, lists confirmed UTXOs, builds the preview, and — when broadcast is
// requested and the preview clears the dust floor — leases the inputs, signs
// the single-output sweep, and submits it to the shared txconfirm broadcaster.
func (a *Ark) handleSweepWalletFunds(ctx context.Context,
	req *SweepWalletFundsRequest) fn.Result[WalletResp] {

	if !a.walletSweepEnabled() {
		return fn.Err[WalletResp](
			errors.New("wallet sweep subsystem not initialised"),
		)
	}
	if req.FeeRateSatPerVByte < 0 {
		return fn.Err[WalletResp](
			errors.New("fee_rate_sat_per_vbyte must be " +
				"non-negative"),
		)
	}
	if req.DestinationAddress == "" {
		return fn.Err[WalletResp](
			errors.New("destination_address is required"),
		)
	}

	log := a.logger(ctx)

	// A wallet sweep is an operator-initiated, fund-moving action, so log
	// its entry at info with the request shape that determines the outcome.
	log.InfoS(ctx, "Wallet sweep requested",
		slog.String("destination", req.DestinationAddress),
		slog.Bool("broadcast", req.Broadcast),
		slog.Int64("fee_rate_sat_per_vbyte", req.FeeRateSatPerVByte),
	)

	// Decode and network-validate the destination address against the
	// configured chain params before doing any work. A wrong-network
	// address is an operator input error, surfaced as a transport error so
	// the RPC maps it to InvalidArgument.
	destScript, err := a.walletSweepDestScript(req.DestinationAddress)
	if err != nil {
		return fn.Err[WalletResp](err)
	}

	// Resolve the fee rate (explicit or estimated) and then ALWAYS cap it.
	// The cap is never a no-op: when no operator max is configured it falls
	// back to txconfirm.DefaultMaxFeeRateSatPerVByte.
	resolvedFeeRate, err := a.resolveWalletSweepFeeRate(
		ctx, req.FeeRateSatPerVByte, req.ConfTarget,
	)
	if err != nil {
		return fn.Err[WalletResp](err)
	}
	feeRate := a.applyWalletSweepFeeCap(resolvedFeeRate)

	// The cap is never a no-op; trace when it actually clamped the resolved
	// rate so an operator can tell their requested rate was lowered.
	if feeRate < resolvedFeeRate {
		log.DebugS(ctx, "Wallet sweep fee rate capped",
			slog.Int64("resolved_sat_per_vbyte", resolvedFeeRate),
			slog.Int64("capped_sat_per_vbyte", feeRate),
		)
	}

	// A high fee rate on a wallet-draining sweep is worth flagging to the
	// operator even when it is within the configured cap, mirroring the
	// boarding-sweep warning.
	if feeRate >= boardingSweepHighFeeRateWarningSatPerVByte {
		log.WarnS(ctx, "Wallet sweep fee rate is unusually high",
			nil,
			slog.Int64("fee_rate_sat_per_vbyte", feeRate),
		)
	}

	utxos, err := a.walletSweeper.ListUnspent(
		ctx, 1, MaxConfsForListUnspent,
	)
	if err != nil {
		return fn.Err[WalletResp](
			fmt.Errorf("list wallet unspent: %w", err),
		)
	}

	resp := walletSweepPreview(utxos, destScript, feeRate)

	// Trace the preview math so a preview-only request (or the broadcast
	// gate below) leaves a record of what was selected and whether it could
	// have been published.
	log.DebugS(ctx, "Wallet sweep preview built",
		slog.Int("num_inputs", len(resp.Inputs)),
		slog.Int64("total_input_sat", resp.TotalInputSat),
		slog.Int64("estimated_fee_sat", resp.EstimatedFeeSat),
		slog.Int64("net_amount_sat", resp.NetAmountSat),
		slog.Bool("can_broadcast", resp.CanBroadcast),
	)

	if !req.Broadcast {
		return fn.Ok[WalletResp](resp)
	}
	if !resp.CanBroadcast {

		// Nothing to broadcast (no inputs or dust); return the preview
		// with the failure reason already populated by the preview.
		return fn.Ok[WalletResp](resp)
	}

	return a.broadcastWalletSweep(ctx, log, utxos, destScript, resp)
}

// walletSweepDestScript decodes the caller-supplied destination address,
// validates it for the wallet's configured network, and returns the
// destination pkScript.
func (a *Ark) walletSweepDestScript(address string) (txscript.PkScript, error) {
	var zero txscript.PkScript

	addr, err := btcaddr.DecodeAddress(address, a.sweepChainParams)
	if err != nil {
		return zero, fmt.Errorf("invalid destination_address: %w", err)
	}
	if !addr.IsForNet(a.sweepChainParams) {
		return zero, errors.New("destination_address is for the " +
			"wrong network")
	}

	scriptBytes, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return zero, fmt.Errorf("destination script: %w", err)
	}
	destScript, err := txscript.ParsePkScript(scriptBytes)
	if err != nil {
		return zero, fmt.Errorf("destination script: %w", err)
	}

	return destScript, nil
}

// resolveWalletSweepFeeRate returns the explicit fee rate when the caller
// supplied one, otherwise estimates it via the chainsource actor at the
// resolved conf target. A zero conf target falls back to the default. The
// returned rate is NOT yet capped — callers must run applyWalletSweepFeeCap.
func (a *Ark) resolveWalletSweepFeeRate(ctx context.Context,
	feeRateSatPerVByte int64, confTarget uint32) (int64, error) {

	if feeRateSatPerVByte > 0 {
		return feeRateSatPerVByte, nil
	}
	if confTarget == 0 {
		confTarget = defaultWalletSweepConfTarget
	}

	feeRate, err := a.askFeeEstimate(ctx, confTarget)
	if err != nil {
		return 0, fmt.Errorf("estimate fee: %w", err)
	}
	if int64(feeRate) <= 0 {
		return 0, errors.New("fee estimate returned non-positive rate")
	}

	return int64(feeRate), nil
}

// applyWalletSweepFeeCap caps the resolved fee rate. This is the H-2 fix: the
// cap must never be a no-op. When the operator configured a positive max fee
// rate the rate is capped to it; otherwise the rate is capped UNCONDITIONALLY
// to txconfirm.DefaultMaxFeeRateSatPerVByte so a runaway explicit or estimated
// rate cannot drain the wallet to miners.
func (a *Ark) applyWalletSweepFeeCap(feeRate int64) int64 {
	maxRate := a.walletSweepMaxFeeRate
	if maxRate <= 0 {
		maxRate = txconfirm.DefaultMaxFeeRateSatPerVByte
	}
	if feeRate > maxRate {
		return maxRate
	}

	return feeRate
}

// walletSweepPreview builds the preview response for a general wallet sweep:
// it sums the input value, estimates the signed-tx vsize via the same
// script-class logic the broadcaster uses, derives the fee and net amount, and
// gates CanBroadcast on a positive net amount above the dust floor.
func walletSweepPreview(utxos []*Utxo, destScript txscript.PkScript,
	feeRate int64) *SweepWalletFundsResponse {

	resp := &SweepWalletFundsResponse{
		FeeRateSatPerVByte: feeRate,
		Inputs:             make([]WalletSweepInputInfo, 0, len(utxos)),
	}

	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		resp.Inputs = append(resp.Inputs, WalletSweepInputInfo{
			Outpoint:  utxo.Outpoint,
			AmountSat: int64(utxo.Amount),
		})
		resp.TotalInputSat += int64(utxo.Amount)
	}

	if len(resp.Inputs) == 0 {
		resp.FailureReason = "no confirmed backing-wallet UTXOs"

		return resp
	}

	fee := int64(estimateWalletSweepVSize(utxos, destScript)) * feeRate
	resp.EstimatedFeeSat = fee
	resp.NetAmountSat = resp.TotalInputSat - fee
	if resp.NetAmountSat <= int64(txconfirm.DustLimit) {
		resp.FailureReason = walletSweepDustFailureReason

		return resp
	}

	resp.CanBroadcast = true

	return resp
}

// estimateWalletSweepVSize estimates the virtual size of the aggregate sweep
// transaction by adding a witness-class-appropriate input weight for every
// UTXO and a single destination output.
func estimateWalletSweepVSize(utxos []*Utxo, destScript txscript.PkScript) int {
	var est input.TxWeightEstimator
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		addWalletSweepInputForScript(&est, utxo.PkScript)
	}
	est.AddOutput(destScript.Script())

	return est.VSize()
}

// addWalletSweepInputForScript adds the weight contribution of one sweep input
// to the estimator, dispatching on the input's script class. Unknown classes
// fall back to P2WKH (a conservative over-estimate for P2TR, safe for fee
// floor purposes), matching the broadcaster's child-vsize estimation.
func addWalletSweepInputForScript(est *input.TxWeightEstimator,
	pkScript []byte) {

	switch txscript.GetScriptClass(pkScript) {
	case txscript.WitnessV0PubKeyHashTy:
		est.AddP2WKHInput()

	case txscript.WitnessV1TaprootTy:
		est.AddTaprootKeySpendInput(txscript.SigHashDefault)

	case txscript.ScriptHashTy:
		est.AddNestedP2WKHInput()

	case txscript.PubKeyHashTy:
		est.AddP2PKHInput()

	default:
		est.AddP2WKHInput()
	}
}

// broadcastWalletSweep performs the broadcast leg of a general wallet sweep:
// it leases every input under walletSweepLockID, builds and signs the
// single-output sweep tx, and submits it to the shared txconfirm broadcaster.
// On any failure before a successful submit the leased inputs are released; on
// success the leases are retained so the broadcaster keeps exclusive use of the
// inputs.
func (a *Ark) broadcastWalletSweep(ctx context.Context, log btclog.Logger,
	utxos []*Utxo, destScript txscript.PkScript,
	resp *SweepWalletFundsResponse) fn.Result[WalletResp] {

	locked, err := a.lockWalletSweepInputs(ctx, utxos)
	if err != nil {
		return failedWalletSweepResponse(resp, err)
	}
	releaseInputs := true
	defer func() {
		if !releaseInputs {
			return
		}

		a.releaseWalletSweepInputs(ctx, log, locked)
	}()

	tx, err := buildWalletSweepTx(utxos, destScript, resp.NetAmountSat)
	if err != nil {
		return failedWalletSweepResponse(
			resp, fmt.Errorf("build sweep tx: %w", err),
		)
	}

	finalTx, err := a.signWalletSweepTx(ctx, tx, utxos)
	if err != nil {
		return failedWalletSweepResponse(
			resp, fmt.Errorf("sign sweep tx: %w", err),
		)
	}

	// Use the chain best height as the broadcaster's confirmation-watch
	// height hint. A failure to resolve it is transient; surface it so the
	// inputs are released and the caller can retry.
	heightHint, err := a.askBestHeight(ctx)
	if err != nil {
		return failedWalletSweepResponse(
			resp, fmt.Errorf("resolve best height: %w", err),
		)
	}

	err = a.submitWalletSweepConfirmer(
		ctx, finalTx, destScript.Script(), uint32(heightHint),
	)
	if err != nil {
		return failedWalletSweepResponse(
			resp, fmt.Errorf("submit sweep to broadcaster: %w",
				err),
		)
	}

	// The broadcaster now owns the inputs and the rebroadcast/CPFP
	// lifecycle, so keep the leases.
	releaseInputs = false

	txid := finalTx.TxHash()
	resp.Txid = txid
	resp.HasTxid = true

	log.InfoS(ctx, "Wallet backing sweep submitted to broadcaster",
		slog.String("txid", txid.String()),
		slog.Int("num_inputs", len(resp.Inputs)),
		slog.Int64("estimated_fee_sat", resp.EstimatedFeeSat),
		slog.Int64("net_amount_sat", resp.NetAmountSat),
		slog.Int64("fee_rate_sat_per_vbyte", resp.FeeRateSatPerVByte),
	)

	return fn.Ok[WalletResp](resp)
}

// lockWalletSweepInputs leases every non-nil sweep input under
// walletSweepLockID. If any lease fails, the leases acquired so far are
// released before returning the error so no UTXOs are stranded.
func (a *Ark) lockWalletSweepInputs(ctx context.Context, utxos []*Utxo) (
	[]wire.OutPoint, error) {

	locked := make([]wire.OutPoint, 0, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		_, err := a.walletSweeper.LeaseOutput(
			ctx, walletSweepLockID, utxo.Outpoint,
			txconfirm.DefaultFeeInputLeaseExpiry,
		)
		if err != nil {
			a.releaseWalletSweepInputs(
				ctx, a.logger(ctx), locked,
			)

			return nil, fmt.Errorf("lock wallet sweep input %s: %w",
				utxo.Outpoint, err)
		}

		locked = append(locked, utxo.Outpoint)
	}

	return locked, nil
}

// releaseWalletSweepInputs releases the leases on every supplied outpoint.
// Release errors are best-effort and logged rather than returned: the caller is
// already on a failure or cleanup path.
func (a *Ark) releaseWalletSweepInputs(ctx context.Context, log btclog.Logger,
	outpoints []wire.OutPoint) {

	for _, op := range outpoints {
		err := a.walletSweeper.ReleaseOutput(
			ctx, walletSweepLockID, op,
		)
		if err != nil {
			log.WarnS(ctx, "Failed to release wallet sweep input",
				err,
				slog.String("outpoint", op.String()),
			)
		}
	}
}

// buildWalletSweepTx assembles the unsigned single-output aggregate sweep
// transaction: every non-nil UTXO becomes an input and the net amount is paid
// to the destination script. It re-asserts the dust floor as a defensive guard
// in case a caller bypassed the preview gate.
func buildWalletSweepTx(utxos []*Utxo, destScript txscript.PkScript,
	netAmount int64) (*wire.MsgTx, error) {

	if netAmount <= int64(txconfirm.DustLimit) {
		return nil, fmt.Errorf("net amount %d is dust", netAmount)
	}

	tx := wire.NewMsgTx(walletSweepTxVersion)
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: utxo.Outpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    netAmount,
		PkScript: destScript.Script(),
	})

	return tx, nil
}

// signWalletSweepTx packages the unsigned sweep into a PSBT, attaches each
// input's witness UTXO, hands it to the backend for finalization, and asserts
// the signer did not alter the outputs before returning the finalized tx.
func (a *Ark) signWalletSweepTx(ctx context.Context, tx *wire.MsgTx,
	utxos []*Utxo) (*wire.MsgTx, error) {

	inputs := make([]*wire.OutPoint, 0, len(tx.TxIn))
	sequences := make([]uint32, 0, len(tx.TxIn))
	witnessByOutpoint := make(map[wire.OutPoint]*wire.TxOut, len(utxos))
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		witnessByOutpoint[utxo.Outpoint] = &wire.TxOut{
			Value:    int64(utxo.Amount),
			PkScript: utxo.PkScript,
		}
	}

	for _, txIn := range tx.TxIn {
		inputs = append(inputs, &txIn.PreviousOutPoint)
		sequences = append(sequences, txIn.Sequence)
	}

	packet, err := psbt.New(
		inputs, tx.TxOut, tx.Version, tx.LockTime, sequences,
	)
	if err != nil {
		return nil, fmt.Errorf("create PSBT: %w", err)
	}

	for idx, txIn := range tx.TxIn {
		witness, ok := witnessByOutpoint[txIn.PreviousOutPoint]
		if !ok {
			return nil, fmt.Errorf("missing witness UTXO for %s",
				txIn.PreviousOutPoint)
		}

		packet.Inputs[idx].WitnessUtxo = witness
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize PSBT: %w", err)
	}

	finalTx, err := a.walletSweeper.FinalizePsbt(ctx, buf.Bytes())
	if err != nil {
		return nil, err
	}

	if err := verifyWalletSweepOutputsEqual(tx, finalTx); err != nil {
		return nil, err
	}

	return finalTx, nil
}

// verifyWalletSweepOutputsEqual asserts the backend signer returned a tx whose
// outputs are byte-for-byte identical to the ones we built. A signer that
// silently changed an output value or script could redirect the sweep, so we
// fail closed rather than broadcasting an unexpected tx.
func verifyWalletSweepOutputsEqual(expected, actual *wire.MsgTx) error {
	if expected == nil || actual == nil {
		return fmt.Errorf("transactions must be non-nil")
	}
	if len(expected.TxOut) != len(actual.TxOut) {
		return fmt.Errorf("wallet changed sweep output count from "+
			"%d to %d", len(expected.TxOut), len(actual.TxOut))
	}

	for idx := range expected.TxOut {
		exp := expected.TxOut[idx]
		got := actual.TxOut[idx]
		if exp.Value != got.Value ||
			!bytes.Equal(exp.PkScript, got.PkScript) {
			return fmt.Errorf("wallet changed sweep output %d", idx)
		}
	}

	return nil
}

// submitWalletSweepConfirmer hands the signed general sweep transaction off to
// the shared txconfirm broadcaster for durable in-session rebroadcast and
// confirmation tracking via the common submitSweepToConfirm path. It routes
// terminal notifications into a WalletSweepTxNotification whose handler only
// logs — there is no boarding-store record to reconcile, by the lean decision
// that general wallet sweeps are not persisted.
func (a *Ark) submitWalletSweepConfirmer(ctx context.Context, tx *wire.MsgTx,
	pkScript []byte, heightHint uint32) error {

	walletNotif := actor.NewMapInputRef[
		WalletSweepTxNotification, WalletMsg,
	](
		a.selfRef,
		func(n WalletSweepTxNotification) WalletMsg {
			return n
		},
	)

	subscriber := txconfirm.FilterMapNotification(walletNotif,
		func(n txconfirm.Notification) (WalletSweepTxNotification,
			bool) {

			switch ev := n.(type) {
			case *txconfirm.TxConfirmed:
				return WalletSweepTxNotification{
					Confirmed:   true,
					Txid:        ev.Txid,
					BlockHeight: ev.BlockHeight,
					NumConfs:    ev.NumConfs,
				}, true

			// Finalization replays the confirmation numbers once
			// the tx is past the reorg-safety depth; this handler
			// is log-only so it reads as a (repeat) confirmation
			// rather than the failure the old zero-value fallback
			// produced.
			case *txconfirm.TxFinalized:
				return WalletSweepTxNotification{
					Confirmed:   true,
					Txid:        ev.Txid,
					BlockHeight: ev.BlockHeight,
					NumConfs:    ev.NumConfs,
				}, true

			case *txconfirm.TxFailed:
				return WalletSweepTxNotification{
					Confirmed: false,
					Txid:      ev.Txid,
					Reason:    ev.Reason,
				}, true
			}

			// TxReorged (best-effort, superseded by the next
			// reliable event) and unknown variants carry nothing
			// this log-only handler can report; drop them.
			return WalletSweepTxNotification{}, false
		},
	)

	return a.submitSweepToConfirm(
		ctx, tx, pkScript, heightHint, walletSweepBroadcastLabel,
		subscriber,
	)
}

// handleWalletSweepTxNotification processes a txconfirm terminal notification
// for a general backing-wallet sweep. General wallet sweeps are not persisted,
// so this handler only logs the outcome and returns the no-op ack: a confirm
// is observability, and a failure simply means the operator can re-run the
// manual sweep.
func (a *Ark) handleWalletSweepTxNotification(ctx context.Context,
	notif WalletSweepTxNotification) fn.Result[WalletResp] {

	log := a.logger(ctx)
	switch {
	case notif.Confirmed:
		log.DebugS(ctx, "Wallet backing sweep confirmed",
			nil,
			slog.String("txid", notif.Txid.String()),
			slog.Int("block_height", int(notif.BlockHeight)),
			slog.Uint64("num_confs", uint64(notif.NumConfs)),
		)

	default:
		log.WarnS(ctx, "Wallet backing sweep reported failure",
			errors.New(notif.Reason),
			slog.String("txid", notif.Txid.String()),
		)
	}

	return fn.Ok[WalletResp](&WalletSweepNotificationAck{})
}
