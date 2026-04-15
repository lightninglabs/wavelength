//go:build systest

package systest

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

var (
	// oorChainParams are the chain parameters used by the docker harness in
	// systests.
	oorChainParams = &chaincfg.RegressionNetParams
)

const (
	// oorExitDelay is the VTXO/Checkpoint CSV delay used for OOR systests.
	oorExitDelay = uint32(10)

	// oorConfirmDepth is the number of blocks we mine to make
	// sure the funding txs are confirmed and available for
	// package relay.
	oorConfirmDepth = 6

	// oorVTXOParentFeeSat is the fee we pay in the
	// "VTXO creation" transaction that mints a spendable
	// VTXO UTXO for tests.
	oorVTXOParentFeeSat = int64(2_000)

	// oorCheckpointFeeSat is the sat amount we pay in the
	// checkpoint tx by adding a sponsor input + change output.
	// This makes the checkpoint relayable under default
	// minrelaytxfee policy.
	oorCheckpointFeeSat = int64(5_000)

	// oorCPFPPackageFeeSat is the sat amount we pay in the
	// CPFP child to cover the fee-less Ark tx.
	oorCPFPPackageFeeSat = int64(15_000)
)

// lndRPCSigner adapts an lnd signrpc-backed SignerClient into the in-process
// input.Signer interface used by the OOR primitives.
//
// This lets us run system tests against a "real wallet" key store and actual
// schnorr signatures, without routing through Ark RPC.
type lndRPCSigner struct {
	client  lndclient.SignerClient
	timeout time.Duration
}

// NewLNDRPCSigner creates a new input.Signer backed by the given lndclient
// Signer service.
func NewLNDRPCSigner(client lndclient.SignerClient,
	timeout time.Duration) input.Signer {

	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &lndRPCSigner{
		client:  client,
		timeout: timeout,
	}
}

// SignOutputRaw signs the given input using signrpc and returns a schnorr
// signature without appended sighash flag.
func (s *lndRPCSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {

	if tx == nil {
		return nil, fmt.Errorf("tx must be provided")
	}

	if signDesc == nil {
		return nil, fmt.Errorf("sign descriptor must be provided")
	}

	if signDesc.Output == nil {
		return nil, fmt.Errorf(
			"sign descriptor output must be provided",
		)
	}

	prevOutputs := make([]*wire.TxOut, len(tx.TxIn))
	if signDesc.InputIndex < 0 || signDesc.InputIndex >= len(prevOutputs) {
		return nil, fmt.Errorf("input index %d out of bounds",
			signDesc.InputIndex)
	}

	// If a prev output fetcher is provided, use it to populate the full
	// prev outputs slice. For taproot (SegWit v1) sighashes, the signature
	// commits to all input prev outputs, so the remote signer must see the
	// full vector.
	if signDesc.PrevOutputFetcher != nil {
		for i := range tx.TxIn {
			op := tx.TxIn[i].PreviousOutPoint
			prev := signDesc.PrevOutputFetcher.FetchPrevOutput(op)
			if prev == nil {
				return nil, fmt.Errorf("missing prev output "+
					"for input %d outpoint %s", i,
					op.String())
			}

			prevOutputs[i] = prev
		}
	} else {
		prevOutputs[signDesc.InputIndex] = signDesc.Output

		// lndclient's RPC marshaller does not accept nil prev outputs.
		// We fill missing outputs with empty placeholders for segwit v0
		// signing and single-input cases.
		for i := range prevOutputs {
			if prevOutputs[i] == nil {
				prevOutputs[i] = &wire.TxOut{}
			}
		}
	}

	rpcDesc := &lndclient.SignDescriptor{
		KeyDesc:       signDesc.KeyDesc,
		SingleTweak:   signDesc.SingleTweak,
		DoubleTweak:   signDesc.DoubleTweak,
		TapTweak:      signDesc.TapTweak,
		WitnessScript: signDesc.WitnessScript,
		SignMethod:    signDesc.SignMethod,
		Output:        signDesc.Output,
		HashType:      signDesc.HashType,
		InputIndex:    signDesc.InputIndex,
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	rawSigs, err := s.client.SignOutputRaw(
		ctx, tx,
		[]*lndclient.SignDescriptor{rpcDesc},
		prevOutputs,
	)
	if err != nil {
		return nil, err
	}

	if len(rawSigs) != 1 {
		return nil, fmt.Errorf("expected 1 signature, got %d",
			len(rawSigs))
	}

	return input.ParseSignature(rawSigs[0])
}

// ComputeInputScript computes the segwit input scripts for the specified input,
// delegating to signrpc.
func (s *lndRPCSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	if tx == nil {
		return nil, fmt.Errorf("tx must be provided")
	}

	if signDesc == nil {
		return nil, fmt.Errorf("sign descriptor must be provided")
	}

	if signDesc.Output == nil {
		return nil, fmt.Errorf(
			"sign descriptor output must be provided",
		)
	}

	prevOutputs := make([]*wire.TxOut, len(tx.TxIn))
	if signDesc.InputIndex < 0 || signDesc.InputIndex >= len(prevOutputs) {
		return nil, fmt.Errorf("input index %d out of bounds",
			signDesc.InputIndex)
	}

	if signDesc.PrevOutputFetcher != nil {
		for i := range tx.TxIn {
			op := tx.TxIn[i].PreviousOutPoint
			prev := signDesc.PrevOutputFetcher.FetchPrevOutput(op)
			if prev == nil {
				return nil, fmt.Errorf("missing prev output "+
					"for input %d outpoint %s", i,
					op.String())
			}

			prevOutputs[i] = prev
		}
	} else {
		prevOutputs[signDesc.InputIndex] = signDesc.Output

		// lndclient's RPC marshaller does not accept nil prev outputs.
		for i := range prevOutputs {
			if prevOutputs[i] == nil {
				prevOutputs[i] = &wire.TxOut{}
			}
		}
	}

	rpcDesc := &lndclient.SignDescriptor{
		KeyDesc:       signDesc.KeyDesc,
		SingleTweak:   signDesc.SingleTweak,
		DoubleTweak:   signDesc.DoubleTweak,
		TapTweak:      signDesc.TapTweak,
		WitnessScript: signDesc.WitnessScript,
		SignMethod:    signDesc.SignMethod,
		Output:        signDesc.Output,
		HashType:      signDesc.HashType,
		InputIndex:    signDesc.InputIndex,
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	scripts, err := s.client.ComputeInputScript(
		ctx, tx,
		[]*lndclient.SignDescriptor{rpcDesc},
		prevOutputs,
	)
	if err != nil {
		return nil, err
	}

	if len(scripts) != 1 {
		return nil, fmt.Errorf("expected 1 input script, got %d",
			len(scripts))
	}

	return scripts[0], nil
}

// MuSig2CreateSession is not required by the current OOR system tests.
func (s *lndRPCSigner) MuSig2CreateSession(_ input.MuSig2Version,
	_ keychain.KeyLocator, _ []*btcec.PublicKey, _ *input.MuSig2Tweaks,
	_ [][musig2.PubNonceSize]byte, _ *musig2.Nonces) (
	*input.MuSig2SessionInfo, error) {

	return nil, fmt.Errorf("musig2 not implemented in systest signer")
}

// MuSig2RegisterNonces is not required by the current OOR system tests.
func (s *lndRPCSigner) MuSig2RegisterNonces(_ input.MuSig2SessionID,
	_ [][musig2.PubNonceSize]byte) (bool, error) {

	return false, fmt.Errorf("musig2 not implemented in systest signer")
}

// MuSig2RegisterCombinedNonce is not required by the current OOR system tests.
func (s *lndRPCSigner) MuSig2RegisterCombinedNonce(_ input.MuSig2SessionID,
	_ [musig2.PubNonceSize]byte) error {

	return fmt.Errorf("musig2 not implemented in systest signer")
}

// MuSig2GetCombinedNonce is not required by the current OOR system tests.
func (s *lndRPCSigner) MuSig2GetCombinedNonce(_ input.MuSig2SessionID) (
	[musig2.PubNonceSize]byte, error) {

	return [musig2.PubNonceSize]byte{},
		fmt.Errorf("musig2 not implemented in systest signer")
}

// MuSig2Sign is not required by the current OOR system tests.
func (s *lndRPCSigner) MuSig2Sign(_ input.MuSig2SessionID, _ [32]byte,
	_ bool) (*musig2.PartialSignature, error) {

	return nil, fmt.Errorf("musig2 not implemented in systest signer")
}

// MuSig2CombineSig is not required by the current OOR system tests.
func (s *lndRPCSigner) MuSig2CombineSig(_ input.MuSig2SessionID,
	_ []*musig2.PartialSignature) (*schnorr.Signature, bool, error) {

	return nil, false, fmt.Errorf(
		"musig2 not implemented in systest signer",
	)
}

// MuSig2Cleanup is not required by the current OOR system tests.
func (s *lndRPCSigner) MuSig2Cleanup(_ input.MuSig2SessionID) error {
	return fmt.Errorf("musig2 not implemented in systest signer")
}

var _ input.Signer = (*lndRPCSigner)(nil)

// mapPrevOutputFetcher is a txscript.PrevOutputFetcher backed by a map.
type mapPrevOutputFetcher struct {
	prev map[wire.OutPoint]*wire.TxOut
}

// FetchPrevOutput returns the prev output for a given outpoint.
func (f *mapPrevOutputFetcher) FetchPrevOutput(op wire.OutPoint) *wire.TxOut {
	if f == nil || f.prev == nil {
		return nil
	}

	return f.prev[op]
}

// oorSerializePSBT encodes a PSBT packet into raw bytes for test assertions.
func oorSerializePSBT(pkt *psbt.Packet) ([]byte, error) {
	if pkt == nil {
		return nil, fmt.Errorf("psbt must be provided")
	}

	var buf bytes.Buffer
	err := pkt.Serialize(&buf)
	if err != nil {
		return nil, fmt.Errorf("serialize psbt: %w", err)
	}

	return buf.Bytes(), nil
}

// oorFindOutpoint locates the outpoint and previous output for a pkScript in a
// transaction.
func oorFindOutpoint(tx *wire.MsgTx, txid chainhash.Hash,
	pkScript []byte) (wire.OutPoint, *wire.TxOut, error) {

	if tx == nil {
		return wire.OutPoint{}, nil, fmt.Errorf("tx must be provided")
	}

	if len(pkScript) == 0 {
		return wire.OutPoint{}, nil, fmt.Errorf(
			"pkScript must be provided",
		)
	}

	for i, out := range tx.TxOut {
		if !bytes.Equal(out.PkScript, pkScript) {
			continue
		}

		return wire.OutPoint{
			Hash:  txid,
			Index: uint32(i),
		}, out, nil
	}

	return wire.OutPoint{}, nil, fmt.Errorf("output not found")
}

// oorOwnerLeafOperatorCheckSig returns a minimal owner leaf script that is
// spendable by the operator key.
func oorOwnerLeafOperatorCheckSig(operatorKey *btcec.PublicKey) ([]byte,
	error) {

	if operatorKey == nil {
		return nil, fmt.Errorf("operator key must be provided")
	}

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(operatorKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// oorVTXOPkScript computes the canonical v0 VTXO P2TR pkScript for the given
// owner/operator keys and exit delay.
func oorVTXOPkScript(t *testing.T, ownerKey *btcec.PublicKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) []byte {

	t.Helper()

	tapKey, err := arkscript.VTXOTapKey(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	require.NoError(t, err)

	return pkScript
}
