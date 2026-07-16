package lndrest

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
)

// Signer REST paths, taken from lnd's signrpc grpc-gateway pattern vars.
const (
	pathSignOutputRaw     = "/v2/signer/signraw"
	pathComputeInputScr   = "/v2/signer/inputscript"
	pathSignMessage       = "/v2/signer/signmessage"
	pathDeriveSharedKey   = "/v2/signer/sharedkey"
	pathMuSig2CreateSess  = "/v2/signer/musig2/createsession"
	pathMuSig2RegNonces   = "/v2/signer/musig2/registernonces"
	pathMuSig2RegCombined = "/v2/signer/musig2/registercombinednonce"
	pathMuSig2GetCombined = "/v2/signer/musig2/getcombinednonce"
	pathMuSig2Sign        = "/v2/signer/musig2/sign"
	pathMuSig2CombineSig  = "/v2/signer/musig2/combinesig"
	pathMuSig2Cleanup     = "/v2/signer/musig2/cleanup"
)

// signerClient implements lndclient.SignerClient over lnd's REST gateway. It
// also exposes SignOutputRawWithLocator so the wallet backend's locator-aware
// signing path works over REST without touching a raw gRPC client.
type signerClient struct {
	conn *conn
}

// A compile-time check that signerClient satisfies the lndclient interface.
var _ lndclient.SignerClient = (*signerClient)(nil)

// RawClientWithMacAuth is required by the lndclient ServiceClient interface but
// has no meaning over REST: there is no raw gRPC client to hand back. It
// returns a nil client; the locator-aware signing path uses
// SignOutputRawWithLocator instead, so this is never dereferenced.
func (s *signerClient) RawClientWithMacAuth(parentCtx context.Context) (
	context.Context, time.Duration, signrpc.SignerClient) {

	return parentCtx, s.conn.timeout, nil
}

// marshalSignDescriptors converts lndclient sign descriptors into their signrpc
// counterparts, mirroring lndclient's own marshalling. When fullDescriptor is
// true the key locator is forwarded whenever either component is non-zero
// (fixing the family-6/index-0 identity path); otherwise only one of the raw
// public key or the locator is populated, matching lndclient's partial form.
func marshalSignDescriptors(signDescriptors []*lndclient.SignDescriptor,
	fullDescriptor bool) ([]*signrpc.SignDescriptor, error) {

	rpcSignDescs := make([]*signrpc.SignDescriptor, len(signDescriptors))
	for i, signDesc := range signDescriptors {
		if signDesc == nil {
			return nil, fmt.Errorf("sign descriptor %d is nil", i)
		}
		if signDesc.Output == nil {
			return nil, fmt.Errorf("sign descriptor %d has "+
				"no output", i)
		}

		keyDesc := &signrpc.KeyDescriptor{}
		loc := signDesc.KeyDesc.KeyLocator
		switch {
		// Full form: forward the pubkey and, whenever any locator
		// component is set, the locator too.
		case fullDescriptor:
			if signDesc.KeyDesc.PubKey != nil {
				keyDesc.RawKeyBytes = signDesc.KeyDesc.PubKey.
					SerializeCompressed()
			}
			if loc.Family != 0 || loc.Index != 0 {
				keyDesc.KeyLoc = &signrpc.KeyLocator{
					KeyFamily: int32(loc.Family),
					KeyIndex:  int32(loc.Index),
				}
			}

		// Partial form: either the pubkey or the locator, not both.
		case signDesc.KeyDesc.PubKey != nil:
			keyDesc.RawKeyBytes = signDesc.KeyDesc.PubKey.
				SerializeCompressed()

		default:
			keyDesc.KeyLoc = &signrpc.KeyLocator{
				KeyFamily: int32(loc.Family),
				KeyIndex:  int32(loc.Index),
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

	return rpcSignDescs, nil
}

// marshalTxOut converts previous outputs into their signrpc counterparts.
func marshalTxOut(outputs []*wire.TxOut) []*signrpc.TxOut {
	rpcOutputs := make([]*signrpc.TxOut, len(outputs))
	for i, output := range outputs {
		if output == nil {
			continue
		}

		rpcOutputs[i] = &signrpc.TxOut{
			PkScript: output.PkScript,
			Value:    output.Value,
		}
	}

	return rpcOutputs
}

// signOutputRaw is the shared implementation of the SignOutputRaw variants.
func (s *signerClient) signOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor, prevOutputs []*wire.TxOut,
	fullDescriptor bool) ([][]byte, error) {

	txRaw, err := encodeTx(tx)
	if err != nil {
		return nil, err
	}
	rpcSignDescs, err := marshalSignDescriptors(
		signDescriptors, fullDescriptor,
	)
	if err != nil {
		return nil, err
	}

	req := &signrpc.SignReq{
		RawTxBytes:  txRaw,
		SignDescs:   rpcSignDescs,
		PrevOutputs: marshalTxOut(prevOutputs),
	}
	resp := &signrpc.SignResp{}
	if err := s.conn.post(ctx, pathSignOutputRaw, req, resp); err != nil {
		return nil, err
	}

	return resp.RawSigs, nil
}

// SignOutputRaw generates signatures for the given inputs/outputs.
func (s *signerClient) SignOutputRaw(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	return s.signOutputRaw(ctx, tx, signDescriptors, prevOutputs, false)
}

// SignOutputRawKeyLocator is the locator-forwarding variant of SignOutputRaw.
func (s *signerClient) SignOutputRawKeyLocator(ctx context.Context,
	tx *wire.MsgTx, signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	return s.signOutputRaw(ctx, tx, signDescriptors, prevOutputs, true)
}

// SignOutputRawWithLocator is the transport-agnostic seam the wallet backend
// uses to always forward the key locator. Over REST this is simply
// SignOutputRaw with the full descriptor, so no raw gRPC client is needed.
func (s *signerClient) SignOutputRawWithLocator(ctx context.Context,
	tx *wire.MsgTx, signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	return s.signOutputRaw(ctx, tx, signDescriptors, prevOutputs, true)
}

// ComputeInputScript generates complete input scripts for the given inputs.
func (s *signerClient) ComputeInputScript(ctx context.Context, tx *wire.MsgTx,
	signDescriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([]*input.Script, error) {

	txRaw, err := encodeTx(tx)
	if err != nil {
		return nil, err
	}
	rpcSignDescs, err := marshalSignDescriptors(signDescriptors, false)
	if err != nil {
		return nil, err
	}

	req := &signrpc.SignReq{
		RawTxBytes:  txRaw,
		SignDescs:   rpcSignDescs,
		PrevOutputs: marshalTxOut(prevOutputs),
	}
	resp := &signrpc.InputScriptResp{}
	if err := s.conn.post(ctx, pathComputeInputScr, req, resp); err != nil {
		return nil, err
	}

	inputScripts := make([]*input.Script, 0, len(resp.InputScripts))
	for _, inputScript := range resp.InputScripts {
		inputScripts = append(inputScripts, &input.Script{
			SigScript: inputScript.SigScript,
			Witness:   inputScript.Witness,
		})
	}

	return inputScripts, nil
}

// SignMessage signs a message with the key at the locator, applying any option
// mutators (e.g. Schnorr signing with a tag) to the request before sending.
func (s *signerClient) SignMessage(ctx context.Context, msg []byte,
	locator keychain.KeyLocator, opts ...lndclient.SignMessageOption) (
	[]byte, error) {

	req := &signrpc.SignMessageReq{
		Msg: msg,
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(locator.Family),
			KeyIndex:  int32(locator.Index),
		},
	}
	for _, opt := range opts {
		opt(req)
	}

	resp := &signrpc.SignMessageResp{}
	if err := s.conn.post(ctx, pathSignMessage, req, resp); err != nil {
		return nil, err
	}

	return resp.Signature, nil
}

// VerifyMessage is not used by the wallet backend and is unsupported over REST.
func (s *signerClient) VerifyMessage(_ context.Context, _, _ []byte, _ [33]byte,
	_ ...lndclient.VerifyMessageOption) (bool, error) {

	return false, errUnsupportedOverREST
}

// DeriveSharedKey performs ECDH between the ephemeral key and the located key.
func (s *signerClient) DeriveSharedKey(ctx context.Context,
	ephemeralPubKey *btcec.PublicKey, keyLocator *keychain.KeyLocator) (
	[32]byte, error) {

	req := &signrpc.SharedKeyRequest{
		EphemeralPubkey: ephemeralPubKey.SerializeCompressed(),
	}
	if keyLocator != nil {
		req.KeyLoc = &signrpc.KeyLocator{
			KeyFamily: int32(keyLocator.Family),
			KeyIndex:  int32(keyLocator.Index),
		}
	}

	resp := &signrpc.SharedKeyResponse{}
	if err := s.conn.post(ctx, pathDeriveSharedKey, req, resp); err != nil {
		return [32]byte{}, err
	}

	var sharedKey [32]byte
	copy(sharedKey[:], resp.SharedKey)

	return sharedKey, nil
}

// marshalMuSig2Version maps the input MuSig2 version enum to the RPC enum,
// mirroring lndclient's own mapping.
func marshalMuSig2Version(version input.MuSig2Version) (signrpc.MuSig2Version,
	error) {

	switch version {
	case input.MuSig2Version040:
		return signrpc.MuSig2Version_MUSIG2_VERSION_V040, nil

	case input.MuSig2Version100RC2:
		return signrpc.MuSig2Version_MUSIG2_VERSION_V100RC2, nil

	default:
		return signrpc.MuSig2Version_MUSIG2_VERSION_UNDEFINED,
			fmt.Errorf("invalid MuSig2 version %v", version)
	}
}

// MuSig2CreateSession creates a new MuSig2 signing session.
func (s *signerClient) MuSig2CreateSession(ctx context.Context,
	version input.MuSig2Version, signerLoc *keychain.KeyLocator,
	signers [][]byte, opts ...lndclient.MuSig2SessionOpts) (
	*input.MuSig2SessionInfo, error) {

	rpcVersion, err := marshalMuSig2Version(version)
	if err != nil {
		return nil, err
	}

	req := &signrpc.MuSig2SessionRequest{
		KeyLoc: &signrpc.KeyLocator{
			KeyFamily: int32(signerLoc.Family),
			KeyIndex:  int32(signerLoc.Index),
		},
		AllSignerPubkeys: signers,
		Version:          rpcVersion,
	}
	for _, opt := range opts {
		opt(req)
	}

	resp := &signrpc.MuSig2SessionResponse{}
	if err := s.conn.post(
		ctx, pathMuSig2CreateSess, req, resp,
	); err != nil {
		return nil, err
	}

	combinedKey, err := schnorr.ParsePubKey(resp.CombinedKey)
	if err != nil {
		return nil, fmt.Errorf("could not parse combined key: %w", err)
	}

	session := &input.MuSig2SessionInfo{
		CombinedKey:   combinedKey,
		HaveAllNonces: resp.HaveAllNonces,
	}

	if len(resp.LocalPublicNonces) != musig2.PubNonceSize {
		return nil, fmt.Errorf("unexpected local nonce size: %d",
			len(resp.LocalPublicNonces))
	}
	copy(session.PublicNonce[:], resp.LocalPublicNonces)

	if len(resp.SessionId) != 32 {
		return nil, fmt.Errorf("unexpected session ID length: %d",
			len(resp.SessionId))
	}
	copy(session.SessionID[:], resp.SessionId)

	return session, nil
}

// noncesToBytes flattens fixed-size public nonces into a byte-slice slice.
func noncesToBytes(nonces [][musig2.PubNonceSize]byte) [][]byte {
	nonceBytes := make([][]byte, len(nonces))
	for i := range nonces {
		nonceBytes[i] = nonces[i][:]
	}

	return nonceBytes
}

// MuSig2RegisterNonces registers additional public nonces for a session.
func (s *signerClient) MuSig2RegisterNonces(ctx context.Context,
	sessionID [32]byte, nonces [][66]byte) (bool, error) {

	req := &signrpc.MuSig2RegisterNoncesRequest{
		SessionId:               sessionID[:],
		OtherSignerPublicNonces: noncesToBytes(nonces),
	}

	resp := &signrpc.MuSig2RegisterNoncesResponse{}
	if err := s.conn.post(ctx, pathMuSig2RegNonces, req, resp); err != nil {
		return false, err
	}

	return resp.HaveAllNonces, nil
}

// MuSig2Sign creates the local partial signature for the message digest.
func (s *signerClient) MuSig2Sign(ctx context.Context, sessionID [32]byte,
	message [32]byte, cleanup bool) ([]byte, error) {

	req := &signrpc.MuSig2SignRequest{
		SessionId:     sessionID[:],
		MessageDigest: message[:],
		Cleanup:       cleanup,
	}

	resp := &signrpc.MuSig2SignResponse{}
	if err := s.conn.post(ctx, pathMuSig2Sign, req, resp); err != nil {
		return nil, err
	}

	return resp.LocalPartialSignature, nil
}

// MuSig2CombineSig combines partial signatures, returning the final signature
// once every participant has registered.
func (s *signerClient) MuSig2CombineSig(ctx context.Context, sessionID [32]byte,
	otherPartialSigs [][]byte) (bool, []byte, error) {

	req := &signrpc.MuSig2CombineSigRequest{
		SessionId:              sessionID[:],
		OtherPartialSignatures: otherPartialSigs,
	}

	resp := &signrpc.MuSig2CombineSigResponse{}
	if err := s.conn.post(
		ctx, pathMuSig2CombineSig, req, resp,
	); err != nil {
		return false, nil, err
	}

	return resp.HaveAllSignatures, resp.FinalSignature, nil
}

// MuSig2Cleanup removes a session from lnd's memory.
func (s *signerClient) MuSig2Cleanup(ctx context.Context,
	sessionID [32]byte) error {

	req := &signrpc.MuSig2CleanupRequest{
		SessionId: sessionID[:],
	}

	return s.conn.post(
		ctx, pathMuSig2Cleanup, req, &signrpc.MuSig2CleanupResponse{},
	)
}

// MuSig2RegisterCombinedNonce registers a pre-aggregated combined nonce.
func (s *signerClient) MuSig2RegisterCombinedNonce(ctx context.Context,
	sessionID [32]byte, combinedNonce [66]byte) error {

	req := &signrpc.MuSig2RegisterCombinedNonceRequest{
		SessionId:           sessionID[:],
		CombinedPublicNonce: combinedNonce[:],
	}

	return s.conn.post(
		ctx, pathMuSig2RegCombined, req,
		&signrpc.MuSig2RegisterCombinedNonceResponse{},
	)
}

// MuSig2GetCombinedNonce retrieves the combined nonce for a session.
func (s *signerClient) MuSig2GetCombinedNonce(ctx context.Context,
	sessionID [32]byte) ([66]byte, error) {

	req := &signrpc.MuSig2GetCombinedNonceRequest{
		SessionId: sessionID[:],
	}

	resp := &signrpc.MuSig2GetCombinedNonceResponse{}
	if err := s.conn.post(
		ctx, pathMuSig2GetCombined, req, resp,
	); err != nil {
		return [66]byte{}, err
	}

	if len(resp.CombinedPublicNonce) != 66 {
		return [66]byte{}, fmt.Errorf("unexpected combined nonce "+
			"size: %d", len(resp.CombinedPublicNonce))
	}

	var combinedNonce [66]byte
	copy(combinedNonce[:], resp.CombinedPublicNonce)

	return combinedNonce, nil
}
