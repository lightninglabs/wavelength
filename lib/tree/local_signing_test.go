package tree

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// multiKeyMuSig2Signer models a wallet holding every cosigner key: one
// mockMuSig2Signer per key, session creation dispatched on the key
// locator index and everything else on the session owner recorded at
// creation.
type multiKeyMuSig2Signer struct {
	byIndex map[uint32]*mockMuSig2Signer
	owner   map[input.MuSig2SessionID]*mockMuSig2Signer
}

// newMultiKeyMuSig2Signer wires one single-key mock per private key; each
// inner mock gets a disjoint session ID range so ownership stays
// unambiguous.
func newMultiKeyMuSig2Signer(
	privKeys []*btcec.PrivateKey) *multiKeyMuSig2Signer {

	signer := &multiKeyMuSig2Signer{
		byIndex: make(map[uint32]*mockMuSig2Signer),
		owner:   make(map[input.MuSig2SessionID]*mockMuSig2Signer),
	}
	for i, privKey := range privKeys {
		inner := newMockMuSig2Signer(privKey)
		inner.nextSessionID = i * 32
		signer.byIndex[uint32(i)] = inner
	}

	return signer
}

// MuSig2CreateSession dispatches to the key named by the locator index.
func (m *multiKeyMuSig2Signer) MuSig2CreateSession(
	version input.MuSig2Version, keyLoc keychain.KeyLocator,
	signers []*btcec.PublicKey, tweaks *input.MuSig2Tweaks,
	otherNonces [][musig2.PubNonceSize]byte,
	localNonces *musig2.Nonces) (*input.MuSig2SessionInfo, error) {

	inner, ok := m.byIndex[keyLoc.Index]
	if !ok {
		return nil, fmt.Errorf("no key at locator index %d",
			keyLoc.Index)
	}

	info, err := inner.MuSig2CreateSession(
		version, keyLoc, signers, tweaks, otherNonces, localNonces,
	)
	if err != nil {
		return nil, err
	}
	m.owner[info.SessionID] = inner

	return info, nil
}

// sessionOwner resolves the inner mock a session was created on.
func (m *multiKeyMuSig2Signer) sessionOwner(
	sessionID input.MuSig2SessionID) (*mockMuSig2Signer, error) {

	inner, ok := m.owner[sessionID]
	if !ok {
		return nil, fmt.Errorf("unknown session %x", sessionID)
	}

	return inner, nil
}

// MuSig2RegisterNonces delegates to the session's owner.
func (m *multiKeyMuSig2Signer) MuSig2RegisterNonces(
	sessionID input.MuSig2SessionID,
	nonces [][musig2.PubNonceSize]byte) (bool, error) {

	inner, err := m.sessionOwner(sessionID)
	if err != nil {
		return false, err
	}

	return inner.MuSig2RegisterNonces(sessionID, nonces)
}

// MuSig2RegisterCombinedNonce delegates to the session's owner.
func (m *multiKeyMuSig2Signer) MuSig2RegisterCombinedNonce(
	sessionID input.MuSig2SessionID,
	aggNonce [musig2.PubNonceSize]byte) error {

	inner, err := m.sessionOwner(sessionID)
	if err != nil {
		return err
	}

	return inner.MuSig2RegisterCombinedNonce(sessionID, aggNonce)
}

// MuSig2GetCombinedNonce delegates to the session's owner.
func (m *multiKeyMuSig2Signer) MuSig2GetCombinedNonce(
	sessionID input.MuSig2SessionID) ([musig2.PubNonceSize]byte, error) {

	inner, err := m.sessionOwner(sessionID)
	if err != nil {
		return [musig2.PubNonceSize]byte{}, err
	}

	return inner.MuSig2GetCombinedNonce(sessionID)
}

// MuSig2Sign delegates to the session's owner.
func (m *multiKeyMuSig2Signer) MuSig2Sign(sessionID input.MuSig2SessionID,
	msg [32]byte, cleanup bool) (*musig2.PartialSignature, error) {

	inner, err := m.sessionOwner(sessionID)
	if err != nil {
		return nil, err
	}

	return inner.MuSig2Sign(sessionID, msg, cleanup)
}

// MuSig2CombineSig delegates to the session's owner.
func (m *multiKeyMuSig2Signer) MuSig2CombineSig(
	sessionID input.MuSig2SessionID,
	partialSigs []*musig2.PartialSignature) (*schnorr.Signature, bool,
	error) {

	inner, err := m.sessionOwner(sessionID)
	if err != nil {
		return nil, false, err
	}

	return inner.MuSig2CombineSig(sessionID, partialSigs)
}

// MuSig2Cleanup delegates to the session's owner.
func (m *multiKeyMuSig2Signer) MuSig2Cleanup(
	sessionID input.MuSig2SessionID) error {

	inner, err := m.sessionOwner(sessionID)
	if err != nil {
		return err
	}

	return inner.MuSig2Cleanup(sessionID)
}

// localSigningFixture builds a four-leaf VTXO tree plus a wallet holding the
// operator and every leaf owner key, returning the descriptors in ceremony
// order (operator first).
func localSigningFixture(t *testing.T) (*Tree, input.MuSig2Signer,
	[]*keychain.KeyDescriptor) {

	t.Helper()

	const numLeaves = 4

	privKeys := make([]*btcec.PrivateKey, 0, numLeaves+1)
	keyDescs := make([]*keychain.KeyDescriptor, 0, numLeaves+1)
	for i := 0; i < numLeaves+1; i++ {
		privKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		privKeys = append(privKeys, privKey)
		keyDescs = append(keyDescs, &keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 1,
				Index:  uint32(i),
			},
			PubKey: privKey.PubKey(),
		})
	}
	operatorKey := keyDescs[0].PubKey

	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	vtxos := make([]VTXODescriptor, 0, numLeaves)
	for _, ownerDesc := range keyDescs[1:] {
		pkScript, err := txscript.PayToTaprootScript(ownerDesc.PubKey)
		require.NoError(t, err)
		vtxos = append(vtxos, VTXODescriptor{
			PkScript:    pkScript,
			Amount:      btcutil.Amount(5_000),
			CoSignerKey: ownerDesc.PubKey,
		})
	}

	batchOutpoint := wire.OutPoint{
		Hash: chainhash.HashH([]byte("local_signing_commitment")),
	}
	batchOutput := &wire.TxOut{
		Value:    int64(numLeaves) * 5_000,
		PkScript: []byte("batch_script"),
	}

	built, err := BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos, operatorKey,
		sweepKey.PubKey(), 144, 2,
	)
	require.NoError(t, err)

	return built, newMultiKeyMuSig2Signer(privKeys), keyDescs
}

// TestSignTreeLocally runs the complete single-wallet ceremony over a
// four-leaf tree and expects a fully verified tree.
func TestSignTreeLocally(t *testing.T) {
	t.Parallel()

	built, signer, keyDescs := localSigningFixture(t)

	require.NoError(t, SignTreeLocally(built, signer, keyDescs, nil))

	// Every node ends up carrying a verified final signature.
	require.NoError(t, built.VerifySigned())
}

// TestSignTreeLocallyRequiresTreeWideCombiner rejects a ceremony whose first
// cosigner does not participate in every node.
func TestSignTreeLocallyRequiresTreeWideCombiner(t *testing.T) {
	t.Parallel()

	built, signer, keyDescs := localSigningFixture(t)

	// A leaf owner only covers its own path, so it cannot combine.
	reordered := []*keychain.KeyDescriptor{
		keyDescs[1], keyDescs[0], keyDescs[2], keyDescs[3],
		keyDescs[4],
	}
	err := SignTreeLocally(built, signer, reordered, nil)
	require.ErrorContains(t, err, "must participate in every node")
}

// TestSignTreeLocallyRequiresCosigners rejects an empty cosigner set.
func TestSignTreeLocallyRequiresCosigners(t *testing.T) {
	t.Parallel()

	built, signer, _ := localSigningFixture(t)

	err := SignTreeLocally(built, signer, nil, nil)
	require.ErrorContains(t, err, "at least one cosigner key")
}
