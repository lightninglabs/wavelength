package tree

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// SignTreeLocally runs the complete MuSig2 tree-signing ceremony for a
// tree whose cosigner keys are all held by one signer: one session per
// cosigner, per-transaction nonce aggregation, partial signatures, and
// final combination, submitted back onto the tree. The first cosigner
// key must participate in every node (the operator's tree-signing key);
// its sessions combine the partials.
//
// tweakLookup optionally overrides the taproot tweak per node; asset
// trees pass AssetTreeContext.TweakLookup(), Bitcoin-only trees nil.
func SignTreeLocally(t *Tree, signer input.MuSig2Signer,
	cosignerKeys []*keychain.KeyDescriptor,
	tweakLookup TaprootTweakLookup) error {

	if len(cosignerKeys) == 0 {
		return fmt.Errorf("at least one cosigner key is required")
	}

	sessions := make([]*SignerSession, 0, len(cosignerKeys))
	cleanup := func() {
		// Best effort: the ceremony error is the caller's signal,
		// stale sessions only linger in the signer's memory.
		for _, session := range sessions {
			_ = session.Cleanup()
		}
	}

	for _, key := range cosignerKeys {
		session, err := t.NewTreeSignerSession(
			signer, key, tweakLookup,
		)
		if err != nil {
			cleanup()

			return fmt.Errorf("session for cosigner %x: %w",
				key.PubKey.SerializeCompressed(), err)
		}
		sessions = append(sessions, session)
	}

	// The first session must span the whole tree, since it combines
	// every transaction's partials.
	combinerIDs := sessions[0].SessionIDs()
	if len(combinerIDs) != t.NumTx() {
		cleanup()

		return fmt.Errorf("first cosigner covers %d of %d tree "+
			"transactions; it must participate in every node",
			len(combinerIDs), t.NumTx())
	}

	// Phase 1: aggregate nonces per transaction across exactly the
	// sessions whose paths include it.
	noncesByTx := make(map[TxID][][musig2.PubNonceSize]byte)
	for _, session := range sessions {
		for txid, nonce := range session.GetNonces() {
			noncesByTx[txid] = append(noncesByTx[txid], nonce)
		}
	}
	aggByTx := make(map[TxID]Musig2PubNonce, len(noncesByTx))
	for txid, nonces := range noncesByTx {
		agg, err := musig2.AggregateNonces(nonces)
		if err != nil {
			cleanup()

			return fmt.Errorf("aggregate nonces for %s: %w", txid,
				err)
		}
		aggByTx[txid] = agg
	}
	for _, session := range sessions {
		scoped := make(map[TxID]Musig2PubNonce)
		for txid := range session.SessionIDs() {
			scoped[txid] = aggByTx[txid]
		}
		if err := session.RegisterAggNonces(scoped); err != nil {
			cleanup()

			return fmt.Errorf("register aggregated nonces: %w", err)
		}
	}

	// Phase 2: partial signatures from every session. The combiner's
	// sessions must outlive their partials so the signer can fold in the
	// other cosigners' shares below; every other session is done once it
	// has signed.
	partialsByTx := make(map[TxID][]*musig2.PartialSignature)
	for i, session := range sessions {
		partials, err := session.Signatures(i != 0)
		if err != nil {
			cleanup()

			return fmt.Errorf("partial signatures: %w", err)
		}
		if i == 0 {
			continue
		}
		for txid, partial := range partials {
			partialsByTx[txid] = append(
				partialsByTx[txid], partial,
			)
		}
	}

	finalSigs := make(map[TxID]*schnorr.Signature, len(combinerIDs))
	for txid, sessionID := range combinerIDs {
		sig, haveAll, err := signer.MuSig2CombineSig(
			sessionID, partialsByTx[txid],
		)
		if err != nil {
			cleanup()

			return fmt.Errorf("combine signatures for %s: %w", txid,
				err)
		}
		if !haveAll {
			cleanup()

			return fmt.Errorf("missing partial signatures for %s",
				txid)
		}
		finalSigs[txid] = sig
	}

	if err := t.SubmitTreeSigs(finalSigs); err != nil {
		cleanup()

		return fmt.Errorf("submit tree signatures: %w", err)
	}

	return t.VerifySigned()
}
