package lndbackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/build"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
)

// ClientWallet adapts lndclient's remote signing interfaces to the
// round.ClientWallet interface (input.Signer + DeriveNextKey). This
// allows the round actor's FSM to sign VTXO tree branches and forfeit
// transactions through lnd's remote signer without requiring a local
// wallet.
//
// Because input.Signer does not carry context, the adapter uses a
// background context for all RPC calls. The underlying gRPC deadline
// from the lndclient dial options still applies.
type ClientWallet struct {
	signer    Signer
	walletKit lndclient.WalletKitClient

	// Log is an optional logger for this wallet. If None, the wallet falls
	// back to extracting a logger from context.
	Log fn.Option[btclog.Logger]
}

// Signer is the transport-agnostic signing surface the ClientWallet needs. It
// is the full lndclient.SignerClient plus a locator-forwarding SignOutputRaw
// so the family-6/index-0 identity signing path works over any lnd transport.
// The gRPC lndclient signer gains the extra method via grpcLocatorSigner (which
// forwards through the raw client); the REST signer implements it directly, so
// the REST transport never has to expose a raw gRPC client.
type Signer interface {
	lndclient.SignerClient

	// SignOutputRawWithLocator mirrors SignOutputRaw but always forwards
	// the key locator when one is available, including family/index pairs
	// with a zero component such as lnd's node key at family 6 index 0.
	SignOutputRawWithLocator(ctx context.Context, tx *wire.MsgTx,
		signDescriptors []*lndclient.SignDescriptor,
		prevOutputs []*wire.TxOut) ([][]byte, error)
}

// NewClientWallet creates a new ClientWallet from the lndclient signer
// and wallet kit clients. A signer that already implements the locator-aware
// Signer interface (the REST backend) is used directly; a plain gRPC
// lndclient.SignerClient is wrapped so the locator-forwarding path routes
// through its raw client.
func NewClientWallet(
	signer lndclient.SignerClient,
	walletKit lndclient.WalletKitClient,
) *ClientWallet {

	return &ClientWallet{
		signer:    asLocatorSigner(signer),
		walletKit: walletKit,
	}
}

// asLocatorSigner promotes a plain lndclient signer to the locator-aware Signer
// interface, returning it unchanged when it already satisfies Signer.
func asLocatorSigner(signer lndclient.SignerClient) Signer {
	if ls, ok := signer.(Signer); ok {
		return ls
	}

	return &grpcLocatorSigner{SignerClient: signer}
}

// grpcLocatorSigner adapts a raw gRPC lndclient signer to the Signer interface
// by implementing SignOutputRawWithLocator through the raw signrpc client,
// preserving the locator-forwarding behavior on the gRPC transport.
type grpcLocatorSigner struct {
	lndclient.SignerClient
}

// SignOutputRawWithLocator mirrors lndclient.SignOutputRaw but always includes
// the key locator when one is available, including family/index zero pairs such
// as lnd's node key at family 6 index 0.
//
// TODO(wavelength): drop this helper and go back to lndclient.SignOutputRaw
// once upstream forwards the key locator on SignOutputRaw for descriptors whose
// KeyLocator has Family != 0 but Index == 0. The current helper in lndclient
// omits the locator in that case, which breaks signing for the family-6/index-0
// identity path used by the indexer proof signer.
func (g *grpcLocatorSigner) SignOutputRawWithLocator(ctx context.Context,
	tx *wire.MsgTx, signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	rpcCtx, timeout, rawClient := g.RawClientWithMacAuth(ctx)
	rpcCtx, cancel := context.WithTimeout(rpcCtx, timeout)
	defer cancel()

	var txBuf bytes.Buffer
	if err := tx.Serialize(&txBuf); err != nil {
		return nil, fmt.Errorf("serialize tx: %w", err)
	}

	rpcSignDescs := make([]*signrpc.SignDescriptor, len(signDescriptors))
	for i, signDesc := range signDescriptors {
		if signDesc == nil {
			return nil, fmt.Errorf("sign descriptor %d is nil", i)
		}

		keyDesc := &signrpc.KeyDescriptor{}
		if signDesc.KeyDesc.PubKey != nil {
			keyDesc.RawKeyBytes = signDesc.KeyDesc.PubKey.
				SerializeCompressed()
		}

		if signDesc.KeyDesc.KeyLocator.Family != 0 ||
			signDesc.KeyDesc.KeyLocator.Index != 0 {

			keyDesc.KeyLoc = &signrpc.KeyLocator{
				KeyFamily: int32(
					signDesc.KeyDesc.KeyLocator.Family,
				),
				KeyIndex: int32(
					signDesc.KeyDesc.KeyLocator.Index,
				),
			}
		}

		var doubleTweak []byte
		if signDesc.DoubleTweak != nil {
			doubleTweak = signDesc.DoubleTweak.Serialize()
		}

		rpcSignDescs[i] = &signrpc.SignDescriptor{
			WitnessScript: signDesc.WitnessScript,
			SignMethod: lndclient.MarshalSignMethod(
				signDesc.SignMethod,
			),
			Output: &signrpc.TxOut{
				PkScript: signDesc.Output.PkScript,
				Value:    signDesc.Output.Value,
			},
			Sighash:     uint32(signDesc.HashType),
			InputIndex:  int32(signDesc.InputIndex),
			KeyDesc:     keyDesc,
			SingleTweak: signDesc.SingleTweak,
			DoubleTweak: doubleTweak,
			TapTweak:    signDesc.TapTweak,
		}
	}

	rpcPrevOutputs := make([]*signrpc.TxOut, len(prevOutputs))
	for i, output := range prevOutputs {
		if output == nil {
			continue
		}

		rpcPrevOutputs[i] = &signrpc.TxOut{
			PkScript: output.PkScript,
			Value:    output.Value,
		}
	}

	resp, err := rawClient.SignOutputRaw(rpcCtx, &signrpc.SignReq{
		RawTxBytes:  txBuf.Bytes(),
		SignDescs:   rpcSignDescs,
		PrevOutputs: rpcPrevOutputs,
	})
	if err != nil {
		return nil, err
	}

	return resp.RawSigs, nil
}

// logger returns the configured logger, falling back to the context logger.
func (c *ClientWallet) logger(ctx context.Context) btclog.Logger {
	return c.Log.UnwrapOr(build.LoggerFromContext(ctx))
}

// Compile-time check that ClientWallet satisfies the interface
// expected by round.RoundClientConfig.Wallet. We can't import the
// round package here (that would create a cycle), so we check against
// input.Signer which is the signing half of round.ClientWallet.
//
// NOTE: Full round.ClientWallet check (which adds DeriveNextKey and
// MuSig2* methods) is omitted to avoid the import cycle. Any drift
// will surface as a compile error in waved/server.go where
// ClientWallet is passed to the round config.
var _ input.Signer = (*ClientWallet)(nil)

// DeriveNextKey derives the next key in the specified key family via
// lnd's WalletKit RPC. This is used by the round actor to generate
// fresh VTXO signing keys for each round.
func (c *ClientWallet) DeriveNextKey(ctx context.Context,
	family keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	c.logger(ctx).DebugS(ctx, "Deriving next key via client wallet",
		slog.Int("key_family", int(family)),
	)

	keyDesc, err := c.walletKit.DeriveNextKey(ctx, int32(family))
	if err != nil {
		return nil, fmt.Errorf("derive next key: %w", err)
	}

	c.logger(ctx).DebugS(ctx, "Derived next key via client wallet",
		slog.Int("key_family", int(family)),
		slog.Int("key_index", int(keyDesc.Index)),
	)

	return keyDesc, nil
}

// SignOutputRaw generates a schnorr/ECDSA signature for a single input
// of the provided transaction according to the sign descriptor. The
// call is forwarded to lnd's remote signer via gRPC.
func (c *ClientWallet) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	c.logger(context.TODO()).DebugS(
		context.TODO(),
		"Signing output raw via LND remote signer",
		slog.Int("input_index", signDesc.InputIndex),
		slog.String("sign_method", signDesc.SignMethod.String()),
	)

	lndDesc := inputDescToLndclient(signDesc)

	// Collect prevouts for taproot sighash computation. When the
	// caller provides a PrevOutputFetcher we extract every input's
	// previous output; otherwise we fall back to the single output
	// in the sign descriptor.
	prevOuts := prevOutputsFromDesc(tx, signDesc)

	sigs, err := c.signer.SignOutputRawWithLocator(
		context.Background(), tx, []*lndclient.SignDescriptor{
			lndDesc,
		}, prevOuts,
	)
	if err != nil {
		return nil, fmt.Errorf("sign output raw: %w", err)
	}

	if len(sigs) == 0 {
		return nil, fmt.Errorf("no signatures returned")
	}

	c.logger(context.TODO()).DebugS(
		context.TODO(),
		"Signed output raw successfully",
		slog.Int("input_index", signDesc.InputIndex),
		slog.Int("sig_len", len(sigs[0])),
	)

	return parseSigBytes(sigs[0], signDesc.SignMethod)
}

// ComputeInputScript generates a complete input script (witness) for
// the specified input. This is used for standard p2wkh and np2wkh
// spends.
func (c *ClientWallet) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	lndDesc := inputDescToLndclient(signDesc)
	prevOuts := prevOutputsFromDesc(tx, signDesc)

	scripts, err := c.signer.ComputeInputScript(
		context.Background(), tx, []*lndclient.SignDescriptor{
			lndDesc,
		}, prevOuts,
	)
	if err != nil {
		return nil, fmt.Errorf("compute input script: %w", err)
	}

	if len(scripts) == 0 {
		return nil, fmt.Errorf("no scripts returned")
	}

	return scripts[0], nil
}

// MuSig2CreateSession creates a new MuSig2 signing session via lnd's
// remote signer. The session is identified by the returned session ID
// and manages nonce aggregation and partial signing state.
func (c *ClientWallet) MuSig2CreateSession(version input.MuSig2Version,
	locator keychain.KeyLocator, allSignerPubkeys []*btcec.PublicKey,
	tweaks *input.MuSig2Tweaks, otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	c.logger(context.TODO()).DebugS(
		context.TODO(),
		"Creating MuSig2 session via LND",
		slog.Int("num_signers", len(allSignerPubkeys)),
		slog.Int("num_other_nonces", len(otherNonces)),
		slog.Bool("has_local_nonces", localNonces != nil),
	)

	signerBytes := serializeMuSig2SignerPubKeys(
		version, allSignerPubkeys,
	)

	var opts []lndclient.MuSig2SessionOpts

	// Apply taproot tweaks if specified. The lndclient API uses
	// a separate option for taproot key path tweaks.
	if tweaks != nil {
		if tweaks.TaprootBIP0086Tweak || len(tweaks.TaprootTweak) > 0 {
			opts = append(
				opts, lndclient.MuSig2TaprootTweakOpt(
					tweaks.TaprootTweak,
					tweaks.TaprootBIP0086Tweak,
				),
			)
		}
	}

	if len(otherNonces) > 0 {
		opts = append(opts, lndclient.MuSig2NonceOpt(
			otherNonces,
		))
	}

	if localNonces != nil {
		opts = append(
			opts, lndclient.MuSig2LocalNonceOpt(
				localNonces.SecNonce,
			),
		)
	}

	sessionInfo, err := c.signer.MuSig2CreateSession(
		context.Background(), version, &locator, signerBytes, opts...,
	)
	if err != nil {
		return nil, fmt.Errorf("musig2 create session: %w", err)
	}

	c.logger(context.TODO()).DebugS(
		context.TODO(),
		"Created MuSig2 session successfully",
		slog.Int("num_signers", len(allSignerPubkeys)),
	)

	return sessionInfo, nil
}

// serializeMuSig2SignerPubKeys matches lnd's RPC expectations for each
// MuSig2 draft version. v0.4.0 uses x-only keys while v1.0.0rc2 uses
// compressed public keys.
func serializeMuSig2SignerPubKeys(version input.MuSig2Version,
	allSignerPubkeys []*btcec.PublicKey) [][]byte {

	signerBytes := make([][]byte, len(allSignerPubkeys))
	for i, pk := range allSignerPubkeys {
		switch version {
		case input.MuSig2Version040:
			signerBytes[i] = schnorr.SerializePubKey(pk)

		default:
			signerBytes[i] = pk.SerializeCompressed()
		}
	}

	return signerBytes
}

// MuSig2RegisterNonces registers additional public nonces for a
// MuSig2 session. Returns true once all nonces have been collected.
func (c *ClientWallet) MuSig2RegisterNonces(sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	return c.signer.MuSig2RegisterNonces(
		context.Background(), sessionID, nonces,
	)
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined
// nonce for a session, bypassing individual nonce registration.
func (c *ClientWallet) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	combinedNonce [musig2.PubNonceSize]byte,
) error {

	return c.signer.MuSig2RegisterCombinedNonce(
		context.Background(), sessionID, combinedNonce,
	)
}

// MuSig2GetCombinedNonce retrieves the combined nonce for a session
// after all individual nonces have been registered.
func (c *ClientWallet) MuSig2GetCombinedNonce(sessionID input.MuSig2SessionID) (
	[musig2.PubNonceSize]byte, error) {

	return c.signer.MuSig2GetCombinedNonce(
		context.Background(), sessionID,
	)
}

// MuSig2Sign creates a partial signature using the local key for the
// specified session. The message must be a 32-byte SHA256 digest.
func (c *ClientWallet) MuSig2Sign(sessionID input.MuSig2SessionID,
	message [sha256.Size]byte, cleanup bool) (*musig2.PartialSignature,
	error) {

	c.logger(context.TODO()).DebugS(
		context.TODO(),
		"Creating MuSig2 partial signature",
		slog.Bool("cleanup", cleanup),
	)

	sigBytes, err := c.signer.MuSig2Sign(
		context.Background(), sessionID, message, cleanup,
	)
	if err != nil {
		return nil, fmt.Errorf("musig2 sign: %w", err)
	}

	var partialSig musig2.PartialSignature
	reader := bytes.NewReader(sigBytes)
	if err := partialSig.Decode(reader); err != nil {
		return nil, fmt.Errorf("decode partial sig: %w", err)
	}

	return &partialSig, nil
}

// MuSig2CombineSig combines partial signatures from all participants
// and returns the final Schnorr signature once all are registered.
func (c *ClientWallet) MuSig2CombineSig(sessionID input.MuSig2SessionID,
	otherPartialSigs []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	c.logger(context.TODO()).DebugS(
		context.TODO(),
		"Combining MuSig2 partial signatures",
		slog.Int("num_other_sigs", len(otherPartialSigs)),
	)

	sigBytes := make([][]byte, len(otherPartialSigs))
	for i, ps := range otherPartialSigs {
		var buf bytes.Buffer
		if err := ps.Encode(&buf); err != nil {
			return nil, false, fmt.Errorf("encode partial sig "+
				"%d: %w", i, err)
		}

		sigBytes[i] = buf.Bytes()
	}

	haveAll, finalSigBytes, err := c.signer.MuSig2CombineSig(
		context.Background(), sessionID, sigBytes,
	)
	if err != nil {
		return nil, false, fmt.Errorf("musig2 combine sig: %w", err)
	}

	if !haveAll || len(finalSigBytes) == 0 {
		return nil, haveAll, nil
	}

	finalSig, err := schnorr.ParseSignature(finalSigBytes)
	if err != nil {
		return nil, false, fmt.Errorf("parse final sig: %w", err)
	}

	return finalSig, true, nil
}

// MuSig2Cleanup removes a session from lnd's memory.
func (c *ClientWallet) MuSig2Cleanup(sessionID input.MuSig2SessionID) error {
	return c.signer.MuSig2Cleanup(
		context.Background(), sessionID,
	)
}

// parseSigBytes interprets raw signature bytes according to the sign
// method. Taproot key/script spends use schnorr (64 bytes), while
// legacy SegWit uses DER-encoded ECDSA.
func parseSigBytes(sigBytes []byte,
	method input.SignMethod) (input.Signature, error) {

	switch method {
	case input.TaprootKeySpendBIP0086SignMethod,
		input.TaprootKeySpendSignMethod,
		input.TaprootScriptSpendSignMethod:
		return schnorr.ParseSignature(sigBytes)

	default:
		return ecdsa.ParseDERSignature(sigBytes)
	}
}

// inputDescToLndclient converts an input.SignDescriptor to a
// lndclient.SignDescriptor. The two types have nearly identical fields
// but different Go types.
func inputDescToLndclient(
	desc *input.SignDescriptor) *lndclient.SignDescriptor {

	return &lndclient.SignDescriptor{
		KeyDesc:       desc.KeyDesc,
		SingleTweak:   desc.SingleTweak,
		DoubleTweak:   desc.DoubleTweak,
		TapTweak:      desc.TapTweak,
		WitnessScript: desc.WitnessScript,
		SignMethod:    desc.SignMethod,
		Output:        desc.Output,
		HashType:      desc.HashType,
		InputIndex:    desc.InputIndex,
	}
}

// prevOutputsFromDesc extracts the previous outputs needed for taproot
// sighash computation. If the sign descriptor includes a
// PrevOutputFetcher, we use it to collect all inputs' prev outputs.
// Otherwise we return just the single output from the descriptor.
func prevOutputsFromDesc(tx *wire.MsgTx,
	desc *input.SignDescriptor) []*wire.TxOut {

	if desc.PrevOutputFetcher != nil {
		prevOuts := make([]*wire.TxOut, len(tx.TxIn))
		for i, txIn := range tx.TxIn {
			prevOuts[i] = desc.PrevOutputFetcher.FetchPrevOutput(
				txIn.PreviousOutPoint,
			)
		}

		return prevOuts
	}

	// Fall back to a single-entry slice using the descriptor's
	// Output field.
	if desc.Output != nil {
		return []*wire.TxOut{desc.Output}
	}

	return nil
}
