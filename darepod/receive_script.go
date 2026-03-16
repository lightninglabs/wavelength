package darepod

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/indexer"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	defaultOORReceiveScriptRegistrationTTL = 30 * 24 * time.Hour

	// defaultOORReceiveKeyFamily is the dedicated BIP32 key family used
	// for the daemon's default OOR receive script. This must be separate
	// from the node identity key family because lnd's signer path for the
	// identity key does not produce valid tapscript spend signatures for
	// rematerialized OOR VTXOs.
	defaultOORReceiveKeyFamily keychain.KeyFamily = 44
)

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
		[]db.OwnedReceiveScriptRecord, error,
	)
}

// defaultOORReceiveScriptStateStore provides the persistent state needed to
// reuse or register the daemon-managed default OOR receive key.
type defaultOORReceiveScriptStateStore interface {
	ownedReceiveScriptStore
	ownedReceiveScriptLister
}

// DeriveDefaultOORReceiveKeyFunc derives the next wallet-managed default OOR
// receive key when no previously persisted key exists.
type DeriveDefaultOORReceiveKeyFunc func(context.Context) (
	*keychain.KeyDescriptor, error,
)

// DefaultOORReceiveKeyFamily returns the dedicated key family used for
// daemon-managed OOR receive keys.
func DefaultOORReceiveKeyFamily() keychain.KeyFamily {
	return defaultOORReceiveKeyFamily
}

// BuildPubKeyVTXOReceiveScript derives the standard VTXO-compatible taproot
// output script for an OOR recipient pubkey.
func BuildPubKeyVTXOReceiveScript(recipientKey,
	operatorKey *btcec.PublicKey, exitDelay uint32) ([]byte, error) {

	switch {
	case recipientKey == nil:
		return nil, fmt.Errorf("recipient key must be provided")

	case operatorKey == nil:
		return nil, fmt.Errorf("operator key must be provided")
	}

	tapKey, err := scripts.VTXOTapKey(
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
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"owned receive script lookup must be provided",
		)

	case len(recipient.PkScript) == 0:
		return keychain.KeyDescriptor{}, fmt.Errorf(
			"recipient pk script must be provided",
		)
	}

	rec, err := store.LookupOwnedReceiveScript(ctx, recipient.PkScript)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("lookup owned "+
			"receive script: %w", err)
	}

	return rec.ClientKey, nil
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
	deriveNextKey DeriveDefaultOORReceiveKeyFunc) (
	*keychain.KeyDescriptor, error,
) {

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
func EnsureDefaultOORReceiveScript(ctx context.Context,
	idx *indexer.Client, store defaultOORReceiveScriptStateStore,
	deriveNextKey DeriveDefaultOORReceiveKeyFunc,
	operatorKey *btcec.PublicKey, exitDelay uint32, label string) (
	*keychain.KeyDescriptor, []byte, error,
) {

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
		ctx, idx, store, *keyDesc, operatorKey, exitDelay, label,
	)
	if err != nil {
		return nil, nil, err
	}

	return keyDesc, pkScript, nil
}

// RegisterDefaultOORReceiveScript derives, registers, and persists the
// default OOR receive script for the provided local wallet key.
func RegisterDefaultOORReceiveScript(ctx context.Context,
	idx *indexer.Client, store ownedReceiveScriptStore,
	clientKey keychain.KeyDescriptor, operatorKey *btcec.PublicKey,
	exitDelay uint32, label string) ([]byte, error) {

	switch {
	case idx == nil:
		return nil, fmt.Errorf("indexer client must be provided")

	case clientKey.PubKey == nil:
		return nil, fmt.Errorf("client key must be provided")
	}

	pkScript, err := BuildPubKeyVTXOReceiveScript(
		clientKey.PubKey, operatorKey, exitDelay,
	)
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(defaultOORReceiveScriptRegistrationTTL)
	_, err = idx.RegisterReceiveScriptTaproot(
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
