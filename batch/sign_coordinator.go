package batch

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btclog/v2"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// TxID is an alias for treepkg.TxID (chainhash.Hash), used as a key in maps.
type TxID = treepkg.TxID

// TxSignerCoordinator coordinates MuSig2 signing for a single transaction.
// It embeds the operator's signing session and collects nonces and partial
// signatures from other cosigners, producing the final schnorr signature.
type TxSignerCoordinator struct {
	// signer is the MuSig2 signer interface.
	signer input.MuSig2Signer

	// signingKey is the operator's key for this musig session.
	signingKey *btcec.PublicKey

	// sessionInfo is the operator's MuSig2 session info for this tx.
	sessionInfo *input.MuSig2SessionInfo

	// sigHash is the data that will be signed.
	sigHash [32]byte

	// cosigners is the map of expected cosigners (hex key -> pubkey).
	cosigners map[string]*btcec.PublicKey

	// finalSig stores the final combined signature once all partial
	// signatures have been received and combined.
	finalSig *schnorr.Signature
}

// NewTxSignerCoordinator creates a new coordinator for signing a single
// transaction. It creates the operator's MuSig2 session internally for
// automatic operator nonce and signature management.
func NewTxSignerCoordinator(operatorSigner input.MuSig2Signer,
	operatorKey *keychain.KeyDescriptor, node *treepkg.Node,
	sweepTapscriptRoot []byte,
	prevOutFetcher txscript.PrevOutputFetcher) (*TxSignerCoordinator,
	error) {

	// Build map of expected cosigners.
	cosignerMap := make(map[string]*btcec.PublicKey, len(node.CoSigners))
	for _, pk := range node.CoSigners {
		cosignerMap[toHexKey(pk)] = pk
	}

	// Ensure that the cosigners were unique.
	if len(cosignerMap) != len(node.CoSigners) {
		return nil, fmt.Errorf("duplicate cosigner public keys found")
	}

	// Verify operator key is in the cosigner set.
	if _, ok := cosignerMap[toHexKey(operatorKey.PubKey)]; !ok {
		return nil, fmt.Errorf("operator key not found in cosigners")
	}

	sigHash, err := node.SigHash(prevOutFetcher)
	if err != nil {
		return nil, fmt.Errorf("failed to compute sighash: %w", err)
	}

	signerSess, err := node.NewSignerSession(
		operatorKey, operatorSigner, sweepTapscriptRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create operator sign "+
			"session: %w", err)
	}

	return &TxSignerCoordinator{
		signer:      operatorSigner,
		signingKey:  operatorKey.PubKey,
		sessionInfo: signerSess,
		sigHash:     [32]byte(sigHash),
		cosigners:   cosignerMap,
	}, nil
}

// toHexKey converts a public key to its hex string representation for use as
// a map key.
func toHexKey(pk *btcec.PublicKey) string {
	return hex.EncodeToString(pk.SerializeCompressed())
}

// sessionID returns the MuSig2 session ID for this transaction.
func (c *TxSignerCoordinator) sessionID() [32]byte {
	return c.sessionInfo.SessionID
}

// AddNonce adds a nonce from a non-operator cosigner and registers it with
// the MuSig2 session. Returns an error if the signer is the operator or not
// part of expected cosigners.
func (c *TxSignerCoordinator) AddNonce(signer *btcec.PublicKey,
	nonce treepkg.Musig2PubNonce) error {

	hexKey := toHexKey(signer)

	// Reject operator nonces (operator's nonce is automatic).
	if signer.IsEqual(c.signingKey) {
		return fmt.Errorf("operator nonce is managed automatically")
	}

	if _, ok := c.cosigners[hexKey]; !ok {
		return fmt.Errorf("signer %x not part of expected cosigners",
			signer.SerializeCompressed())
	}

	// Register nonce with the MuSig2 session. The session handles
	// tracking and duplicate detection. The return value indicates whether
	// all nonces have now been registered.
	haveAll, err := c.signer.MuSig2RegisterNonces(
		c.sessionID(), [][musig2.PubNonceSize]byte{nonce},
	)
	if err != nil {
		return fmt.Errorf("failed to register nonce: %w", err)
	}

	// Update our tracking of whether all nonces have been received.
	c.sessionInfo.HaveAllNonces = haveAll

	return nil
}

// GetNonce returns the operator's public nonce for this transaction's
// MuSig2 session.
func (c *TxSignerCoordinator) GetNonce() treepkg.Musig2PubNonce {
	return c.sessionInfo.PublicNonce
}

// HasAllNonces returns true if nonces have been received from all non-operator
// cosigners. The operator's nonce is managed automatically. This field is
// updated based on the return value from MuSig2RegisterNonces.
func (c *TxSignerCoordinator) HasAllNonces() bool {
	return c.sessionInfo.HaveAllNonces
}

// AggregateNonces returns the aggregated nonce from the MuSig2 session.
// Returns an error if not all non-operator nonces have been received yet.
func (c *TxSignerCoordinator) AggregateNonces() (
	treepkg.Musig2PubNonce, error) {

	if !c.HasAllNonces() {
		return treepkg.Musig2PubNonce{}, fmt.Errorf("not all nonces " +
			"have been received")
	}

	// Get the combined nonce from the session. The session automatically
	// aggregates nonces as they're registered.
	aggNonce, err := c.signer.MuSig2GetCombinedNonce(c.sessionID())
	if err != nil {
		return treepkg.Musig2PubNonce{}, fmt.Errorf("failed to get "+
			"combined nonce: %w", err)
	}

	return aggNonce, nil
}

// AddPartialSignature adds a partial signature from a non-operator cosigner
// and combines it with the MuSig2 session. The operator's signature is managed
// automatically via the embedded session.
func (c *TxSignerCoordinator) AddPartialSignature(signer *btcec.PublicKey,
	sig *musig2.PartialSignature) error {

	hexKey := toHexKey(signer)

	// Reject operator signatures (operator signs via session).
	if signer.IsEqual(c.signingKey) {
		return fmt.Errorf("operator signature is managed automatically")
	}

	if _, ok := c.cosigners[hexKey]; !ok {
		return fmt.Errorf("signer %x not part of expected cosigners",
			signer.SerializeCompressed())
	}

	// Cannot accept partial signature before nonces have been received.
	if !c.HasAllNonces() {
		return fmt.Errorf("not all nonces have been received")
	}

	// Combine this signature with the session. The session handles
	// tracking and validation. If this is the last signature, the final
	// combined signature is returned.
	finalSig, complete, err := c.signer.MuSig2CombineSig(
		c.sessionID(), []*musig2.PartialSignature{sig},
	)
	if err != nil {
		return fmt.Errorf("failed to combine signature: %w", err)
	}

	// Store the final signature if all signatures have been collected.
	if complete {
		if finalSig == nil {
			return fmt.Errorf("final signature should not be " +
				"nil when the musig session is complete")
		}

		c.finalSig = finalSig
	}

	return nil
}

// FullySigned returns true if partial signatures have been received from all
// non-operator cosigners and the final signature has been computed. The
// operator's signature is managed automatically.
func (c *TxSignerCoordinator) FullySigned() bool {
	return c.finalSig != nil
}

// Sign generates the operator's partial signature for this
// transaction using the embedded session. This should be called after all
// non-operator nonces have been received and aggregated.
func (c *TxSignerCoordinator) Sign() error {
	// Generate operator's partial signature using the session.
	// Don't clean up yet since we need to combine with client signatures.
	_, err := c.signer.MuSig2Sign(c.sessionID(), c.sigHash, false)

	return err
}

// AggregateSig returns the final combined schnorr signature. The signature is
// computed and stored when the last partial signature is added via
// AddPartialSignature. Returns an error if not all partial signatures have
// been received.
//
// Note: This method does NOT verify the signature cryptographically. Callers
// should use treepkg.VerifySigned() after storing signatures to validate them
// properly within the treepkg context.
func (c *TxSignerCoordinator) AggregateSig() (*schnorr.Signature, error) {
	if !c.FullySigned() {
		return nil, fmt.Errorf("not all partial signatures received")
	}

	// Clean up the session.
	_ = c.signer.MuSig2Cleanup(c.sessionID())

	return c.finalSig, nil
}

// TreeSignCoordinator coordinates MuSig2 signing for all transactions in a
// tree. Each TxSignerCoordinator manages its own operator MuSig2 session.
type TreeSignCoordinator struct {
	// signer is the MuSig2 signer for the operator.
	signer input.MuSig2Signer

	// txSigners maps transaction IDs to their signing coordinators.
	txSigners map[TxID]*TxSignerCoordinator

	// signerTxIndex is a reverse index mapping each cosigner key (hex) to
	// the list of transaction IDs they're involved in. Built once in the
	// constructor for fast lookup in GetAggNoncesForSigners and
	// GetFinalSigsForSigners.
	signerTxIndex map[string][]TxID

	// log is the logger for this coordinator.
	log btclog.Logger
}

// TreeSignCoordinatorOpt is a functional option for TreeSignCoordinator.
type TreeSignCoordinatorOpt func(*TreeSignCoordinator)

// WithTreeSignLog injects a logger into the tree sign coordinator.
func WithTreeSignLog(
	l fn.Option[btclog.Logger]) TreeSignCoordinatorOpt {

	return func(c *TreeSignCoordinator) {
		c.log = l.UnwrapOr(btclog.Disabled)
	}
}

// NewTreeSignCoordinator creates a new coordinator for signing an entire
// tree. The operator's per-transaction MuSig2 sessions are created for all
// transactions in the tree automatically.
func NewTreeSignCoordinator(signer input.MuSig2Signer,
	operatorKey *keychain.KeyDescriptor,
	tree *treepkg.Tree,
	opts ...TreeSignCoordinatorOpt) (*TreeSignCoordinator, error) {

	// Validate inputs.
	if signer == nil {
		return nil, fmt.Errorf("operator signer cannot be nil")
	}

	if operatorKey == nil || operatorKey.PubKey == nil {
		return nil, fmt.Errorf("operator key cannot be nil")
	}

	if tree == nil {
		return nil, fmt.Errorf("tree cannot be nil")
	}

	prevOutFetcher, err := tree.Root.PrevOutputFetcher(tree.BatchOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to create prev output "+
			"fetcher: %w", err)
	}

	// Initialize per-transaction coordinators and build reverse index
	// mapping each cosigner to their transactions. Each coordinator creates
	// its own operator MuSig2 session internally.
	signers := make(map[TxID]*TxSignerCoordinator, tree.NumTx())
	signerTxIndex := make(map[string][]TxID)

	err = tree.Root.ForEach(func(node *treepkg.Node) error {
		txid, err := node.TXID()
		if err != nil {
			return fmt.Errorf("failed to get TXID: %w", err)
		}

		// Create tx coordinator. It will create the operator's MuSig2
		// session internally.
		coordinator, err := NewTxSignerCoordinator(
			signer, operatorKey, node,
			tree.SweepTapscriptRoot, prevOutFetcher,
		)
		if err != nil {
			return fmt.Errorf("failed to create tx coordinator: %w",
				err)
		}

		signers[txid] = coordinator

		// Build reverse index: for each cosigner in this transaction,
		// add this txid to their list.
		for _, cosigner := range node.CoSigners {
			keyHex := toHexKey(cosigner)
			signerTxIndex[keyHex] = append(
				signerTxIndex[keyHex], txid,
			)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	c := &TreeSignCoordinator{
		signer:        signer,
		txSigners:     signers,
		signerTxIndex: signerTxIndex,
		log:           btclog.Disabled,
	}

	for _, opt := range opts {
		opt(c)
	}

	ctx := context.Background()
	c.log.InfoS(ctx, "Created tree sign coordinator",
		slog.Int("tx_count", len(signers)),
		slog.Int("cosigner_count", len(signerTxIndex)))

	return c, nil
}

// AddNonces adds nonces from a signer for their transactions. Returns the
// number of nonces successfully added. Nonces for transactions this signer
// isn't involved in are silently skipped (using the precomputed signerTxIndex).
// Returns an error if a nonce fails to register for a transaction where this
// signer IS expected to be a cosigner.
func (c *TreeSignCoordinator) AddNonces(signer *btcec.PublicKey,
	nonces map[TxID]treepkg.Musig2PubNonce) (int, error) {

	// Build set of expected txids for this signer from the index.
	expectedTxIDs := c.signerTxIndex[toHexKey(signer)]
	expectedSet := make(map[TxID]struct{}, len(expectedTxIDs))
	for _, txid := range expectedTxIDs {
		expectedSet[txid] = struct{}{}
	}

	acceptedCount := 0
	for txid, nonce := range nonces {
		// Skip nonces for transactions this signer isn't involved in.
		if _, expected := expectedSet[txid]; !expected {
			continue
		}

		txCoordinator := c.txSigners[txid]
		err := txCoordinator.AddNonce(signer, nonce)
		if err != nil {
			return acceptedCount, fmt.Errorf("failed to add "+
				"nonce for tx %s: %w", txid, err)
		}

		acceptedCount++
	}

	ctx := context.Background()
	c.log.DebugS(ctx, "Nonces registered",
		slog.Int("accepted", acceptedCount),
		slog.Int("submitted", len(nonces)),
		slog.Int("expected_txs", len(expectedTxIDs)))

	return acceptedCount, nil
}

// HasAllNonces returns true if all transactions have received all nonces.
func (c *TreeSignCoordinator) HasAllNonces() bool {
	for _, txCoordinator := range c.txSigners {
		if !txCoordinator.HasAllNonces() {
			return false
		}
	}

	return true
}

// OperatorNonces returns the operator's nonces for all transactions.
// These should be distributed to client signers along with the tree structure.
func (c *TreeSignCoordinator) OperatorNonces() map[TxID]treepkg.Musig2PubNonce {
	nonces := make(
		map[TxID]treepkg.Musig2PubNonce, len(c.txSigners),
	)
	for txid, coordinator := range c.txSigners {
		nonces[txid] = coordinator.GetNonce()
	}

	return nonces
}

func (c *TreeSignCoordinator) GetAggregatedNonces() (
	map[TxID]treepkg.Musig2PubNonce, error) {

	aggNonces := make(map[TxID]treepkg.Musig2PubNonce, len(c.txSigners))
	for txid, txCoordinator := range c.txSigners {
		aggNonce, err := txCoordinator.AggregateNonces()
		if err != nil {
			return nil, fmt.Errorf("failed to aggregate nonces "+
				"for tx %s: %w", txid.String(), err)
		}

		aggNonces[txid] = aggNonce
	}

	return aggNonces, nil
}

// GetAggNoncesForSigners returns AGGREGATED nonces for transactions where the
// given signing keys are cosigners. This is used to send a client only the
// aggregated nonces they need for the transactions they're signing, allowing
// them to register via MuSig2RegisterCombinedNonce.
func (c *TreeSignCoordinator) GetAggNoncesForSigners(
	signingKeys []*btcec.PublicKey) (map[TxID]treepkg.Musig2PubNonce,
	error) {

	result := make(map[TxID]treepkg.Musig2PubNonce)

	// Use the precomputed reverse index to find transactions for these
	// signing keys. This is O(num_keys * num_txs_per_key) instead of
	// O(num_txs * num_cosigners).
	for _, key := range signingKeys {
		keyHex := toHexKey(key)
		txids := c.signerTxIndex[keyHex]

		for _, txid := range txids {
			// Skip if we've already added this transaction's nonce
			// (multiple keys might be cosigners for the same tx).
			if _, exists := result[txid]; exists {
				continue
			}

			txCoordinator := c.txSigners[txid]
			aggNonce, err := txCoordinator.AggregateNonces()
			if err != nil {
				return nil, fmt.Errorf("failed to aggregate "+
					"nonces for tx %s: %w", txid, err,
				)
			}

			result[txid] = aggNonce
		}
	}

	return result, nil
}

// GetFinalSigsForSigners returns final aggregated signatures for transactions
// where any of the given signing keys is a cosigner. This is used to send a
// client only the signatures they need for the transactions they're signing.
func (c *TreeSignCoordinator) GetFinalSigsForSigners(
	signingKeys []*btcec.PublicKey) (map[TxID]*schnorr.Signature, error) {

	result := make(map[TxID]*schnorr.Signature)

	// Use the precomputed reverse index to find transactions for these
	// signing keys. This is O(num_keys * num_txs_per_key) instead of
	// O(num_txs * num_cosigners).
	for _, key := range signingKeys {
		keyHex := toHexKey(key)
		txids := c.signerTxIndex[keyHex]

		for _, txid := range txids {
			// Skip if we've already added this transaction's
			// signature (multiple keys might be cosigners for the
			// same tx).
			if _, exists := result[txid]; exists {
				continue
			}

			txCoordinator := c.txSigners[txid]
			sig, err := txCoordinator.AggregateSig()
			if err != nil {
				return nil, fmt.Errorf("failed to sign tx "+
					"%s: %w", txid, err)
			}

			result[txid] = sig
		}
	}

	return result, nil
}

// AllFinalSigs returns aggregated signatures for all transactions in this
// coordinator. This is used by the server to apply signatures to the
// persisted VTXO trees so OOR receivers can obtain signed tree paths.
func (c *TreeSignCoordinator) AllFinalSigs() (
	map[TxID]*schnorr.Signature, error) {

	result := make(map[TxID]*schnorr.Signature, len(c.txSigners))

	for txid, txCoordinator := range c.txSigners {
		sig, err := txCoordinator.AggregateSig()
		if err != nil {
			return nil, fmt.Errorf("aggregate sig for %s: %w",
				txid, err)
		}

		result[txid] = sig
	}

	return result, nil
}

// AddPartialSignatures adds partial signatures from a signer for their
// transactions. Returns the number of signatures accepted.
func (c *TreeSignCoordinator) AddPartialSignatures(signer *btcec.PublicKey,
	sigs map[TxID]*musig2.PartialSignature) (int, error) {

	accepted := 0
	for txid, sig := range sigs {
		txCoordinator, ok := c.txSigners[txid]
		if !ok {
			return accepted, fmt.Errorf(
				"tx %s not found in coordinator", txid,
			)
		}

		err := txCoordinator.AddPartialSignature(signer, sig)
		if err != nil {
			return accepted, fmt.Errorf("failed to add partial "+
				"signature for tx %s: %w", txid, err)
		}

		accepted++
	}

	ctx := context.Background()
	c.log.DebugS(ctx, "Partial signatures registered",
		slog.Int("accepted", accepted),
		slog.Int("submitted", len(sigs)))

	return accepted, nil
}

// FullySigned returns true if all transactions have received all partial
// signatures.
func (c *TreeSignCoordinator) FullySigned() bool {
	for _, txCoordinator := range c.txSigners {
		if !txCoordinator.FullySigned() {
			return false
		}
	}

	return true
}

// Sign generates the operator's partial signatures for all
// transactions. MuSig2Sign automatically adds the operator's signature to the
// session, so no need to call MuSig2CombineSig for the operator's signature.
// This should be called after all non-operator nonces have been received and
// nonces have been aggregated.
func (c *TreeSignCoordinator) Sign() error {
	ctx := context.Background()
	c.log.InfoS(ctx, "Signing all transactions",
		slog.Int("tx_count", len(c.txSigners)))

	// Generate operator's partial signatures for all transactions.
	// MuSig2Sign automatically adds the signature to the session.
	for txid, coordinator := range c.txSigners {
		// Generate operator's partial signature. This automatically
		// adds it to the session.
		err := coordinator.Sign()
		if err != nil {
			return fmt.Errorf("failed to sign tx %s: %w",
				txid.String(), err)
		}
	}

	c.log.InfoS(ctx, "All transactions signed")

	return nil
}

// AggregateSigs combines all partial signatures and returns the final schnorr
// signatures for all transactions.
func (c *TreeSignCoordinator) AggregateSigs() (map[TxID]*schnorr.Signature,
	error) {

	ctx := context.Background()
	c.log.InfoS(ctx, "Aggregating signatures",
		slog.Int("tx_count", len(c.txSigners)))

	sigs := make(map[TxID]*schnorr.Signature, len(c.txSigners))
	for txid, txCoordinator := range c.txSigners {
		sig, err := txCoordinator.AggregateSig()
		if err != nil {
			return nil, fmt.Errorf("failed to sign tx %s: %w",
				txid.String(), err)
		}

		sigs[txid] = sig
	}

	c.log.InfoS(ctx, "Signatures aggregated",
		slog.Int("sig_count", len(sigs)))

	return sigs, nil
}
