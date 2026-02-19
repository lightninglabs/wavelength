package db

import (
	"database/sql"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	actordelivery "github.com/lightninglabs/darepo-client/db/actordelivery"
	clientsqlc "github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

type sqlDBProvider interface {
	SQLDB() *sql.DB
}

// deliveryStoreShim intentionally exposes only actor.DeliveryStore methods.
// The wrapped client store may also implement TxAwareDeliveryStore, but server
// OOR currently relies on non-transactional processing because the rest of the
// OOR persistence path is not yet transaction-context-aware.
type deliveryStoreShim struct {
	actor.DeliveryStore
}

// toClientBackendType maps server sqlc backend types to shared client db/sqlc
// backend types.
func toClientBackendType(backend sqlc.BackendType) (clientsqlc.BackendType,
	error) {

	switch backend {
	case sqlc.BackendTypeSqlite:
		return clientsqlc.BackendTypeSqlite, nil

	case sqlc.BackendTypePostgres:
		return clientsqlc.BackendTypePostgres, nil

	default:
		return clientsqlc.BackendTypeUnknown, fmt.Errorf(
			"unsupported backend type: %v", backend,
		)
	}
}

// runActorDeliveryMigrations applies the shared actor-delivery schema.
func runActorDeliveryMigrations(db *sql.DB, backend sqlc.BackendType) error {
	clientBackend, err := toClientBackendType(backend)
	if err != nil {
		return err
	}

	return actordelivery.RunMigrations(db, clientBackend)
}

// NewActorDeliveryStoreFromDB creates an actor delivery store using the shared
// client actordelivery package.
func NewActorDeliveryStoreFromDB(dbq BatchedQuerier, clk clock.Clock,
	log btclog.Logger) (actor.DeliveryStore, error) {

	dbProvider, ok := dbq.(sqlDBProvider)
	if !ok {
		return nil, fmt.Errorf(
			"batched querier does not expose SQLDB",
		)
	}

	rawDB := dbProvider.SQLDB()
	if rawDB == nil {
		return nil, fmt.Errorf("sql db handle is nil")
	}

	clientBackend, err := toClientBackendType(dbq.Backend())
	if err != nil {
		return nil, err
	}

	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		rawDB, clientBackend, clk, log,
	)
	if err != nil {
		return nil, err
	}

	return &deliveryStoreShim{DeliveryStore: store}, nil
}
