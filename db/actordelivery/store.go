package actordelivery

import (
	"database/sql"
	"fmt"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// NewTxAwareDeliveryStoreFromDB constructs a transaction-aware actor delivery
// store from a raw sql.DB connection and backend type.
func NewTxAwareDeliveryStoreFromDB(sqlDB *sql.DB, backend sqlc.BackendType,
	clk clock.Clock, log btclog.Logger,
	txOpts ...db.TxExecutorOption) (actor.TxAwareDeliveryStore, error) {

	if sqlDB == nil {
		return nil, fmt.Errorf("db is nil")
	}
	if clk == nil {
		clk = clock.NewDefaultClock()
	}
	if log == nil {
		log = btclog.Disabled
	}

	actorQueries := adsqlc.New(sqlDB)

	var baseQueries *sqlc.Queries
	switch backend {
	case sqlc.BackendTypeSqlite:
		baseQueries = sqlc.NewSqlite(sqlDB)

	case sqlc.BackendTypePostgres:
		baseQueries = sqlc.NewPostgres(sqlDB)

	default:
		return nil, fmt.Errorf("unsupported backend: %v", backend)
	}

	baseDB := &db.BaseDB{
		DB:      sqlDB,
		Queries: baseQueries,
	}

	storeQueries := db.NewTransactionExecutor(
		baseDB,
		func(tx *sql.Tx) ActorDeliveryQueries {
			return actorQueries.WithTx(tx)
		},
		log,
		txOpts...,
	)

	return NewTxAwareActorDeliveryStore(storeQueries, baseDB, clk), nil
}
