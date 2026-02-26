package roundwire

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightningnetwork/lnd/keychain"
	"google.golang.org/protobuf/proto"
)

const (
	// ServiceName is the mailbox RPC service name for round state-machine
	// EVENT envelopes exchanged between client round actor and
	// server rounds actor.
	ServiceName = "roundwire.RoundMailboxService"

	// MethodJoinRoundRequest maps a client JoinRound request.
	MethodJoinRoundRequest = "JoinRoundRequest"

	// MethodSubmitNoncesRequest maps a client nonce submission.
	MethodSubmitNoncesRequest = "SubmitNoncesRequest"

	// MethodSubmitPartialSigRequest maps a client partial signature
	// submission.
	MethodSubmitPartialSigRequest = "SubmitPartialSigRequest"

	// MethodSubmitForfeitSigRequest maps a client boarding input signature
	// submission.
	MethodSubmitForfeitSigRequest = "SubmitForfeitSigRequest"

	// MethodSubmitVTXOForfeitSigsRequest maps a client VTXO
	// forfeit signature submission.
	MethodSubmitVTXOForfeitSigsRequest = "SubmitVTXOForfeitSigsRequest"

	// MethodClientErrorResp maps a server error response to a client.
	MethodClientErrorResp = "ClientErrorResp"

	// MethodClientSuccessResp maps a server successful join response.
	MethodClientSuccessResp = "ClientSuccessResp"

	// MethodClientAwaitingInputSigsResp maps a server "awaiting signatures"
	// response.
	MethodClientAwaitingInputSigsResp = "ClientAwaitingInputSigsResp"

	// MethodClientVTXOAggNonces maps a server aggregated nonce response.
	MethodClientVTXOAggNonces = "ClientVTXOAggNonces"

	// MethodClientVTXOAggSigs maps a server aggregated signature response.
	MethodClientVTXOAggSigs = "ClientVTXOAggSigs"

	// MethodClientBatchInfo maps a server batch information response.
	MethodClientBatchInfo = "ClientBatchInfo"

	// MethodClientRoundFailedResp maps a server round failure response.
	MethodClientRoundFailedResp = "ClientRoundFailedResp"
)

// WrapPayload returns the payload as-is for proto-native mailbox transport.
func WrapPayload(payload proto.Message) (proto.Message, error) {
	if payload == nil {
		return nil, fmt.Errorf("payload is required")
	}

	return payload, nil
}

// UnwrapPayload marshals a proto payload into raw bytes.
func UnwrapPayload(msg proto.Message) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("payload is required")
	}

	return proto.Marshal(msg)
}

// DecodePayload unmarshals raw proto payload bytes into dst.
func DecodePayload(raw []byte, dst proto.Message) error {
	if dst == nil {
		return fmt.Errorf("destination payload is required")
	}

	return (proto.UnmarshalOptions{
		DiscardUnknown: true,
	}).Unmarshal(raw, dst)
}

// EncodeOutPoint converts a wire.OutPoint into its payload representation.
func EncodeOutPoint(op wire.OutPoint) OutPointPayload {
	return OutPointPayload{
		TxId: op.Hash.String(),
		Vout: op.Index,
	}
}

// DecodeOutPoint converts a payload outpoint into wire.OutPoint.
func DecodeOutPoint(op *OutPointPayload) (wire.OutPoint, error) {
	if op == nil {
		return wire.OutPoint{}, fmt.Errorf(
			"outpoint payload is required",
		)
	}

	h, err := chainhash.NewHashFromStr(op.TxId)
	if err != nil {
		return wire.OutPoint{}, err
	}

	return wire.OutPoint{
		Hash:  *h,
		Index: op.Vout,
	}, nil
}

// EncodeTxOut converts a wire.TxOut into its payload representation.
func EncodeTxOut(out *wire.TxOut) TxOutPayload {
	if out == nil {
		return TxOutPayload{}
	}

	return TxOutPayload{
		Value:    out.Value,
		PkScript: hex.EncodeToString(out.PkScript),
	}
}

// DecodeTxOut converts a payload txout into wire.TxOut.
func DecodeTxOut(out *TxOutPayload) (*wire.TxOut, error) {
	if out == nil {
		return nil, fmt.Errorf("txout payload is required")
	}

	pkScript, err := hex.DecodeString(out.PkScript)
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value:    out.Value,
		PkScript: pkScript,
	}, nil
}

// EncodePubKey encodes a compressed public key as hex string.
func EncodePubKey(pk *btcec.PublicKey) string {
	if pk == nil {
		return ""
	}

	return hex.EncodeToString(pk.SerializeCompressed())
}

// DecodePubKey decodes a compressed public key hex string.
func DecodePubKey(pkHex string) (*btcec.PublicKey, error) {
	if pkHex == "" {
		return nil, nil
	}

	raw, err := hex.DecodeString(pkHex)
	if err != nil {
		return nil, err
	}

	return btcec.ParsePubKey(raw)
}

// EncodeKeyDescriptor converts a key descriptor into payload form.
func EncodeKeyDescriptor(desc keychain.KeyDescriptor) KeyDescriptorPayload {
	return KeyDescriptorPayload{
		KeyFamily: int32(desc.KeyLocator.Family),
		KeyIndex:  desc.KeyLocator.Index,
		PubKeyHex: EncodePubKey(desc.PubKey),
	}
}

// DecodeKeyDescriptor converts payload key descriptor to keychain descriptor.
func DecodeKeyDescriptor(
	desc *KeyDescriptorPayload,
) (keychain.KeyDescriptor, error) {

	if desc == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"key descriptor payload is required",
		)
	}

	pubKey, err := DecodePubKey(desc.PubKeyHex)
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}

	return keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(desc.KeyFamily),
			Index:  desc.KeyIndex,
		},
		PubKey: pubKey,
	}, nil
}

// EncodeNonce encodes a MuSig2 nonce into hex.
func EncodeNonce(n tree.Musig2PubNonce) string {
	return hex.EncodeToString(n[:])
}

// DecodeNonce decodes a MuSig2 nonce from hex.
func DecodeNonce(nonceHex string) (tree.Musig2PubNonce, error) {
	var nonce tree.Musig2PubNonce

	raw, err := hex.DecodeString(nonceHex)
	if err != nil {
		return nonce, err
	}

	if len(raw) != len(nonce) {
		return nonce, fmt.Errorf("invalid nonce length: got %d want %d",
			len(raw), len(nonce))
	}

	copy(nonce[:], raw)

	return nonce, nil
}

// EncodePartialSignature encodes a MuSig2 partial signature to hex.
func EncodePartialSignature(sig *musig2.PartialSignature) (string, error) {
	if sig == nil {
		return "", nil
	}

	var buf bytes.Buffer
	if err := sig.Encode(&buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// DecodePartialSignature decodes a MuSig2 partial signature from hex.
func DecodePartialSignature(sigHex string) (*musig2.PartialSignature, error) {
	if sigHex == "" {
		return nil, nil
	}

	raw, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, err
	}

	sig := &musig2.PartialSignature{}
	if err := sig.Decode(bytes.NewReader(raw)); err != nil {
		return nil, err
	}

	return sig, nil
}

// EncodeSchnorrSignature encodes a schnorr signature to hex.
func EncodeSchnorrSignature(sig *schnorr.Signature) string {
	if sig == nil {
		return ""
	}

	return hex.EncodeToString(sig.Serialize())
}

// DecodeSchnorrSignature decodes a schnorr signature from hex.
func DecodeSchnorrSignature(sigHex string) (*schnorr.Signature, error) {
	if sigHex == "" {
		return nil, nil
	}

	raw, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, err
	}

	return schnorr.ParseSignature(raw)
}

// EncodeMsgTx serializes a wire.MsgTx to hex.
func EncodeMsgTx(tx *wire.MsgTx) (string, error) {
	if tx == nil {
		return "", nil
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// DecodeMsgTx deserializes a wire.MsgTx from hex.
func DecodeMsgTx(txHex string) (*wire.MsgTx, error) {
	if txHex == "" {
		return nil, nil
	}

	raw, err := hex.DecodeString(txHex)
	if err != nil {
		return nil, err
	}

	tx := &wire.MsgTx{}
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}

	return tx, nil
}

// EncodePSBT serializes a psbt.Packet to hex.
func EncodePSBT(packet *psbt.Packet) (string, error) {
	if packet == nil {
		return "", nil
	}

	var buf bytes.Buffer
	if err := packet.Serialize(&buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf.Bytes()), nil
}

// DecodePSBT deserializes a psbt.Packet from hex.
func DecodePSBT(psbtHex string) (*psbt.Packet, error) {
	if psbtHex == "" {
		return nil, nil
	}

	raw, err := hex.DecodeString(psbtHex)
	if err != nil {
		return nil, err
	}

	return psbt.NewFromRawBytes(bytes.NewReader(raw), false)
}

// EncodeTree converts a tree.Tree into its payload representation.
func EncodeTree(t *tree.Tree) (*TreePayload, error) {
	if t == nil {
		return nil, nil
	}

	root, err := encodeNode(t.Root)
	if err != nil {
		return nil, err
	}

	var batchOutput *TxOutPayload
	if t.BatchOutput != nil {
		encoded := EncodeTxOut(t.BatchOutput)
		batchOutput = &encoded
	}

	batchOutpoint := EncodeOutPoint(t.BatchOutpoint)

	return &TreePayload{
		Root: root,
		BatchOutpoint: &OutPointPayload{
			TxId: batchOutpoint.TxId,
			Vout: batchOutpoint.Vout,
		},
		BatchOutput: batchOutput,
		SweepTapscriptRoot: hex.EncodeToString(
			t.SweepTapscriptRoot,
		),
	}, nil
}

// DecodeTree converts a tree payload into a tree.Tree.
func DecodeTree(payload *TreePayload) (*tree.Tree, error) {
	if payload == nil {
		return nil, nil
	}

	root, err := decodeNode(payload.Root)
	if err != nil {
		return nil, err
	}

	if payload.BatchOutpoint == nil {
		return nil, fmt.Errorf("batch outpoint is required")
	}

	batchOutpoint, err := DecodeOutPoint(payload.BatchOutpoint)
	if err != nil {
		return nil, err
	}

	var batchOutput *wire.TxOut
	if payload.BatchOutput != nil {
		batchOutput, err = DecodeTxOut(payload.BatchOutput)
		if err != nil {
			return nil, err
		}
	}

	sweepRoot, err := hex.DecodeString(payload.SweepTapscriptRoot)
	if err != nil {
		return nil, err
	}

	return &tree.Tree{
		Root:               root,
		BatchOutpoint:      batchOutpoint,
		BatchOutput:        batchOutput,
		SweepTapscriptRoot: sweepRoot,
	}, nil
}

func encodeNode(n *tree.Node) (*NodePayload, error) {
	if n == nil {
		return nil, nil
	}

	outs := make([]*TxOutPayload, 0, len(n.Outputs))
	for _, out := range n.Outputs {
		encoded := EncodeTxOut(out)
		outs = append(outs, &encoded)
	}

	cosigners := make([]string, 0, len(n.CoSigners))
	for _, signer := range n.CoSigners {
		cosigners = append(cosigners, EncodePubKey(signer))
	}

	children := make([]*NodeChildPayload, 0, len(n.Children))
	if len(n.Children) > 0 {
		indices := make([]int, 0, len(n.Children))
		for idx := range n.Children {
			indices = append(indices, int(idx))
		}
		sort.Ints(indices)

		for _, idx := range indices {
			child := n.Children[uint32(idx)]
			encoded, err := encodeNode(child)
			if err != nil {
				return nil, err
			}

			children = append(children, &NodeChildPayload{
				Index: uint32(idx),
				Node:  encoded,
			})
		}
	}

	input := EncodeOutPoint(n.Input)

	return &NodePayload{
		Input:        &input,
		Outputs:      outs,
		CoSigners:    cosigners,
		Children:     children,
		Amount:       int64(n.Amount),
		SignatureHex: EncodeSchnorrSignature(n.Signature),
		FinalKeyHex:  EncodePubKey(n.FinalKey),
	}, nil
}

func decodeNode(p *NodePayload) (*tree.Node, error) {
	if p == nil {
		return nil, nil
	}

	if p.Input == nil {
		return nil, fmt.Errorf("node input is required")
	}

	input, err := DecodeOutPoint(p.Input)
	if err != nil {
		return nil, err
	}

	outputs := make([]*wire.TxOut, 0, len(p.Outputs))
	for _, out := range p.Outputs {
		if out == nil {
			continue
		}

		decoded, err := DecodeTxOut(out)
		if err != nil {
			return nil, err
		}

		outputs = append(outputs, decoded)
	}

	cosigners := make([]*btcec.PublicKey, 0, len(p.CoSigners))
	for _, signerHex := range p.CoSigners {
		signer, err := DecodePubKey(signerHex)
		if err != nil {
			return nil, err
		}

		cosigners = append(cosigners, signer)
	}

	children := make(map[uint32]*tree.Node, len(p.Children))
	for _, child := range p.Children {
		decoded, err := decodeNode(child.Node)
		if err != nil {
			return nil, err
		}

		children[child.Index] = decoded
	}

	sig, err := DecodeSchnorrSignature(p.SignatureHex)
	if err != nil {
		return nil, err
	}

	finalKey, err := DecodePubKey(p.FinalKeyHex)
	if err != nil {
		return nil, err
	}

	return &tree.Node{
		Input:     input,
		Outputs:   outputs,
		CoSigners: cosigners,
		Children:  children,
		Amount:    btcutil.Amount(p.Amount),
		Signature: sig,
		FinalKey:  finalKey,
	}, nil
}

// SortSignerNonceBundles sorts signer bundles and per-signer nonce entries.
func SortSignerNonceBundles(entries []*SignerNonceBundle) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SignerKeyHex < entries[j].SignerKeyHex
	})

	for i := range entries {
		sort.Slice(entries[i].Nonces, func(a, b int) bool {
			return entries[i].Nonces[a].TxIdHex <
				entries[i].Nonces[b].TxIdHex
		})
	}
}

// SortSignerSigBundles sorts signer bundles and per-signer signature entries.
func SortSignerSigBundles(entries []*SignerSigBundle) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].SignerKeyHex < entries[j].SignerKeyHex
	})

	for i := range entries {
		sort.Slice(entries[i].Signatures, func(a, b int) bool {
			return entries[i].Signatures[a].TxIdHex <
				entries[i].Signatures[b].TxIdHex
		})
	}
}

// SortTxNonceEntries sorts tx nonce entries by txid string.
func SortTxNonceEntries(entries []*TxNonceEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TxIdHex < entries[j].TxIdHex
	})
}

// SortTxSigEntries sorts tx signature entries by txid string.
func SortTxSigEntries(entries []*TxSigEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TxIdHex < entries[j].TxIdHex
	})
}

// ParseIndexKey parses a map key encoded with strconv.Itoa.
func ParseIndexKey(k string) (int, error) {
	return strconv.Atoi(k)
}
