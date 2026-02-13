// Package actordelivery provides an isolated SQL integration surface for
// durable actor mailbox persistence.
//
// The package intentionally separates actor-delivery schema lifecycle from the
// broader client schema lifecycle so other services can reuse durable actor
// storage without pulling unrelated tables.
//
// It exposes:
//   - RunMigrations, which applies only actor-delivery schema migrations with
//     a dedicated migration bookkeeping table.
//   - NewTxAwareDeliveryStoreFromDB, which constructs an
//     actor.TxAwareDeliveryStore from a raw *sql.DB and backend type.
package actordelivery
