package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	claimStorePrefix  = "claim/"
	claimStateVersion = uint16(1)

	claimInputID  = "wavelength-claim-input"
	claimOutputID = "wavelength-claim-output"

	claimAttemptPublish = "publish"
)

// claimTapdVersion is the first tapd version that confirms a transfer
// spending an asset it does not hold. Development builds ahead of v0.9.0
// report 0.8.99.
var claimTapdVersion = [3]int{0, 8, 99}

// TapdCanClaim reports whether tapd of the given version credits a claimed
// asset. Older versions cannot confirm a transfer whose input they do not
// hold.
func TapdCanClaim(version string) (bool, error) {
	var parsed [3]int
	_, err := fmt.Sscanf(
		version, "%d.%d.%d", &parsed[0], &parsed[1], &parsed[2],
	)
	if err != nil {
		return false, fmt.Errorf("parse tapd version %q: %w", version,
			err)
	}
	for i := range parsed {
		if parsed[i] != claimTapdVersion[i] {
			return parsed[i] > claimTapdVersion[i], nil
		}
	}

	return true, nil
}

// ClaimRequest describes an asset claim: the owner of a confirmed composed
// output spends it through the exit leaf of its Ark policy, which moves the
// asset into the asset wallet as an ordinary holding.
type ClaimRequest struct {
	// RequestID keys the claim's journal. A retry under the same ID
	// resumes the stored claim.
	RequestID string

	// Outpoint and Output identify the composed output.
	Outpoint wire.OutPoint
	Output   *wire.TxOut

	// ExitSpend is the output's exit leaf. Its control block binds to the
	// composed output, and its required sequence is the exit delay.
	ExitSpend *arkscript.SpendPath

	// OwnerKey signs the exit leaf.
	OwnerKey keychain.KeyDescriptor

	// AssetRef and Amount describe the asset the output holds.
	AssetRef tapsdk.AssetRef
	Amount   uint64

	// ProofFile is the asset's confirmed proof file, ending in Outpoint.
	ProofFile []byte

	// OPTrueWitness authorizes the asset spend under its OP_TRUE script
	// key.
	OPTrueWitness [][]byte
}

// ClaimResult is the signed claim transaction. It pays no fee: its last
// output is a zero-value P2A anchor that a CPFP child spends.
type ClaimResult struct {
	Tx *wire.MsgTx

	// AssetOutpoint anchors the claimed asset in the asset wallet.
	AssetOutpoint wire.OutPoint
}

// ClaimerConfig holds the dependencies of a Claimer.
type ClaimerConfig struct {
	// Wallet is the asset wallet that receives the claimed asset.
	Wallet *tapsdk.Wallet

	// Store journals each claim.
	Store Store

	// Signer signs the exit leaf with the output owner's key.
	Signer ClaimSigner
}

// ClaimSigner signs a Taproot script-path input.
type ClaimSigner interface {
	SignOutputRaw(*wire.MsgTx,
		*input.SignDescriptor) (input.Signature, error)
}

// claimDriver commits and publishes one claim through tap-sdk.
type claimDriver interface {
	Commit(context.Context, *customanchor.Request,
		customanchor.ConfirmedProofVerifier) (*commitResult, error)

	DecodePackage([]byte) (*commitResult, error)

	Publish(context.Context, []byte, []byte) error

	ReconcileOnboarding(context.Context, []byte) error
}

// Claimer builds, signs, and records asset claims.
type Claimer struct {
	driver claimDriver
	keys   onboardingKeyClient
	store  Store
	signer ClaimSigner

	// mu serializes claims, since each journal entry has one writer.
	mu sync.Mutex
}

// claimState is the journal of one claim. The wallet keys are pinned before
// the commit, so every retry seals the same transaction.
type claimState struct {
	Version           uint16             `json:"version"`
	RequestDigest     tapsdk.Hash        `json:"request_digest"`
	Attempt           string             `json:"attempt,omitempty"`
	ScriptKey         tapsdk.ScriptKey   `json:"script_key"`
	AnchorInternalKey tapsdk.InternalKey `json:"anchor_internal_key"`

	TransferPackage []byte `json:"transfer_package,omitempty"`
	FinalAnchorPSBT []byte `json:"final_anchor_psbt,omitempty"`
	Published       bool   `json:"published"`
}

// NewClaimer constructs the tap-sdk-backed claim workflow.
func NewClaimer(cfg ClaimerConfig) (*Claimer, error) {
	switch {
	case cfg.Wallet == nil:
		return nil, fmt.Errorf("tap-sdk wallet is required")

	case cfg.Store == nil:
		return nil, fmt.Errorf("taproot asset claim store is required")

	case cfg.Signer == nil:
		return nil, fmt.Errorf("taproot asset claim signer is required")
	}

	return &Claimer{
		driver: &sdkDriver{
			wallet: cfg.Wallet,
		},
		keys:   cfg.Wallet.Client(),
		store:  cfg.Store,
		signer: cfg.Signer,
	}, nil
}

// Prepare commits and signs one claim, or returns the journaled claim,
// without recording it in tapd. Once the commit succeeds, every retry
// returns the same transaction.
func (c *Claimer) Prepare(ctx context.Context, request *ClaimRequest) (
	*ClaimResult, error) {

	if err := validateClaimRequest(request); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	state, err := c.prepare(ctx, request)
	if err != nil {
		return nil, err
	}

	return claimResult(state)
}

// Claim prepares one claim and records it in tapd, or resumes the journaled
// claim. A publish whose outcome is unknown is reconciled before the claim is
// published again.
func (c *Claimer) Claim(ctx context.Context, request *ClaimRequest) (
	*ClaimResult, error) {

	if err := validateClaimRequest(request); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	state, err := c.prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := c.publish(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	return claimResult(state)
}

// prepare loads the claim journal, then commits and signs whatever the
// journal lacks.
func (c *Claimer) prepare(ctx context.Context, request *ClaimRequest) (
	*claimState, error) {

	state, err := c.loadState(ctx, request)
	if err != nil {
		return nil, err
	}

	committed, err := c.commit(ctx, request, state)
	if err != nil {
		return nil, err
	}
	if len(state.FinalAnchorPSBT) != 0 {
		return state, nil
	}

	state.FinalAnchorPSBT, err = c.sign(request, committed.anchorPSBT)
	if err != nil {
		return nil, fmt.Errorf("sign claim: %w", err)
	}
	if err := c.storeResult(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	return state, nil
}

// claimResult extracts the signed claim transaction from its journal.
func claimResult(state *claimState) (*ClaimResult, error) {
	packet, err := psbtutil.Parse(state.FinalAnchorPSBT)
	if err != nil {
		return nil, err
	}
	tx, err := psbt.Extract(packet)
	if err != nil {
		return nil, fmt.Errorf("extract claim transaction: %w", err)
	}

	return &ClaimResult{
		Tx: tx,
		AssetOutpoint: wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: 0,
		},
	}, nil
}

// commit resumes the sealed package or commits the claim in tapd.
func (c *Claimer) commit(ctx context.Context, request *ClaimRequest,
	state *claimState) (*commitResult, error) {

	if len(state.TransferPackage) != 0 {
		committed, err := c.driver.DecodePackage(state.TransferPackage)
		if err != nil {
			return nil, fmt.Errorf("restore claim package: %w", err)
		}

		return committed, nil
	}

	template, err := claimTemplate(request)
	if err != nil {
		return nil, err
	}
	anchorPSBT, err := psbtutil.Serialize(template)
	if err != nil {
		return nil, err
	}

	// The claimed asset goes to keys tapd derived, so tapd holds it as
	// its own once the claim confirms.
	requestDTO := &customanchor.Request{
		Inputs: []customanchor.AssetInput{{
			ID:        claimInputID,
			AssetRef:  request.AssetRef,
			Amount:    request.Amount,
			ProofFile: bytes.Clone(request.ProofFile),
			Witness: customanchor.AssetWitnessPlan{
				Mode: customanchor.AssetWitnessCallerProvided,
				Stack: cloneByteSlices(
					request.OPTrueWitness,
				),
			},
		}},
		Outputs: []customanchor.AssetOutput{{
			ID:                claimOutputID,
			AssetRef:          request.AssetRef,
			Amount:            request.Amount,
			AnchorOutputIndex: 0,
			AnchorValueSat:    uint64(request.Output.Value),
			Script: customanchor.AssetScriptPlan{
				Mode: scriptExternal,
				External: &customanchor.
					AssetExternalScriptPlan{
					ScriptKey: state.ScriptKey,
				},
			},
			Anchor: customanchor.OutputPlan{
				InternalKey: state.AnchorInternalKey,
			},
		}},
		AnchorPSBT: anchorPSBT,
		Funding:    callerFundedExact(),
		PassiveAssets: customanchor.PassiveAssets{
			Policy: customanchor.PassiveReject,
		},
		LossPolicy: customanchor.LossPolicy{
			Mode: customanchor.LossReject,
		},
		SigningPlans: []customanchor.InputSigningPlan{{
			InputIndex:   0,
			CallerSigned: &customanchor.CallerSignedPlan{},
		}},
	}

	committed, err := c.driver.Commit(ctx, requestDTO, nil)
	if err != nil {
		return nil, fmt.Errorf("commit claim %q: %w", request.RequestID,
			err)
	}
	if err := validateClaimCommit(committed); err != nil {
		return nil, err
	}

	state.TransferPackage = bytes.Clone(committed.packageBytes)
	if err := c.storeResult(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	return committed, nil
}

// claimTemplate is the unsigned claim: the composed output spent through its
// exit leaf, the claimed asset's anchor at the full output value, and a P2A
// anchor. It pays no fee, so it relays only with a CPFP child.
func claimTemplate(request *ClaimRequest) (*psbt.Packet, error) {
	tx := wire.NewMsgTx(arktx.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: request.Outpoint,
		Sequence:         request.ExitSpend.RequiredSequence,
	})
	tx.AddTxOut(wire.NewTxOut(request.Output.Value, nil))
	tx.AddTxOut(arkscript.AnchorOutput())

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, err
	}
	packet.Inputs[0].WitnessUtxo = cloneTxOut(request.Output)
	if err := request.ExitSpend.AttachTapLeafScript(
		&packet.Inputs[0],
	); err != nil {
		return nil, fmt.Errorf("attach exit leaf: %w", err)
	}

	return packet, nil
}

// validateClaimCommit checks that tapd sealed a zero-fee claim with exactly
// the requested asset output.
func validateClaimCommit(committed *commitResult) error {
	switch {
	case committed == nil:
		return fmt.Errorf("claim commit result is required")

	case committed.fundingMode != customanchor.FundingCallerFundedExact:
		return fmt.Errorf("unexpected claim funding mode %d",
			committed.fundingMode)

	case committed.actualFeeSat != 0:
		return fmt.Errorf("claim must pay no fee, got %d",
			committed.actualFeeSat)

	case len(committed.outputs) != 1:
		return fmt.Errorf("claim must create one asset output, got %d",
			len(committed.outputs))

	case committed.outputs[0].anchorOutputIndex != 0:
		return fmt.Errorf("claimed asset anchors at output %d",
			committed.outputs[0].anchorOutputIndex)
	}

	return nil
}

// sign signs the exit leaf of the committed claim and finalizes it.
func (c *Claimer) sign(request *ClaimRequest, anchorPSBT []byte) ([]byte,
	error) {

	packet, err := psbtutil.Parse(anchorPSBT)
	if err != nil {
		return nil, err
	}
	if len(packet.UnsignedTx.TxIn) != 1 ||
		packet.UnsignedTx.TxIn[0].PreviousOutPoint != request.Outpoint {
		return nil, fmt.Errorf("committed claim does not spend %v",
			request.Outpoint)
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		request.Output.PkScript, request.Output.Value,
	)
	sigHashes := txscript.NewTxSigHashes(packet.UnsignedTx, prevFetcher)
	signDesc := request.ExitSpend.BuildSignDescriptor(
		request.OwnerKey, request.Output, sigHashes, prevFetcher, 0,
	)
	sig, err := c.signer.SignOutputRaw(packet.UnsignedTx, signDesc)
	if err != nil {
		return nil, err
	}
	witness, err := request.ExitSpend.SingleSigWitness(
		sig, signDesc.HashType,
	)
	if err != nil {
		return nil, err
	}

	var encoded bytes.Buffer
	if err := psbt.WriteTxWitness(&encoded, witness); err != nil {
		return nil, err
	}
	packet.Inputs[0].FinalScriptWitness = encoded.Bytes()
	if _, err := psbt.Extract(packet); err != nil {
		return nil, fmt.Errorf("finalize claim: %w", err)
	}

	return psbtutil.Serialize(packet)
}

// publish records the signed claim in tapd. The attempt is journaled first,
// and a retry after an attempt publishes again only once tapd reports no
// record of the claim. tapd keys a transfer by its anchor transaction, so a
// second publish of the same claim attaches to the first rather than logging
// it twice.
func (c *Claimer) publish(ctx context.Context, requestID string,
	state *claimState) error {

	if state.Published {
		return nil
	}

	if state.Attempt == claimAttemptPublish {
		err := c.driver.ReconcileOnboarding(ctx, state.FinalAnchorPSBT)
		if err == nil {
			return c.markPublished(ctx, requestID, state)
		}
		if !errors.Is(err, errTransferNotRecorded) {
			return err
		}
	}

	state.Attempt = claimAttemptPublish
	if err := c.storeState(ctx, requestID, state); err != nil {
		return err
	}

	err := c.driver.Publish(
		ctx, state.TransferPackage, state.FinalAnchorPSBT,
	)
	if err != nil {
		var attempted *customanchor.PublishAttemptError
		if errors.As(err, &attempted) && attempted.OutcomeUnknown {
			return errors.Join(ErrReconciliationRequired, err)
		}

		state.Attempt = ""

		return errors.Join(err, c.storeResult(ctx, requestID, state))
	}

	return c.markPublished(ctx, requestID, state)
}

// markPublished journals that tapd recorded the claim.
func (c *Claimer) markPublished(ctx context.Context, requestID string,
	state *claimState) error {

	state.Attempt = ""
	state.Published = true

	return c.storeResult(ctx, requestID, state)
}

// loadState reads the claim journal, or starts one with fresh wallet keys.
func (c *Claimer) loadState(ctx context.Context, request *ClaimRequest) (
	*claimState, error) {

	digest := claimRequestDigest(request)
	encoded, err := c.store.Load(ctx, claimStorePrefix+request.RequestID)
	if err == nil {
		var state claimState
		if err := json.Unmarshal(encoded, &state); err != nil {
			return nil, fmt.Errorf("decode taproot asset claim "+
				"state: %w", err)
		}
		if state.Version != claimStateVersion {
			return nil, fmt.Errorf("unsupported taproot asset "+
				"claim state version %d", state.Version)
		}
		if state.RequestDigest != digest {
			return nil, fmt.Errorf("claim %q was journaled for a "+
				"different request", request.RequestID)
		}

		return &state, nil
	}
	if !errors.Is(err, ErrStoreNotFound) {
		return nil, err
	}

	scriptKey, err := c.keys.DeriveScriptKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive claim script key: %w", err)
	}
	internalKey, err := c.keys.DeriveInternalKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive claim anchor internal key: %w",
			err)
	}
	if scriptKey == nil || internalKey == nil {
		return nil, fmt.Errorf("tapd returned an empty claim key")
	}

	state := &claimState{
		Version:           claimStateVersion,
		RequestDigest:     digest,
		ScriptKey:         *scriptKey,
		AnchorInternalKey: *internalKey,
	}
	if err := c.storeState(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	return state, nil
}

// storeResult journals state even if the caller was cancelled, so what tapd
// already sealed or recorded is never forgotten.
func (c *Claimer) storeResult(ctx context.Context, requestID string,
	state *claimState) error {

	journalCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), commitJournalWriteTimeout,
	)
	defer cancel()

	return c.storeState(journalCtx, requestID, state)
}

// storeState atomically replaces the claim journal.
func (c *Claimer) storeState(ctx context.Context, requestID string,
	state *claimState) error {

	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode taproot asset claim state: %w", err)
	}

	return c.store.Store(ctx, claimStorePrefix+requestID, encoded)
}

// validateClaimRequest checks a claim before any wallet key is derived.
func validateClaimRequest(request *ClaimRequest) error {
	switch {
	case request == nil:
		return fmt.Errorf("taproot asset claim request is required")

	case request.RequestID == "":
		return fmt.Errorf("taproot asset claim request ID is required")

	case request.Output == nil || len(request.Output.PkScript) == 0:
		return fmt.Errorf("claimed output is required")

	case request.Output.Value < int64(onboardingDustFloorSat):
		return fmt.Errorf("claimed output value %d is below the "+
			"Taproot dust floor", request.Output.Value)

	case request.ExitSpend == nil || request.ExitSpend.SpendInfo == nil:
		return fmt.Errorf("claim exit spend is required")

	case request.OwnerKey.PubKey == nil:
		return fmt.Errorf("claim owner key is required")

	case request.AssetRef == "" || request.Amount == 0:
		return fmt.Errorf("claimed asset ref and amount are required")

	case len(request.ProofFile) == 0:
		return fmt.Errorf("claimed asset proof file is required")

	case len(request.OPTrueWitness) == 0:
		return fmt.Errorf("claimed asset witness is required")
	}

	if err := request.ExitSpend.Validate(); err != nil {
		return fmt.Errorf("claim exit spend: %w", err)
	}

	return request.ExitSpend.VerifyBindsToPkScript(request.Output.PkScript)
}

// claimRequestDigest binds a journal entry to one claim request.
func claimRequestDigest(request *ClaimRequest) tapsdk.Hash {
	h := sha256.New()
	h.Write([]byte("wavelength/asset-claim-request/v0"))

	writeBytes := func(data []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		h.Write(size[:])
		h.Write(data)
	}
	writeUint := func(value uint64) {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], value)
		h.Write(encoded[:])
	}

	h.Write(request.Outpoint.Hash[:])
	writeUint(uint64(request.Outpoint.Index))
	writeUint(uint64(request.Output.Value))
	writeBytes(request.Output.PkScript)
	writeBytes(request.ExitSpend.WitnessScript)
	writeBytes(request.ExitSpend.ControlBlock)
	writeUint(uint64(request.ExitSpend.RequiredSequence))
	writeBytes(request.OwnerKey.PubKey.SerializeCompressed())
	writeBytes([]byte(request.AssetRef))
	writeUint(request.Amount)
	writeBytes(request.ProofFile)
	writeUint(uint64(len(request.OPTrueWitness)))
	for _, item := range request.OPTrueWitness {
		writeBytes(item)
	}

	var digest tapsdk.Hash
	copy(digest[:], h.Sum(nil))

	return digest
}

// cloneTxOut returns a deep copy of an output.
func cloneTxOut(out *wire.TxOut) *wire.TxOut {
	return wire.NewTxOut(out.Value, bytes.Clone(out.PkScript))
}
