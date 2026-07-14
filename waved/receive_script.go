package waved

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	defaultOORReceiveScriptRegistrationTTL = 30 * 24 * time.Hour

	// oorReceiveKeyFamily is the key family used for OOR receive
	// keys. It is distinct from the identity key family (and the
	// boarding/VTXO families) so receive keys live in their own
	// derivation path.
	//
	// The value must stay within the range of key family accounts that
	// lnd pre-provisions on a wallet (families 0..255, created via
	// InitAccounts when the wallet is set up or converted to
	// watch-only). Deriving from a family outside that range forces
	// lnd to create the account on demand, which needs the master key
	// and therefore fails on a watch-only + remote-signer wallet with
	// "address manager is watching-only". 202 keeps us inside the
	// pre-provisioned range and continues the ark operator block
	// (sweep=200, operator=201) used on the server side.
	oorReceiveKeyFamily = keychain.KeyFamily(202)
)

// DefaultOORReceiveKeyFamily returns the key family used for
// deriving OOR receive keys. Exported so that test harnesses can
// derive keys from the same family.
func DefaultOORReceiveKeyFamily() keychain.KeyFamily {
	return oorReceiveKeyFamily
}

// ownedReceiveScriptStore persists locally owned receive-script metadata.
type ownedReceiveScriptStore interface {
	UpsertOwnedReceiveScript(ctx context.Context,
		rec db.OwnedReceiveScriptRecord) error
}

// ownedReceiveScriptLookup loads locally owned receive-script metadata by
// pkScript.
type ownedReceiveScriptLookup interface {
	LookupOwnedReceiveScript(ctx context.Context,
		pkScript []byte) (*db.OwnedReceiveScriptRecord, error)
}

// ownedReceiveScriptLister lists locally owned receive-script metadata.
type ownedReceiveScriptLister interface {
	ListOwnedReceiveScripts(ctx context.Context) (
		[]db.OwnedReceiveScriptRecord, error)
}

// defaultOORReceiveScriptStateStore provides the persistent state needed to
// reuse or register the daemon-managed default OOR receive key.
type defaultOORReceiveScriptStateStore interface {
	ownedReceiveScriptStore
	ownedReceiveScriptLister
}

// DeriveDefaultOORReceiveKeyFunc derives the next wallet-managed OOR receive
// key when no previously persisted key exists.
type DeriveDefaultOORReceiveKeyFunc func(context.Context) (
	*keychain.KeyDescriptor, error,
)

// OORReceiveScriptSignerFactory constructs an indexer proof signer for the
// wallet key that controls one owned receive script.
type OORReceiveScriptSignerFactory func(
	keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner

// BuildPubKeyVTXOReceiveScript derives the standard VTXO-compatible taproot
// output script for an OOR recipient pubkey.
func BuildPubKeyVTXOReceiveScript(recipientKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]byte, error) {

	switch {
	case recipientKey == nil:
		return nil, fmt.Errorf("recipient key must be provided")

	case operatorKey == nil:
		return nil, fmt.Errorf("operator key must be provided")
	}

	tapKey, err := arkscript.VTXOTapKey(
		recipientKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("derive vtxo tap key: %w", err)
	}

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return nil, fmt.Errorf("derive receive pk script: %w", err)
	}

	return pkScript, nil
}

// ResolveOwnedReceiveScriptKey resolves the locally owned wallet key for an
// incoming recipient output using the persisted receive-script ownership map.
func ResolveOwnedReceiveScriptKey(ctx context.Context,
	store ownedReceiveScriptLookup,
	recipient oor.ArkRecipientOutput) (keychain.KeyDescriptor, error) {

	switch {
	case store == nil:
		return keychain.KeyDescriptor{}, fmt.Errorf("owned receive " +
			"script lookup must be provided")

	case len(recipient.PkScript) == 0:
		return keychain.KeyDescriptor{}, fmt.Errorf("recipient pk " +
			"script must be provided")
	}

	rec, err := store.LookupOwnedReceiveScript(ctx, recipient.PkScript)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return keychain.KeyDescriptor{},
				oor.ErrIncomingRecipientNotOwned
		}

		return keychain.KeyDescriptor{}, fmt.Errorf("lookup owned "+
			"receive script: %w", err)
	}

	return rec.ClientKey, nil
}

// ownedReceiveScriptSigner resolves the correct wallet key for each pkScript
// from the persisted owned receive-script map, then delegates signing to the
// backend-specific signer implementation for that key.
type ownedReceiveScriptSigner struct {
	store         ownedReceiveScriptLookup
	signerFactory OORReceiveScriptSignerFactory
}

// fallbackSchnorrSigner tries the primary signer first and falls back to a
// secondary signer when the primary cannot resolve the requested script.
type fallbackSchnorrSigner struct {
	primary  indexer.SchnorrSigner
	fallback indexer.SchnorrSigner
}

// NewOwnedReceiveScriptSigner returns an indexer signer that can prove control
// for any persisted locally owned receive script instead of a single fixed key.
func NewOwnedReceiveScriptSigner(store ownedReceiveScriptLookup,
	signerFactory OORReceiveScriptSignerFactory) indexer.SchnorrSigner {

	return &ownedReceiveScriptSigner{
		store:         store,
		signerFactory: signerFactory,
	}
}

// NewFallbackSchnorrSigner returns a signer that tries primary first and uses
// fallback only when primary fails.
func NewFallbackSchnorrSigner(
	primary, fallback indexer.SchnorrSigner) indexer.SchnorrSigner {

	switch {
	case primary == nil:
		return fallback

	case fallback == nil:
		return primary
	}

	return &fallbackSchnorrSigner{
		primary:  primary,
		fallback: fallback,
	}
}

// SignSchnorr signs hash with the primary signer, falling back on error. The
// primary's error is preserved when the fallback also fails so the original
// cause remains recoverable.
func (s *fallbackSchnorrSigner) SignSchnorr(pkScript []byte, hash [32]byte) (
	[]byte, error) {

	var primaryErr error
	if s.primary != nil {
		sig, err := s.primary.SignSchnorr(pkScript, hash)
		if err == nil {
			return sig, nil
		}
		primaryErr = err
	}

	if s.fallback == nil {
		return nil, joinWithPrimary(
			primaryErr,
			fmt.Errorf("fallback signer not configured"),
		)
	}

	sig, err := s.fallback.SignSchnorr(pkScript, hash)
	if err != nil {
		return nil, joinWithPrimary(primaryErr, err)
	}

	return sig, nil
}

// SignSchnorrMessage signs a tagged message with the primary signer, falling
// back on error. Primary errors are preserved when the fallback also fails.
func (s *fallbackSchnorrSigner) SignSchnorrMessage(ctx context.Context,
	pkScript []byte, message []byte, tag []byte) ([]byte, error) {

	var primaryErr error
	if s.primary != nil {
		msgSigner, ok := s.primary.(interface {
			SignSchnorrMessage(context.Context, []byte, []byte,
				[]byte) ([]byte, error)
		})
		if ok {
			sig, err := msgSigner.SignSchnorrMessage(
				ctx, pkScript, message, tag,
			)
			if err == nil {
				return sig, nil
			}
			primaryErr = err
		}
	}

	if s.fallback == nil {
		return nil, joinWithPrimary(
			primaryErr,
			fmt.Errorf("fallback signer not configured"),
		)
	}

	msgSigner, ok := s.fallback.(interface {
		SignSchnorrMessage(context.Context, []byte, []byte,
			[]byte) ([]byte, error)
	})
	if !ok {
		return nil, joinWithPrimary(
			primaryErr,
			fmt.Errorf("fallback signer does not support tagged "+
				"message signing"),
		)
	}

	sig, err := msgSigner.SignSchnorrMessage(
		ctx, pkScript, message, tag,
	)
	if err != nil {
		return nil, joinWithPrimary(primaryErr, err)
	}

	return sig, nil
}

// ProofPubKey returns the proof pubkey from the primary signer, falling back
// on error. Primary errors are preserved when the fallback also fails.
func (s *fallbackSchnorrSigner) ProofPubKey(pkScript []byte) (*btcec.PublicKey,
	error) {

	var primaryErr error
	if s.primary != nil {
		pubKeySource, ok := s.primary.(interface {
			ProofPubKey([]byte) (*btcec.PublicKey, error)
		})
		if ok {
			pubKey, err := pubKeySource.ProofPubKey(pkScript)
			if err == nil {
				return pubKey, nil
			}
			primaryErr = err
		}
	}

	if s.fallback == nil {
		return nil, joinWithPrimary(
			primaryErr,
			fmt.Errorf("fallback signer not configured"),
		)
	}

	pubKeySource, ok := s.fallback.(interface {
		ProofPubKey([]byte) (*btcec.PublicKey, error)
	})
	if !ok {
		return nil, joinWithPrimary(
			primaryErr,
			fmt.Errorf("fallback signer does not expose proof "+
				"pubkey"),
		)
	}

	pubKey, err := pubKeySource.ProofPubKey(pkScript)
	if err != nil {
		return nil, joinWithPrimary(primaryErr, err)
	}

	return pubKey, nil
}

// joinWithPrimary returns the fallback error on its own when there is no
// primary error, otherwise both are joined so the original primary error
// remains recoverable via errors.Is / errors.As.
func joinWithPrimary(primary, fallback error) error {
	if primary == nil {
		return fallback
	}

	return errors.Join(
		fmt.Errorf("primary signer failed: %w", primary),
		fmt.Errorf("fallback signer failed: %w", fallback),
	)
}

// resolveSigner loads the locally owned key for pkScript and constructs the
// backend-specific signer that can prove control over it.
func (s *ownedReceiveScriptSigner) resolveSigner(ctx context.Context,
	pkScript []byte) (indexer.SchnorrSigner, error) {

	if s.store == nil {
		return nil, fmt.Errorf("owned receive script lookup must be " +
			"provided")
	}

	if s.signerFactory == nil {
		return nil, fmt.Errorf("receive script signer factory must " +
			"be provided")
	}

	rec, err := s.store.LookupOwnedReceiveScript(ctx, pkScript)
	if err != nil {
		return nil, fmt.Errorf("lookup owned receive script: %w", err)
	}

	signer := s.signerFactory(rec.ClientKey)
	if signer == nil {
		return nil, fmt.Errorf("receive script signer factory " +
			"returned nil signer")
	}

	return signer, nil
}

// SignSchnorr signs hash with the wallet key that controls pkScript.
func (s *ownedReceiveScriptSigner) SignSchnorr(pkScript []byte, hash [32]byte) (
	[]byte, error) {

	signer, err := s.resolveSigner(context.Background(), pkScript)
	if err != nil {
		return nil, err
	}

	return signer.SignSchnorr(pkScript, hash)
}

// SignSchnorrMessage signs the canonical proof preimage for pkScript using the
// backend-specific tagged-message path when available.
func (s *ownedReceiveScriptSigner) SignSchnorrMessage(ctx context.Context,
	pkScript []byte, message []byte, tag []byte) ([]byte, error) {

	signer, err := s.resolveSigner(ctx, pkScript)
	if err != nil {
		return nil, err
	}

	msgSigner, ok := signer.(interface {
		SignSchnorrMessage(context.Context, []byte, []byte,
			[]byte) ([]byte, error)
	})
	if !ok {
		return nil, fmt.Errorf("signer does not support tagged " +
			"message signing")
	}

	return msgSigner.SignSchnorrMessage(
		ctx, pkScript, message, tag,
	)
}

// ProofPubKey returns the owner pubkey committed into proofs for pkScript.
func (s *ownedReceiveScriptSigner) ProofPubKey(pkScript []byte) (
	*btcec.PublicKey, error) {

	signer, err := s.resolveSigner(context.Background(), pkScript)
	if err != nil {
		return nil, err
	}

	pubKeySource, ok := signer.(interface {
		ProofPubKey([]byte) (*btcec.PublicKey, error)
	})
	if !ok {
		return nil, fmt.Errorf("signer does not expose proof pubkey")
	}

	return pubKeySource.ProofPubKey(pkScript)
}

// CreateOORReceiveScript derives a fresh wallet key, registers the matching
// receive script with the indexer, and persists the ownership metadata needed
// to prove control over that script later.
func CreateOORReceiveScript(ctx context.Context, idx *indexer.Client,
	store ownedReceiveScriptStore,
	deriveNextKey DeriveDefaultOORReceiveKeyFunc,
	signerFactory OORReceiveScriptSignerFactory,
	operatorKey *btcec.PublicKey, exitDelay uint32, label string) (
	*keychain.KeyDescriptor, []byte, error) {

	if deriveNextKey == nil {
		return nil, nil, fmt.Errorf("derive next key func must be " +
			"provided")
	}

	keyDesc, err := deriveNextKey(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("derive oor receive key: %w", err)
	}

	if keyDesc == nil || keyDesc.PubKey == nil {
		return nil, nil, fmt.Errorf("derive oor receive key: missing " +
			"pubkey")
	}

	pkScript, err := RegisterOwnedOORReceiveScript(
		ctx, idx, store, *keyDesc, signerFactory, operatorKey,
		exitDelay, label,
	)
	if err != nil {
		return nil, nil, err
	}

	return keyDesc, pkScript, nil
}

// RegisterOwnedOORReceiveScript registers and persists one wallet-owned OOR
// receive script for clientKey using the signer produced by signerFactory.
func RegisterOwnedOORReceiveScript(ctx context.Context, idx *indexer.Client,
	store ownedReceiveScriptStore, clientKey keychain.KeyDescriptor,
	signerFactory OORReceiveScriptSignerFactory,
	operatorKey *btcec.PublicKey, exitDelay uint32,
	label string) ([]byte, error) {

	switch {
	case idx == nil:
		return nil, fmt.Errorf("indexer client must be provided")

	case signerFactory == nil:
		return nil, fmt.Errorf("receive script signer factory must " +
			"be provided")

	case clientKey.PubKey == nil:
		return nil, fmt.Errorf("client key must be provided")
	}

	pkScript, err := BuildPubKeyVTXOReceiveScript(
		clientKey.PubKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, err
	}

	registerClient := idx.WithSigner(signerFactory(clientKey))

	expiresAt := time.Now().Add(defaultOORReceiveScriptRegistrationTTL)
	_, err = registerClient.RegisterReceiveScriptTaproot(
		ctx, pkScript, expiresAt, label,
	)
	if err != nil {
		return nil, fmt.Errorf("register receive script: %w", err)
	}

	if store == nil {
		return pkScript, nil
	}

	err = store.UpsertOwnedReceiveScript(ctx, db.OwnedReceiveScriptRecord{
		PkScript:       pkScript,
		ClientKey:      clientKey,
		OperatorPubKey: operatorKey,
		ExitDelay:      int64(exitDelay),
		Source:         db.OwnedReceiveScriptSourceWallet,
		CreatedAt:      time.Now(),
		LastUsedAt:     fn.None[time.Time](),
	})
	if err != nil {
		return nil, fmt.Errorf("persist owned receive script: %w", err)
	}

	return pkScript, nil
}

// LoadDefaultOORReceiveKey loads the most recently persisted wallet-managed
// OOR receive key, if one exists.
func LoadDefaultOORReceiveKey(ctx context.Context,
	store ownedReceiveScriptLister) (*keychain.KeyDescriptor, error) {

	if store == nil {
		return nil, fmt.Errorf("owned receive script lister must be " +
			"provided")
	}

	records, err := store.ListOwnedReceiveScripts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list owned receive scripts: %w", err)
	}

	var latest *db.OwnedReceiveScriptRecord
	for i := range records {
		rec := &records[i]
		if rec.Source != db.OwnedReceiveScriptSourceWallet {
			continue
		}

		if latest == nil || rec.CreatedAt.After(latest.CreatedAt) {
			latest = rec
		}
	}

	if latest == nil {
		return nil, nil
	}

	return &latest.ClientKey, nil
}

// EnsureDefaultOORReceiveKey loads the most recently persisted wallet-managed
// OOR receive key or derives a new one when no prior registration exists.
func EnsureDefaultOORReceiveKey(ctx context.Context,
	store ownedReceiveScriptLister,
	deriveNextKey DeriveDefaultOORReceiveKeyFunc) (*keychain.KeyDescriptor,
	error) {

	if store == nil {
		return nil, fmt.Errorf("owned receive script lister must be " +
			"provided")
	}

	keyDesc, err := LoadDefaultOORReceiveKey(ctx, store)
	if err != nil {
		return nil, err
	}

	if keyDesc != nil {
		return keyDesc, nil
	}

	if deriveNextKey == nil {
		return nil, fmt.Errorf("derive next key func must be provided")
	}

	keyDesc, err = deriveNextKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("derive default oor receive key: %w",
			err)
	}

	if keyDesc == nil || keyDesc.PubKey == nil {
		return nil, fmt.Errorf("derive default oor receive key: " +
			"missing pubkey")
	}

	return keyDesc, nil
}

// EnsureDefaultOORReceiveScript ensures the daemon-managed default OOR receive
// key exists, registers the corresponding receive script, and persists its
// ownership metadata for later incoming-script resolution.
func EnsureDefaultOORReceiveScript(ctx context.Context, idx *indexer.Client,
	store defaultOORReceiveScriptStateStore,
	deriveNextKey DeriveDefaultOORReceiveKeyFunc,
	signerFactory OORReceiveScriptSignerFactory,
	operatorKey *btcec.PublicKey, exitDelay uint32, label string) (
	*keychain.KeyDescriptor, []byte, error) {

	if store == nil {
		return nil, nil, fmt.Errorf("default OOR receive script " +
			"store must be provided")
	}

	keyDesc, err := EnsureDefaultOORReceiveKey(
		ctx, store, deriveNextKey,
	)
	if err != nil {
		return nil, nil, err
	}

	pkScript, err := RegisterDefaultOORReceiveScript(
		ctx, idx, store, *keyDesc, signerFactory, operatorKey,
		exitDelay, label,
	)
	if err != nil {
		return nil, nil, err
	}

	return keyDesc, pkScript, nil
}

// RegisterDefaultOORReceiveScript registers and persists the default OOR
// receive script for the provided local wallet key. It delegates to
// RegisterOwnedOORReceiveScript with the provided signerFactory so the
// registration proof is signed with the correct key (the script may not
// yet be persisted in the DB at call time).
func RegisterDefaultOORReceiveScript(ctx context.Context, idx *indexer.Client,
	store ownedReceiveScriptStore, clientKey keychain.KeyDescriptor,
	signerFactory OORReceiveScriptSignerFactory,
	operatorKey *btcec.PublicKey, exitDelay uint32,
	label string) ([]byte, error) {

	return RegisterOwnedOORReceiveScript(
		ctx, idx, store, clientKey, signerFactory, operatorKey,
		exitDelay, label,
	)
}
