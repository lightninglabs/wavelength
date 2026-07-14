package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"

	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/macaroons"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// MacaroonRootKeyStore is the query surface needed by the macaroon root key
// store.
type MacaroonRootKeyStore interface {
	// GetMacaroonRootKey fetches the root key for the given ID.
	GetMacaroonRootKey(ctx context.Context,
		id []byte) (sqlc.Macaroon, error)

	// InsertMacaroonRootKey persists a root key for the given ID.
	InsertMacaroonRootKey(ctx context.Context,
		arg sqlc.InsertMacaroonRootKeyParams) error
}

// BatchedMacaroonRootKeyStore runs macaroon root key queries in transactions.
type BatchedMacaroonRootKeyStore interface {
	MacaroonRootKeyStore
	BatchedTx[MacaroonRootKeyStore]
}

// RootKeyStore stores macaroon root keys in the daemon database.
type RootKeyStore struct {
	db BatchedMacaroonRootKeyStore
}

// NewMacaroonRootKeyStore creates a macaroon root key store.
func NewMacaroonRootKeyStore(db BatchedMacaroonRootKeyStore) *RootKeyStore {
	return &RootKeyStore{
		db: db,
	}
}

// NewMacaroonRootKeyStore creates a macaroon root key store for this Store.
func (s *Store) NewMacaroonRootKeyStore() *RootKeyStore {
	macaroonDB := NewTransactionExecutor(
		s.BaseDB(), func(tx *sql.Tx) MacaroonRootKeyStore {
			return s.queries.WithTx(tx)
		}, s.log,
	)

	return NewMacaroonRootKeyStore(macaroonDB)
}

// Get returns the root key for the given ID.
//
// NOTE: This implements the bakery.RootKeyStore interface.
func (r *RootKeyStore) Get(ctx context.Context, id []byte) ([]byte, error) {
	var rootKey []byte
	err := r.db.ExecTx(
		ctx, ReadTxOption(),
		func(q MacaroonRootKeyStore) error {
			mac, err := q.GetMacaroonRootKey(ctx, id)
			if err != nil {
				return err
			}

			rootKey = mac.RootKey

			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return rootKey, nil
}

// RootKey returns the root key to use for creating a new macaroon, along with
// the ID that can be used to look it up later with Get.
//
// NOTE: This implements the bakery.RootKeyStore interface.
func (r *RootKeyStore) RootKey(ctx context.Context) ([]byte, []byte, error) {
	id, err := macaroons.RootKeyIDFromContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	var rootKey []byte
	err = r.db.ExecTx(
		ctx, WriteTxOption(),
		func(q MacaroonRootKeyStore) error {
			mac, err := q.GetMacaroonRootKey(ctx, id)
			if err == nil {
				rootKey = mac.RootKey

				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}

			rootKey = make([]byte, macaroons.RootKeyLen)
			if _, err := io.ReadFull(
				rand.Reader, rootKey,
			); err != nil {
				return err
			}

			return q.InsertMacaroonRootKey(
				ctx, sqlc.InsertMacaroonRootKeyParams{
					ID:      id,
					RootKey: rootKey,
				},
			)
		},
	)
	if err != nil {
		return nil, nil, err
	}

	return rootKey, id, nil
}

var _ bakery.RootKeyStore = (*RootKeyStore)(nil)
