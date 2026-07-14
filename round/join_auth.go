package round

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/bip322"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// joinRoundAuthWindowBlocks is the default validity window length
	// applied to join authorization signatures.
	joinRoundAuthWindowBlocks uint32 = 144

	// joinRoundAuthIdentifierKeyFamily is the BIP32 key family used
	// for per-request join authorization identifier keys.
	joinRoundAuthIdentifierKeyFamily keychain.KeyFamily = 43
)

// joinAuthInput carries all data required to produce one script-path
// witness for a BIP-322 proof-of-funds input.
type joinAuthInput struct {
	// OutPoint is the outpoint proven by this input.
	OutPoint wire.OutPoint

	// PrevOut is the referenced tx output data for this input.
	PrevOut *wire.TxOut

	// KeyDesc identifies the key used for timeout-path signing.
	KeyDesc keychain.KeyDescriptor

	// OperatorKey is the operator public key used in the VTXO
	// collaborative and timeout spend paths.
	OperatorKey *btcec.PublicKey

	// TapScript contains the two-leaf VTXO script tree used to
	// derive the unilateral timeout spend witness.
	TapScript *waddrmgr.Tapscript

	// Sequence sets nSequence for this BIP-322 input and must
	// satisfy the timeout leaf's CSV condition.
	Sequence uint32

	// LockTime is the to_sign nLockTime required by this proof input.
	LockTime uint32

	// AuthSpend overrides the default standard-VTXO timeout proof path.
	AuthSpend *arkscript.SpendPath
}

// deriveJoinAuthIdentifierKey derives a fresh key descriptor used as the
// join-request BIP-322 identifier and challenge key.
func deriveJoinAuthIdentifierKey(ctx context.Context,
	wallet ClientWallet) (keychain.KeyDescriptor, error) {

	if wallet == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("wallet signer " +
			"must be provided")
	}

	keyDesc, err := wallet.DeriveNextKey(
		ctx, joinRoundAuthIdentifierKeyFamily,
	)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("derive join auth "+
			"identifier key: %w", err)
	}

	if keyDesc == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("derive join " +
			"auth identifier key returned nil descriptor")
	}

	if keyDesc.PubKey == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("derive join " +
			"auth identifier key returned nil pubkey")
	}

	return *keyDesc, nil
}

// sortedForfeitRequests sorts forfeit requests by outpoint (txid bytes
// then output index) so the resulting list is deterministic. Returns an
// error if any request has a nil VTXOOutpoint.
func sortedForfeitRequests(forfeits []types.ForfeitRequest) (
	[]*types.ForfeitRequest, error) {

	// Index the full local request by outpoint so we can carry
	// both wire-visible and local-only custom-spend metadata
	// through the sort.
	requestByOP := make(
		map[wire.OutPoint]types.ForfeitRequest, len(forfeits),
	)

	// Collect and sort the outpoints, validating that none are nil.
	outpoints := make([]wire.OutPoint, 0, len(forfeits))
	for i := 0; i < len(forfeits); i++ {
		if forfeits[i].VTXOOutpoint == nil {
			return nil, fmt.Errorf("forfeit request %d has nil "+
				"outpoint", i)
		}

		op := *forfeits[i].VTXOOutpoint
		outpoints = append(outpoints, op)
		requestByOP[op] = forfeits[i]
	}
	sortOutPoints(outpoints)

	// Build the sorted result, preserving both the wire-visible
	// outpoint and any local-only custom spend metadata.
	requests := make(
		[]*types.ForfeitRequest, 0, len(outpoints),
	)
	for i := 0; i < len(outpoints); i++ {
		op := outpoints[i]
		req := requestByOP[op]
		req.VTXOOutpoint = &op
		requests = append(
			requests, &req,
		)
	}

	return requests, nil
}

// sortOutPoints orders outpoints by txid bytes then output index.
func sortOutPoints(outpoints []wire.OutPoint) {
	sort.Slice(outpoints, func(i, j int) bool {
		left := outpoints[i]
		right := outpoints[j]

		hashCmp := bytes.Compare(left.Hash[:], right.Hash[:])
		if hashCmp != 0 {
			return hashCmp < 0
		}

		return left.Index < right.Index
	})
}

// computeTotalForfeitAmount looks up each forfeited VTXO's value from
// the VTXOStore and returns the sum. The VTXOStore is always used as
// the canonical source of truth to prevent callers from inflating the
// forfeit total via the embedded Amount field. If the store is nil (test
// harness), the embedded Amount is used as a fallback.
func computeTotalForfeitAmount(ctx context.Context, store VTXOStore,
	forfeits []types.ForfeitRequest) (btcutil.Amount, error) {

	var total btcutil.Amount
	for i := 0; i < len(forfeits); i++ {
		// When a store is available, always use it as the source
		// of truth for the VTXO amount.
		if store != nil {
			vtxo, err := store.GetVTXO(
				ctx, *forfeits[i].VTXOOutpoint,
			)
			if err != nil {
				return 0, fmt.Errorf("forfeit amount lookup "+
					"%s: %w", forfeits[i].VTXOOutpoint, err)
			}
			total += vtxo.Amount

			continue
		}

		// Fallback to the embedded amount when no store is
		// available (e.g., test environments).
		if forfeits[i].Amount != 0 {
			total += forfeits[i].Amount

			continue
		}

		return 0, fmt.Errorf("no store and no embedded amount for %s",
			forfeits[i].VTXOOutpoint)
	}

	return total, nil
}

// buildJoinRoundAuth creates the BIP-322 authorization payload for a
// JoinRoundRequest. The identifier key must already be derived; this
// function builds the canonical message, constructs proof-of-funds
// inputs, and signs everything.
func buildJoinRoundAuth(ctx context.Context, env *ClientEnvironment,
	identifierKeyDesc keychain.KeyDescriptor, intents Intents,
	vtxoReqs []types.VTXORequest, forfeitReqs []*types.ForfeitRequest,
	leaveReqs []*types.LeaveRequest) (*types.JoinRoundAuth, error) {

	log := env.Log
	log.InfoS(ctx, "Building join round auth",
		slog.Int("boarding_intent_count", len(intents.Boarding)),
		slog.Int("vtxo_request_count", len(vtxoReqs)),
		slog.Int("forfeit_request_count", len(forfeitReqs)),
		slog.Int("leave_request_count", len(leaveReqs)),
	)

	// Step 1: Build the canonical request and collect signing
	// inputs. The request mirrors types.JoinRoundRequest and will
	// be TLV-encoded into the message bytes that get signed. The
	// signing inputs carry the per-UTXO data (prevout, key,
	// tapscript) needed to produce proof-of-funds witnesses.
	joinReq, signingInputs, err := buildJoinRoundAuthRequest(
		ctx, env, intents, vtxoReqs, forfeitReqs, leaveReqs,
	)
	if err != nil {
		return nil, err
	}

	// Verify we have at least one provable input (boarding or
	// forfeit) and that all signing keys are populated.
	err = validateJoinAuthSigningInputs(signingInputs)
	if err != nil {
		return nil, err
	}

	log.DebugS(ctx, "Prepared join auth signing inputs",
		slog.Int("proof_input_count", len(signingInputs)),
	)

	// Step 2: Produce the deterministic message bytes. The
	// identifier is set on the request first so it appears in the
	// TLV encoding — this binds the proof to this specific key.
	joinReq.Identifier = identifierKeyDesc.PubKey

	message, err := types.JoinRoundAuthMessage(joinReq)
	if err != nil {
		return nil, fmt.Errorf("join auth message: %w", err)
	}

	// Step 3: Derive the BIP-322 challenge script. This is a
	// BIP-86 key-path taproot output derived from the identifier
	// key. The signer proves ownership by spending this output in
	// input 0 of the virtual to_sign transaction.
	messageChallenge, err := bip322.JoinRoundMessageChallenge(
		joinReq.Identifier,
	)
	if err != nil {
		return nil, fmt.Errorf("join auth message challenge: %w", err)
	}

	// Step 4: Map each signing input to a BIP-322 additional
	// input (proof-of-funds). These become inputs 1..N in the
	// to_sign transaction. Each proves ownership of a boarding
	// UTXO or forfeited VTXO via its unilateral timeout path.
	additionalInputs := buildJoinAuthAdditionalInputs(
		signingInputs,
	)

	lockTime := joinAuthLockTime(signingInputs)

	// Query the chain tip at signing time and use that height as the
	// lower bound for this auth intent.
	validFrom, err := joinAuthValidFrom(ctx, env)
	if err != nil {
		return nil, err
	}

	// Compute the upper bound so the server can reject stale
	// proofs.
	validUntil := joinAuthValidUntil(validFrom)

	intent, err := bip322.NewIntent(
		message, validFrom, validUntil,
	)
	if err != nil {
		return nil, fmt.Errorf("join auth intent: %w", err)
	}

	intentMessage, err := intent.SigningMessage()
	if err != nil {
		return nil, fmt.Errorf("join auth intent: %w", err)
	}

	// Step 5: Build and sign the full BIP-322 proof. This
	// constructs the virtual to_spend and to_sign transactions,
	// then calls our joinRoundBIP322Signer to populate all
	// witnesses: a key-path spend for input 0 (identifier) and
	// script-path timeout spends for each proof-of-funds input.
	sig, err := bip322.BuildAndSignFullTx(
		intentMessage,
		messageChallenge,
		&joinRoundBIP322Signer{
			wallet:            env.Wallet,
			identifierKeyDesc: identifierKeyDesc,
			signingInputs:     signingInputs,
			log:               log,
			ctx:               ctx,
		},
		bip322.WithToSignVersion(2),
		bip322.WithToSignAdditionalInputs(
			additionalInputs...,
		),
		bip322.WithToSignLockTime(lockTime),
	)
	if err != nil {
		return nil, fmt.Errorf("join auth build/sign: %w", err)
	}

	log.DebugS(ctx, "Built join auth signature transaction",
		slog.Int("proof_input_count", len(signingInputs)),
		slog.Int("message_len", len(message)),
		slog.Int("valid_from_block", int(validFrom)),
		slog.Int("valid_until_block", int(validUntil)),
	)

	// Step 6: Serialize the signed to_sign transaction into the
	// wire format that the server will decode and verify.
	rawSig, err := sig.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode join auth signature: %w", err)
	}

	log.InfoS(ctx, "Built join round auth",
		slog.Int("proof_input_count", len(signingInputs)),
		slog.Int("message_len", len(message)),
		slog.Int("signature_len", len(rawSig)),
		slog.Int("valid_from_block", int(validFrom)),
		slog.Int("valid_until_block", int(validUntil)),
	)

	return &types.JoinRoundAuth{
		Message:    message,
		ValidFrom:  validFrom,
		ValidUntil: validUntil,
		Signature:  rawSig,
	}, nil
}

// buildJoinRoundAuthRequest builds the shared request shape used for
// canonical message encoding and returns the corresponding signing
// inputs in the same order.
func buildJoinRoundAuthRequest(ctx context.Context, env *ClientEnvironment,
	intents Intents, vtxoReqs []types.VTXORequest,
	forfeitReqs []*types.ForfeitRequest, leaveReqs []*types.LeaveRequest) (
	*types.JoinRoundRequest, []joinAuthInput, error) {

	boardingReqs := make(
		[]*types.BoardingRequest, 0, len(intents.Boarding),
	)
	signingInputs := make(
		[]joinAuthInput, 0, len(intents.Boarding)+len(forfeitReqs),
	)

	// Each boarding intent contributes a boarding request and a
	// corresponding signing input for the proof-of-funds witness.
	for i := 0; i < len(intents.Boarding); i++ {
		intent := intents.Boarding[i]
		boardingReqCopy := intent.Request
		boardingReqs = append(boardingReqs, &boardingReqCopy)

		pkScript, err := txscript.PayToAddrScript(
			intent.Address.Address,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("boarding auth script: %w",
				err)
		}

		signingInputs = append(signingInputs, joinAuthInput{
			OutPoint: intent.Outpoint,
			PrevOut: &wire.TxOut{
				Value:    int64(intent.ChainInfo.Amount),
				PkScript: pkScript,
			},
			KeyDesc:     intent.Address.KeyDesc,
			OperatorKey: intent.Address.OperatorKey,
			TapScript:   intent.Address.Tapscript,
			Sequence:    intent.Address.ExitDelay,
		})
	}

	// Each forfeit request contributes a signing input built from
	// the persisted VTXO data plus any local custom auth path.
	for i := 0; i < len(forfeitReqs); i++ {
		forfeitReq := forfeitReqs[i]
		outpoint := *forfeitReq.VTXOOutpoint

		vtxo, err := env.VTXOStore.GetVTXO(ctx, outpoint)
		if err != nil {
			return nil, nil, fmt.Errorf("forfeit auth input %s: %w",
				outpoint, err)
		}

		if vtxo == nil {
			return nil, nil, fmt.Errorf("forfeit auth input %s "+
				"not found", outpoint)
		}

		if vtxo.OwnerKey.PubKey == nil {
			return nil, nil, fmt.Errorf("forfeit auth input %s "+
				"missing client key", outpoint)
		}

		if vtxo.OperatorKey == nil {
			return nil, nil, fmt.Errorf("forfeit auth input %s "+
				"missing operator key", outpoint)
		}

		signingInputs = append(signingInputs, joinAuthInput{
			OutPoint: outpoint,
			PrevOut: &wire.TxOut{
				Value:    int64(vtxo.Amount),
				PkScript: bytes.Clone(vtxo.PkScript),
			},
			KeyDesc:     vtxo.OwnerKey,
			OperatorKey: vtxo.OperatorKey,
			Sequence: forfeitAuthSequence(
				vtxo.Expiry, forfeitReq,
			),
			LockTime:  forfeitAuthLockTime(forfeitReq),
			AuthSpend: forfeitReq.AuthSpend,
		})
	}

	// Convert VTXO requests to pointer slice for the shared type.
	sharedVTXOReqs := make(
		[]*types.VTXORequest, 0, len(vtxoReqs),
	)
	for i := 0; i < len(vtxoReqs); i++ {
		reqCopy := vtxoReqs[i]
		sharedVTXOReqs = append(sharedVTXOReqs, &reqCopy)
	}

	return &types.JoinRoundRequest{
		Identifier:   nil,
		BoardingReqs: boardingReqs,
		VTXOReqs:     sharedVTXOReqs,
		ForfeitReqs:  forfeitReqs,
		LeaveReqs:    leaveReqs,
	}, signingInputs, nil
}

// validateJoinAuthSigningInputs checks that join-auth proof-of-funds
// inputs are present and structurally complete.
func validateJoinAuthSigningInputs(signingInputs []joinAuthInput) error {
	if len(signingInputs) == 0 {
		return fmt.Errorf("join auth requires at least one " +
			"proof-of-funds input")
	}

	for i := 0; i < len(signingInputs); i++ {
		if signingInputs[i].KeyDesc.PubKey == nil {
			return fmt.Errorf("join auth proof input %d key "+
				"is missing", i+1)
		}
	}

	return nil
}

// buildJoinAuthAdditionalInputs maps join-auth signing inputs onto
// BIP-322 proof-of-funds additional inputs.
func buildJoinAuthAdditionalInputs(
	signingInputs []joinAuthInput) []bip322.AdditionalInput {

	additionalInputs := make(
		[]bip322.AdditionalInput, 0, len(signingInputs),
	)
	for i := 0; i < len(signingInputs); i++ {
		si := signingInputs[i]
		additionalInputs = append(
			additionalInputs, bip322.AdditionalInput{
				PreviousOutPoint: si.OutPoint,
				Sequence:         si.Sequence,
				WitnessUtxo: &wire.TxOut{
					Value: si.PrevOut.Value,
					PkScript: bytes.Clone(
						si.PrevOut.PkScript,
					),
				},
			},
		)
	}

	return additionalInputs
}

// joinAuthLockTime returns the highest required proof locktime across the join
// auth inputs.
func joinAuthLockTime(signingInputs []joinAuthInput) uint32 {
	var lockTime uint32
	for i := 0; i < len(signingInputs); i++ {
		if signingInputs[i].LockTime > lockTime {
			lockTime = signingInputs[i].LockTime
		}
	}

	return lockTime
}

// forfeitAuthSequence returns the BIP-322 proof sequence for a forfeit input.
func forfeitAuthSequence(defaultSequence uint32,
	req *types.ForfeitRequest) uint32 {

	hasAuthSpend := req != nil &&
		req.AuthSpend != nil &&
		req.AuthSpend.SpendInfo != nil
	if hasAuthSpend {
		return req.AuthSpend.RequiredSequence
	}

	return defaultSequence
}

// forfeitAuthLockTime returns the BIP-322 proof locktime for a forfeit input.
func forfeitAuthLockTime(req *types.ForfeitRequest) uint32 {
	missingAuthSpend := req == nil ||
		req.AuthSpend == nil ||
		req.AuthSpend.SpendInfo == nil
	if missingAuthSpend {
		return 0
	}

	return req.AuthSpend.RequiredLockTime
}

// joinRoundBIP322Signer signs to_sign input 0 with the request
// identifier key and signs all additional proof-of-funds inputs via
// unilateral script-path witnesses.
//
// TxSigner has no context parameter; this field is cached only for logging.
//
//nolint:containedctx
type joinRoundBIP322Signer struct {
	// wallet performs transaction-level signing operations.
	wallet ClientWallet

	// identifierKeyDesc is the key descriptor for to_sign input 0.
	identifierKeyDesc keychain.KeyDescriptor

	// signingInputs is the ordered set of proof-of-funds inputs.
	signingInputs []joinAuthInput

	// log is the logger used to trace per-input signing.
	log btclog.Logger

	// ctx is the logging context for SignBIP322 calls.
	ctx context.Context
}

var _ bip322.TxSigner = (*joinRoundBIP322Signer)(nil)

// SignBIP322 implements bip322.TxSigner for join-round authorization.
func (s *joinRoundBIP322Signer) SignBIP322(toSpend *wire.MsgTx,
	toSign *wire.MsgTx, prevFetcher txscript.PrevOutputFetcher,
	sigHashes *txscript.TxSigHashes) error {

	logCtx := s.ctx
	if logCtx == nil {
		logCtx = context.Background()
	}

	log := s.log

	if s.wallet == nil {
		return fmt.Errorf("join auth wallet signer must be provided")
	}

	if len(toSign.TxIn) != len(s.signingInputs)+1 {
		return fmt.Errorf("join auth to_sign input count %d does not "+
			"match expected %d", len(toSign.TxIn),
			len(s.signingInputs)+1)
	}

	log.DebugS(logCtx, "Signing join auth proof",
		slog.Int("to_sign_input_count", len(toSign.TxIn)),
		slog.Int("proof_input_count", len(s.signingInputs)),
	)

	// Sign input 0 which spends the challenge output and binds
	// the signature to the identifier key.
	err := signJoinAuthMessageInput(
		s.wallet, toSign, toSpend, s.identifierKeyDesc, sigHashes,
		prevFetcher,
	)
	if err != nil {
		return err
	}

	// Sign each proof-of-funds input via the unilateral timeout
	// script path.
	for i := 0; i < len(s.signingInputs); i++ {
		si := s.signingInputs[i]
		inputIndex := i + 1

		log.DebugS(logCtx, "Signing join auth proof input",
			slog.Int("input_index", inputIndex),
			btclog.Fmt("outpoint", "%v", si.OutPoint),
			slog.Int("sequence", int(si.Sequence)),
			slog.Int("locktime", int(si.LockTime)),
		)

		spendPath := si.AuthSpend
		if spendPath == nil {
			spendInfo, err := arkscript.NewVTXOSpendInfoFromPolicy(
				si.KeyDesc.PubKey, si.OperatorKey, si.Sequence,
				1,
			)
			if err != nil {
				return fmt.Errorf("join auth spend info input "+
					"%d: %w", inputIndex, err)
			}

			spendPath = &arkscript.SpendPath{
				SpendInfo: spendInfo,
			}
		}

		signDesc := spendPath.BuildSignDescriptor(
			si.KeyDesc, si.PrevOut, sigHashes, prevFetcher,
			inputIndex,
		)

		sig, err := s.wallet.SignOutputRaw(toSign, signDesc)
		if err != nil {
			return fmt.Errorf("join auth sign input %d: %w",
				inputIndex, err)
		}

		witness, err := spendPath.SingleSigWitness(
			sig, signDesc.HashType,
		)
		if err != nil {
			return fmt.Errorf("join auth witness input %d: %w",
				inputIndex, err)
		}

		toSign.TxIn[inputIndex].Witness = witness
	}

	return nil
}

// signJoinAuthMessageInput signs to_sign input 0, which spends the
// challenge output and binds the signature to the identifier key.
func signJoinAuthMessageInput(wallet ClientWallet, toSign *wire.MsgTx,
	toSpend *wire.MsgTx, identifierKeyDesc keychain.KeyDescriptor,
	sigHashes *txscript.TxSigHashes,
	prevFetcher txscript.PrevOutputFetcher) error {

	if wallet == nil {
		return fmt.Errorf("wallet signer must be provided")
	}

	if identifierKeyDesc.PubKey == nil {
		return fmt.Errorf("join auth identifier pubkey is missing")
	}

	messageSignDesc := &input.SignDescriptor{
		KeyDesc:           identifierKeyDesc,
		Output:            toSpend.TxOut[0],
		HashType:          txscript.SigHashDefault,
		InputIndex:        0,
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		TapTweak:          []byte{},
	}

	messageSig, err := wallet.SignOutputRaw(
		toSign, messageSignDesc,
	)
	if err != nil {
		return fmt.Errorf("join auth sign message input: %w", err)
	}

	toSign.TxIn[0].Witness = wire.TxWitness{
		messageSig.Serialize(),
	}

	return nil
}

// joinAuthValidFrom queries the current best height used as the lower
// bound in join-auth intent validity metadata.
func joinAuthValidFrom(ctx context.Context,
	env *ClientEnvironment) (uint32, error) {

	if env == nil {
		return 0, fmt.Errorf("client environment must be provided")
	}

	if env.QueryBestHeight == nil {
		return 0, fmt.Errorf("join auth valid-from query function " +
			"must be provided")
	}

	height, err := env.QueryBestHeight(ctx)
	if err != nil {
		return 0, fmt.Errorf("query join auth valid-from height: %w",
			err)
	}

	return height, nil
}

// joinAuthValidUntil returns the default join-auth expiration height.
func joinAuthValidUntil(currentHeight uint32) uint32 {
	if currentHeight > ^uint32(0)-joinRoundAuthWindowBlocks {
		return ^uint32(0)
	}

	return currentHeight + joinRoundAuthWindowBlocks
}
