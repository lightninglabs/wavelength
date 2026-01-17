// Package lndbackend provides lndclient-backed implementations of server
// interfaces for connecting to remote LND nodes.
package lndbackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// ErrNotImplemented is returned for MuSig2 methods that lndclient doesn't
// support directly.
var ErrNotImplemented = errors.New("not implemented in lndclient")

// LndWalletController implements rounds.WalletController using lndclient. This
// enables real coin selection and PSBT signing via the LND wallet.
type LndWalletController struct {
	walletKit lndclient.WalletKitClient
	signer    lndclient.SignerClient
}

// NewLndWalletController creates a new wallet controller connected to LND.
func NewLndWalletController(walletKit lndclient.WalletKitClient,
	signer lndclient.SignerClient) *LndWalletController {

	return &LndWalletController{
		walletKit: walletKit,
		signer:    signer,
	}
}

// FundPsbt performs coin selection and adds wallet inputs to fund the outputs
// in the PSBT. It also adds a change output if needed. Returns the change
// output index (-1 if no change).
//
// This uses the PsbtCoinSelect template mode which supports external inputs
// (inputs not belonging to lnd's wallet). This is required for Ark because
// boarding inputs belong to clients, not the server. External inputs must
// have their WitnessUtxo field populated for fee calculation.
func (l *LndWalletController) FundPsbt(ctx context.Context, packet *psbt.Packet,
	minConfs int32, feeRate chainfee.SatPerKWeight,
	account string) (int32, error) {

	// Serialize the PSBT to send to LND.
	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return -1, fmt.Errorf("serialize psbt: %w", err)
	}
	psbtBytes := buf.Bytes()

	// Build the FundPsbt request using coin_select template mode.
	// The coin_select mode is required because:
	//
	// 1. External inputs (boarding UTXOs from clients) are not in lnd's
	//    UTXO set
	//
	// 2. lnd will add wallet inputs to cover: outputs + fees -
	//    external_input_value
	//
	// 3. External inputs must have WitnessUtxo populated for fee
	//    calculation.
	req := &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_CoinSelect{
			CoinSelect: &walletrpc.PsbtCoinSelect{
				Psbt: psbtBytes,
				ChangeOutput: &walletrpc.PsbtCoinSelect_Add{
					Add: true,
				},
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: uint64(feeRate.FeePerVByte()),
		},
		Account:  account,
		MinConfs: minConfs,
	}

	fundedPacket, changeIdx, _, err := l.walletKit.FundPsbt(ctx, req)
	if err != nil {
		return -1, fmt.Errorf("fund psbt via lnd: %w", err)
	}

	*packet = *fundedPacket

	return changeIdx, nil
}

// FinalizePsbt signs all wallet-controlled inputs and finalizes the PSBT,
// making it ready for broadcast. Returns the finalized raw transaction.
func (l *LndWalletController) FinalizePsbt(ctx context.Context,
	packet *psbt.Packet) (*wire.MsgTx, error) {

	_, finalTx, err := l.walletKit.FinalizePsbt(ctx, packet, "")
	if err != nil {
		return nil, fmt.Errorf("finalize psbt via lnd: %w", err)
	}

	return finalTx, nil
}

// SignOutputRaw generates a signature for the passed transaction according to
// the data within the passed SignDescriptor.
//
// NOTE: The input.Signer interface doesn't include context, so we use a
// background context here. For production usage, consider adding context
// support to the calling code.
func (l *LndWalletController) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	ctx := context.Background()

	// Convert input.SignDescriptor to lndclient.SignDescriptor.
	lndSignDesc := convertSignDescriptor(signDesc)

	// Build prevOutputs from ALL transaction inputs. For Taproot signing
	// (BIP341), the sighash commits to all prevouts, so we must provide
	// the previous output for every input in the transaction.
	prevOutputs := make([]*wire.TxOut, len(tx.TxIn))
	for i, txIn := range tx.TxIn {
		prevOut := signDesc.PrevOutputFetcher.FetchPrevOutput(
			txIn.PreviousOutPoint,
		)
		if prevOut == nil {
			return nil, fmt.Errorf("missing prevout for input %d "+
				"(outpoint %s)", i, txIn.PreviousOutPoint)
		}
		prevOutputs[i] = prevOut
	}

	sigs, err := l.signer.SignOutputRaw(
		ctx, tx, []*lndclient.SignDescriptor{lndSignDesc}, prevOutputs,
	)
	if err != nil {
		return nil, fmt.Errorf("sign output raw via lnd: %w", err)
	}

	if len(sigs) != 1 {
		return nil, fmt.Errorf("expected 1 signature, got %d",
			len(sigs))
	}

	sig, err := schnorr.ParseSignature(sigs[0])
	if err != nil {
		return nil, fmt.Errorf("parse signature: %w", err)
	}

	return sig, nil
}

// ComputeInputScript generates a complete InputScript for the passed
// transaction with the signature as defined within the passed SignDescriptor.
func (l *LndWalletController) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	ctx := context.Background()

	// Convert input.SignDescriptor to lndclient.SignDescriptor.
	lndSignDesc := convertSignDescriptor(signDesc)

	prevOutputs := []*wire.TxOut{signDesc.Output}

	scripts, err := l.signer.ComputeInputScript(
		ctx, tx, []*lndclient.SignDescriptor{lndSignDesc}, prevOutputs,
	)
	if err != nil {
		return nil, fmt.Errorf("compute input script via lnd: %w", err)
	}

	if len(scripts) != 1 {
		return nil, fmt.Errorf("expected 1 script, got %d",
			len(scripts))
	}

	return scripts[0], nil
}

// MuSig2CreateSession creates a new MuSig2 signing session using the local key
// identified by the key locator.
func (l *LndWalletController) MuSig2CreateSession(version input.MuSig2Version,
	keyLoc keychain.KeyLocator, pubKeys []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	ctx := context.Background()

	// Convert public keys to byte slices.
	signers := make([][]byte, len(pubKeys))
	for i, pk := range pubKeys {
		signers[i] = pk.SerializeCompressed()
	}

	// Build session options.
	var opts []lndclient.MuSig2SessionOpts
	if tweaks != nil {
		if tweaks.TaprootBIP0086Tweak {
			opts = append(opts, lndclient.MuSig2TaprootTweakOpt(
				nil, true,
			))
		} else if len(tweaks.TaprootTweak) > 0 {
			opts = append(opts, lndclient.MuSig2TaprootTweakOpt(
				tweaks.TaprootTweak, false,
			))
		}
	}

	if len(otherNonces) > 0 {
		opts = append(opts, lndclient.MuSig2NonceOpt(otherNonces))
	}

	// Call LND's MuSig2CreateSession.
	sessionInfo, err := l.signer.MuSig2CreateSession(
		ctx, version, &keyLoc, signers, opts...,
	)
	if err != nil {
		return nil, fmt.Errorf("musig2 create session via lnd: %w", err)
	}

	return sessionInfo, nil
}

// MuSig2RegisterNonces registers one or more public nonces of other signing
// participants for a session identified by its ID.
func (l *LndWalletController) MuSig2RegisterNonces(
	sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	ctx := context.Background()

	haveAll, err := l.signer.MuSig2RegisterNonces(ctx, sessionID, nonces)
	if err != nil {
		return false, fmt.Errorf("musig2 register nonces "+
			"via lnd: %w", err)
	}

	return haveAll, nil
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce for a
// session. This is used when a coordinator has already aggregated all
// individual nonces and wants to distribute the combined nonce to participants.
func (l *LndWalletController) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	combinedNonce [musig2.PubNonceSize]byte,
) error {

	ctx := context.Background()

	err := l.signer.MuSig2RegisterCombinedNonce(
		ctx, sessionID, combinedNonce,
	)
	if err != nil {
		return fmt.Errorf("musig2 register combined nonce via lnd: %w",
			err)
	}

	return nil
}

// MuSig2GetCombinedNonce retrieves the combined nonce for a session. This will
// be available after either all individual nonces have been registered via
// MuSig2RegisterNonces, or a combined nonce has been registered via
// MuSig2RegisterCombinedNonce.
func (l *LndWalletController) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID,
) ([musig2.PubNonceSize]byte, error) {

	ctx := context.Background()

	combinedNonce, err := l.signer.MuSig2GetCombinedNonce(ctx, sessionID)
	if err != nil {
		return [musig2.PubNonceSize]byte{}, fmt.Errorf(
			"musig2 get combined nonce via lnd: %w", err,
		)
	}

	return combinedNonce, nil
}

// MuSig2Sign creates a partial signature using the local signing key.
func (l *LndWalletController) MuSig2Sign(sessionID input.MuSig2SessionID,
	message [sha256.Size]byte,
	cleanup bool) (*musig2.PartialSignature, error) {

	ctx := context.Background()

	sigBytes, err := l.signer.MuSig2Sign(ctx, sessionID, message, cleanup)
	if err != nil {
		return nil, fmt.Errorf("musig2 sign via lnd: %w", err)
	}

	// Parse the partial signature bytes. The format is 32 bytes for the
	// scalar value.
	var s btcec.ModNScalar
	if overflow := s.SetByteSlice(sigBytes); overflow {
		return nil, fmt.Errorf("partial signature scalar overflow")
	}

	return &musig2.PartialSignature{S: &s}, nil
}

// MuSig2CombineSig combines the given partial signatures with the local one.
func (l *LndWalletController) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	partialSigs []*musig2.PartialSignature,
) (*schnorr.Signature, bool, error) {

	ctx := context.Background()

	// Convert partial signatures to byte slices.
	sigBytes := make([][]byte, len(partialSigs))
	for i, sig := range partialSigs {
		b := sig.S.Bytes()
		sigBytes[i] = b[:]
	}

	haveAll, finalSigBytes, err := l.signer.MuSig2CombineSig(
		ctx, sessionID, sigBytes,
	)
	if err != nil {
		return nil, false, fmt.Errorf(
			"musig2 combine sig via lnd: %w", err,
		)
	}

	if !haveAll || len(finalSigBytes) == 0 {
		return nil, haveAll, nil
	}

	// Parse the final signature.
	finalSig, err := schnorr.ParseSignature(finalSigBytes)
	if err != nil {
		return nil, false, fmt.Errorf("parse final signature: %w", err)
	}

	return finalSig, haveAll, nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (l *LndWalletController) MuSig2Cleanup(sessionID input.MuSig2SessionID,
) error {

	ctx := context.Background()

	err := l.signer.MuSig2Cleanup(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("musig2 cleanup via lnd: %w", err)
	}

	return nil
}

// convertSignDescriptor converts an input.SignDescriptor to an
// lndclient.SignDescriptor.
func convertSignDescriptor(sd *input.SignDescriptor) *lndclient.SignDescriptor {
	return &lndclient.SignDescriptor{
		KeyDesc:       sd.KeyDesc,
		SingleTweak:   sd.SingleTweak,
		DoubleTweak:   sd.DoubleTweak,
		TapTweak:      sd.TapTweak,
		WitnessScript: sd.WitnessScript,
		Output:        sd.Output,
		HashType:      sd.HashType,
		InputIndex:    sd.InputIndex,
		SignMethod:    sd.SignMethod,
	}
}

// Compile-time check that LndWalletController implements
// rounds.WalletController.
var _ rounds.WalletController = (*LndWalletController)(nil)

// Compile-time check that LndWalletController implements input.Signer (which is
// what round.ClientWallet requires on the client side).
var _ input.Signer = (*LndWalletController)(nil)

// DeriveNextKey derives the next key in the specified key family using LND's
// key derivation infrastructure. This satisfies the client's round.ClientWallet
// interface for VTXO signing key derivation.
func (l *LndWalletController) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	return l.walletKit.DeriveNextKey(ctx, int32(family))
}

// DeriveKey derives an arbitrary key specified by the KeyLocator using LND's
// key derivation infrastructure.
func (l *LndWalletController) DeriveKey(ctx context.Context,
	keyLoc keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	return l.walletKit.DeriveKey(ctx, &keyLoc)
}
