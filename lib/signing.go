package lib

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

type Musig2PubNonce [musig2.PubNonceSize]byte

// TxSignerCoordinator represents a signing session for a single VTX transaction.
type TxSignerCoordinator struct {
	// cosigners is the list of public keys of the cosigners involved in
	// signing this transaction's input.
	cosigners   map[string]*btcec.PublicKey
	sweepTweek  []byte
	keys        []*btcec.PublicKey
	nonces      map[string]Musig2PubNonce
	partialSigs map[string]*musig2.PartialSignature

	sigHash  [32]byte
	aggNonce Musig2PubNonce
	aggKey   *btcec.PublicKey
}

func NewTxSignerCoordinator(node *TreeNode, sweepTweek []byte,
	prevOutFetcher txscript.PrevOutputFetcher) (*TxSignerCoordinator, error) {

	cosignerMap := make(map[string]*btcec.PublicKey, len(node.CoSigners))
	for _, pk := range node.CoSigners {
		cosignerMap[toHexKey(pk)] = pk
	}

	tx, err := node.ToTx()
	if err != nil {
		return nil, err
	}

	aggKey, _, _, err := musig2.AggregateKeys(
		node.CoSigners, true, musig2.WithTaprootKeyTweak(sweepTweek),
	)
	if err != nil {
		return nil, err
	}

	sigHash, err := txscript.CalcTaprootSignatureHash(
		txscript.NewTxSigHashes(tx, prevOutFetcher),
		txscript.SigHashDefault, tx,
		0,
		prevOutFetcher,
	)
	if err != nil {
		return nil, err
	}

	return &TxSignerCoordinator{
		aggKey:      aggKey.FinalKey,
		sweepTweek:  sweepTweek,
		keys:        node.CoSigners,
		cosigners:   cosignerMap,
		nonces:      make(map[string]Musig2PubNonce),
		partialSigs: make(map[string]*musig2.PartialSignature),
		sigHash:     [32]byte(sigHash),
	}, nil
}

func toHexKey(pk *btcec.PublicKey) string {
	return hex.EncodeToString(pk.SerializeCompressed())
}

func (t *TxSignerCoordinator) AddNonce(signer *btcec.PublicKey,
	nonce Musig2PubNonce) error {

	if t.hasAllNonces() {
		return fmt.Errorf("all nonces have already been received")
	}

	if _, ok := t.cosigners[toHexKey(signer)]; !ok {
		return fmt.Errorf("signer not part of cosigners")
	}

	t.nonces[toHexKey(signer)] = nonce

	return nil
}

func (t *TxSignerCoordinator) HasAllNonces() bool {
	return t.hasAllNonces()
}

func (t *TxSignerCoordinator) hasAllNonces() bool {
	return len(t.nonces) == len(t.cosigners)
}

func (t *TxSignerCoordinator) fullySigned() bool {
	return len(t.partialSigs) == len(t.cosigners)
}

func (t *TxSignerCoordinator) GetNonces() []Musig2PubNonce {
	nonces := make([]Musig2PubNonce, 0, len(t.nonces))
	for _, nonce := range t.nonces {
		nonces = append(nonces, nonce)
	}

	return nonces
}

func (t *TxSignerCoordinator) AggregateNonces() (Musig2PubNonce, error) {
	if len(t.nonces) != len(t.cosigners) {
		return Musig2PubNonce{}, fmt.Errorf("not all nonces have " +
			"been received")
	}

	nonces := make([][musig2.PubNonceSize]byte, 0, len(t.nonces))
	for _, nonce := range t.nonces {
		nonces = append(nonces, nonce)
	}

	aggNonce, err := musig2.AggregateNonces(nonces)
	if err != nil {
		return Musig2PubNonce{}, fmt.Errorf("failed to aggregate "+
			"nonces: %w", err)
	}
	t.aggNonce = aggNonce

	return aggNonce, nil
}

func (t *TxSignerCoordinator) AddPartialSignature(signer *btcec.PublicKey,
	sig *musig2.PartialSignature) error {

	if _, ok := t.cosigners[toHexKey(signer)]; !ok {
		return fmt.Errorf("signer not part of cosigners")
	}

	// Cannot accept partial signature before nonces have been aggregated.
	if len(t.nonces) != len(t.cosigners) {
		return fmt.Errorf("not all nonces have been received")
	}

	// get this signer's nonce
	signerNonce, ok := t.nonces[toHexKey(signer)]
	if !ok {
		return fmt.Errorf("nonce for signer not found")
	}

	aggNonce, err := t.AggregateNonces()
	if err != nil {
		return fmt.Errorf("failed to aggregate nonces: %w", err)
	}

	valid := sig.Verify(
		signerNonce, aggNonce, t.keys, signer, t.sigHash,
		musig2.WithSortedKeys(),
		musig2.WithTaprootSignTweak(t.sweepTweek),
	)
	if !valid {
		return fmt.Errorf("partial signature failed to verify")
	}

	t.partialSigs[toHexKey(signer)] = sig

	return nil
}

func (t *TxSignerCoordinator) Sign() (*schnorr.Signature, error) {
	if len(t.partialSigs) != len(t.cosigners) {
		return nil, fmt.Errorf("not all partial signatures have " +
			"been received")
	}

	var (
		combinedNonce *btcec.PublicKey
		sigs          = make(
			[]*musig2.PartialSignature, 0, len(t.partialSigs),
		)
	)
	for _, sig := range t.partialSigs {
		sigs = append(sigs, sig)
		if combinedNonce == nil {
			combinedNonce = sig.R
			continue
		}

		if !combinedNonce.IsEqual(sig.R) {
			return nil, fmt.Errorf("partial signatures have " +
				"different nonces")
		}
	}

	combinedSig := musig2.CombineSigs(
		combinedNonce, sigs,
		musig2.WithTaprootTweakedCombine(
			t.sigHash, t.keys, t.sweepTweek, true,
		),
	)

	if !combinedSig.Verify(t.sigHash[:], t.aggKey) {
		return nil, fmt.Errorf("combined signature failed to verify")
	}

	return combinedSig, nil
}

type TreeSignerCoordinator struct {
	// txSigners is a map of TXIDs to their corresponding TxSigner.
	txSigners map[string]*TxSignerCoordinator
}

func NewTreeSignerCoordinator(tree *TreeNode, sweepTweek []byte,
	prevOuts txscript.PrevOutputFetcher) (*TreeSignerCoordinator, error) {

	// Initialize a TxSignerCoordinator for each transaction in the tree.
	signers := make(map[string]*TxSignerCoordinator, tree.NumTx())
	err := tree.ForEach(func(node *TreeNode) error {
		txid, err := node.TXID()
		if err != nil {
			return err
		}

		session, err := NewTxSignerCoordinator(node, sweepTweek, prevOuts)
		if err != nil {
			return err
		}

		signers[txid.String()] = session

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &TreeSignerCoordinator{
		txSigners: signers,
	}, nil
}

func (t *TreeSignerCoordinator) AddNonces(signer *btcec.PublicKey,
	nonces map[string]Musig2PubNonce) error {

	for txid, nonce := range nonces {
		txSigner, ok := t.txSigners[txid]
		if !ok {
			return fmt.Errorf("tx %s not found in coordinator", txid)
		}

		err := txSigner.AddNonce(signer, nonce)
		if err != nil {
			return fmt.Errorf("failed to add nonce for tx %s: %w",
				txid, err)
		}
	}

	return nil
}

func (t *TreeSignerCoordinator) HasAllNonces() bool {
	for _, txSigner := range t.txSigners {
		if !txSigner.HasAllNonces() {
			return false
		}
	}

	return true
}

func (t *TreeSignerCoordinator) GetAllNonces() (map[string][]Musig2PubNonce, error) {
	allNonces := make(map[string][]Musig2PubNonce, len(t.txSigners))
	for txid, txSigner := range t.txSigners {
		allNonces[txid] = txSigner.GetNonces()
	}

	return allNonces, nil

}

func (t *TreeSignerCoordinator) AddPartialSignatures(signer *btcec.PublicKey,
	sigs map[string]*musig2.PartialSignature) error {

	for txid, sig := range sigs {
		txSigner, ok := t.txSigners[txid]
		if !ok {
			return fmt.Errorf("tx %s not found in coordinator", txid)
		}

		err := txSigner.AddPartialSignature(signer, sig)
		if err != nil {
			return fmt.Errorf("failed to add partial signature for tx %s: %w",
				txid, err)
		}
	}

	return nil
}

func (t *TreeSignerCoordinator) FullySigned() bool {
	for _, txSigner := range t.txSigners {
		if !txSigner.fullySigned() {
			return false
		}
	}

	return true
}

func (t *TreeSignerCoordinator) Signatures() (map[string]*schnorr.Signature, error) {
	sigs := make(map[string]*schnorr.Signature, len(t.txSigners))
	for txid, txSigner := range t.txSigners {
		sig, err := txSigner.Sign()
		if err != nil {
			return nil, fmt.Errorf("failed to sign tx %s: %w",
				txid, err)
		}

		sigs[txid] = sig
	}

	return sigs, nil
}

type TxSignerSession struct {
	signer      input.MuSig2Signer
	signSession *input.MuSig2SessionInfo

	sigHash [32]byte
}

func NewTxSignerSession(signer input.MuSig2Signer,
	tweak []byte,
	cosigners []*btcec.PublicKey,
	signerKey *keychain.KeyDescriptor,
	tx *wire.MsgTx,
	fetcher txscript.PrevOutputFetcher) (*TxSignerSession, error) {

	message, err := txscript.CalcTaprootSignatureHash(
		txscript.NewTxSigHashes(tx, fetcher),
		txscript.SigHashDefault,
		tx, 0,
		fetcher,
	)
	if err != nil {
		return nil, err
	}

	musigSession, err := signer.MuSig2CreateSession(
		input.MuSig2Version100RC2,
		signerKey.KeyLocator,
		cosigners,
		&input.MuSig2Tweaks{
			TaprootTweak: tweak,
		},
		nil,
		nil,
	)
	if err != nil {
		return nil, err
	}

	return &TxSignerSession{
		signer:      signer,
		signSession: musigSession,
		sigHash:     [32]byte(message),
	}, nil
}

func (s *TxSignerSession) GetNonce() (Musig2PubNonce, error) {
	return s.signSession.PublicNonce, nil
}

func (s *TxSignerSession) RegisterNonces(nonces [][66]byte) error {
	// filter out this signer's nonce from the list
	var filteredNonces [][66]byte
	for _, n := range nonces {
		if n == s.signSession.PublicNonce {
			continue
		}
		filteredNonces = append(filteredNonces, n)
	}

	ok, err := s.signer.MuSig2RegisterNonces(
		s.signSession.SessionID, filteredNonces,
	)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("not all nonces registered successfully")
	}

	return nil
}

// TODO: need to update the musig2 lib  & input lib to allow setting aggregate
// nonce instead of having to pass in all nonces in.
//func (s *TxSignerSession) SetAggregateNonce(nonce *Musig2PubNonce) error {
//}

func (s *TxSignerSession) Sign() (*musig2.PartialSignature, error) {
	return s.signer.MuSig2Sign(
		s.signSession.SessionID, s.sigHash, true,
	)
}

type TreeSignerSession struct {
	// signerKey is the key descriptor for the signer.
	signerKey *keychain.KeyDescriptor

	txs map[string]*TxSignerSession
}

func NewTreeSignerSession(signer input.MuSig2Signer,
	signerKey *keychain.KeyDescriptor,
	tweak []byte, prevOuts txscript.PrevOutputFetcher,
	tree *TreeNode) (*TreeSignerSession, error) {

	// Extract the path for this cosigner only.
	signerPath := tree.ExtractPathForCosigner(signerKey.PubKey)

	// Get this signer's transactions.
	txs := make(map[string]*TxSignerSession)
	err := signerPath.ForEach(func(node *TreeNode) error {
		tx, err := node.ToTx()
		if err != nil {
			return err
		}

		signer, err := NewTxSignerSession(
			signer, tweak, node.CoSigners, signerKey, tx,
			prevOuts,
		)
		if err != nil {
			return err
		}

		txs[tx.TxHash().String()] = signer
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &TreeSignerSession{
		signerKey: signerKey,
		txs:       txs,
	}, nil
}

func (s *TreeSignerSession) PubKey() *btcec.PublicKey {
	return s.signerKey.PubKey
}

func (s *TreeSignerSession) GetNonces() (map[string]Musig2PubNonce, error) {
	nonces := make(map[string]Musig2PubNonce, len(s.txs))
	for txid, txSigner := range s.txs {
		nonce, err := txSigner.GetNonce()
		if err != nil {
			return nil, fmt.Errorf("failed to get nonce for tx %s: %w",
				txid, err)
		}

		nonces[txid] = nonce
	}

	return nonces, nil
}

// TODO: need to update musig2 lib & input lib to allow setting aggregate nonce.
// in the mean time, this expects a map of txid to all the nonces for this tx.
func (s *TreeSignerSession) RegisterNonces(noncesSet map[string][]Musig2PubNonce) error {
	for txid, txSigner := range s.txs {
		nonces, ok := noncesSet[txid]
		if !ok {
			return fmt.Errorf("nonce for tx %s not found", txid)
		}

		ns := make([][66]byte, 0, len(nonces))
		for _, n := range nonces {
			ns = append(ns, n)
		}

		err := txSigner.RegisterNonces(ns)
		if err != nil {
			return fmt.Errorf("failed to register nonce for tx %s: %w",
				txid, err)
		}
	}

	return nil
}

func (s *TreeSignerSession) Signatures() (map[string]*musig2.PartialSignature,
	error) {

	sigs := make(map[string]*musig2.PartialSignature, len(s.txs))
	for txid, txSigner := range s.txs {
		sig, err := txSigner.Sign()
		if err != nil {
			return nil, fmt.Errorf("failed to sign tx %s: %w",
				txid, err)
		}

		sigs[txid] = sig
	}

	return sigs, nil
}
