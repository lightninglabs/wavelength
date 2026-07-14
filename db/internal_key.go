package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	// ErrInternalKeyPubKeyMissing is returned when a key descriptor with a
	// nil public key is registered or resolved.
	ErrInternalKeyPubKeyMissing = errors.New("internal key descriptor " +
		"has no public key")

	// ErrInternalKeyNotFound is returned when an internal key cannot be
	// resolved from the registry.
	ErrInternalKeyNotFound = errors.New("internal key not found")
)

// InternalKeyQuerier is the minimal query surface the internal-key registry
// helpers need. The sqlc-generated *Queries satisfies it, as does every
// consumer store interface (RoundStore, BoardingStore, ...) that embeds these
// two methods, so a store can register-then-reference an internal key inside
// the same transaction that writes the referencing row.
type InternalKeyQuerier interface {
	UpsertInternalKey(ctx context.Context,
		arg sqlc.UpsertInternalKeyParams) (int64, error)

	GetInternalKeyByID(ctx context.Context,
		id int64) (sqlc.InternalKey, error)
}

// RegisterInternalKeyTx idempotently records desc within the caller's
// transaction/query context and returns its registry id, using now as the
// created_at timestamp for a freshly inserted row. It is the entry point
// consumer stores use to register-then-reference an internal key in the same
// transaction that writes the referencing row. Re-registering an identical
// (pubkey, key_family, key_index) triple returns the existing id.
//
// Unlike the server registry, client keys carry no role: the canonical
// identity is the full (pubkey, key_family, key_index) triple, so there is no
// "same pubkey, different locator" conflict to surface -- a different triple
// is simply a different key.
func RegisterInternalKeyTx(ctx context.Context, qtx InternalKeyQuerier,
	now int64, desc keychain.KeyDescriptor) (int64, error) {

	if desc.PubKey == nil {
		return 0, ErrInternalKeyPubKeyMissing
	}

	pubKey := desc.PubKey.SerializeCompressed()
	family := int64(desc.KeyLocator.Family)
	index := int64(desc.KeyLocator.Index)

	// A single cross-backend UPSERT inserts the row when the triple is new
	// and otherwise no-ops on the existing row, RETURNING the stored
	// surrogate id either way. The RETURNING-on-conflict shape closes the
	// read-then-insert race a separate re-select would leave open.
	id, err := qtx.UpsertInternalKey(ctx, sqlc.UpsertInternalKeyParams{
		Pubkey:    pubKey,
		KeyFamily: family,
		KeyIndex:  index,
		CreatedAt: now,
	})
	if err != nil {
		return 0, fmt.Errorf("upsert internal key: %w", err)
	}

	return id, nil
}

// InternalKeyDescByIDTx reconstructs the internal key descriptor for a registry
// id within the caller's transaction/query context. It is the entry point
// consumer stores use to hydrate a descriptor from a referencing row's
// *_key_id foreign key on load.
func InternalKeyDescByIDTx(ctx context.Context, qtx InternalKeyQuerier,
	id int64) (keychain.KeyDescriptor, error) {

	row, err := qtx.GetInternalKeyByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return keychain.KeyDescriptor{}, fmt.Errorf("%w: id %d",
			ErrInternalKeyNotFound, id)
	}
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("get internal key "+
			"by id %d: %w", id, err)
	}

	return internalKeyDescFromRow(row)
}

// internalKeyDescFromRow turns a stored internal_keys row into a
// keychain.KeyDescriptor.
func internalKeyDescFromRow(row sqlc.InternalKey) (keychain.KeyDescriptor,
	error) {

	pubKey, err := btcec.ParsePubKey(row.Pubkey)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("parse internal "+
			"key pubkey: %w", err)
	}

	return keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(row.KeyFamily),
			Index:  uint32(row.KeyIndex),
		},
		PubKey: pubKey,
	}, nil
}
