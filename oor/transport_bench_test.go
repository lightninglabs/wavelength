package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// BenchmarkOORTransportHandoff measures the real SQLite writes around an OOR
// transport event. The two-row approximation intentionally excludes publisher
// claim/complete bookkeeping, so the reported delta is a lower bound.
func BenchmarkOORTransportHandoff(b *testing.B) {
	ctx := context.Background()
	msg := benchmarkAckRequest()

	b.Run("direct_serverconn_mailbox_enqueue", func(b *testing.B) {
		store := newBenchDeliveryStore(ctx, b)
		codec := serverconn.NewServerConnCodec()
		benchClock := clock.NewDefaultClock()
		sendReq := benchmarkServerConnMessage(ctx, b, msg)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			payload, err := codec.Encode(sendReq)
			if err != nil {
				b.Fatalf("encode serverconn message: %v", err)
			}

			id, err := uuid.NewV7()
			if err != nil {
				b.Fatalf("new uuid: %v", err)
			}

			params := actor.EnqueueParams{
				ID:             id.String(),
				MailboxID:      "bench-serverconn",
				MessageType:    sendReq.MessageType(),
				Payload:        payload,
				AvailableAt:    benchClock.Now(),
				MaxAttempts:    3,
				CorrelationKey: sendReq.CorrelationKey(),
			}

			if err := store.EnqueueMessage(
				ctx, params,
			); err != nil {

				b.Fatalf("enqueue serverconn mailbox: %v", err)
			}
		}
	})

	b.Run("actor_outbox_encode_enqueue", func(b *testing.B) {
		store := newBenchDeliveryStore(ctx, b)
		codec := serverconn.NewServerConnCodec()
		connID := "bench-serverconn"
		benchClock := clock.NewDefaultClock()
		sendReq := benchmarkServerConnMessage(ctx, b, msg)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			payload, err := codec.Encode(sendReq)
			if err != nil {
				b.Fatalf("encode serverconn message: %v", err)
			}

			id, err := uuid.NewV7()
			if err != nil {
				b.Fatalf("new uuid: %v", err)
			}

			params := actor.OutboxParams{
				ID:            id.String(),
				SourceActorID: "bench-oor-actor",
				TargetActorID: connID,
				MessageType:   sendReq.MessageType(),
				Payload:       payload,
				DomainKey:     sendReq.CorrelationKey(),
				Version:       benchClock.Now().UnixNano(),
			}

			if err := store.EnqueueOutbox(ctx, params); err != nil {
				b.Fatalf("enqueue outbox: %v", err)
			}
		}
	})

	b.Run("old_two_row_handoff_approx", func(b *testing.B) {
		store := newBenchDeliveryStore(ctx, b)
		codec := serverconn.NewServerConnCodec()
		benchClock := clock.NewDefaultClock()
		sendReq := benchmarkServerConnMessage(ctx, b, msg)

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			outboxPayload, err := codec.Encode(sendReq)
			if err != nil {
				b.Fatalf("encode outbox message: %v", err)
			}

			outboxID, err := uuid.NewV7()
			if err != nil {
				b.Fatalf("new outbox uuid: %v", err)
			}

			outboxParams := actor.OutboxParams{
				ID:            outboxID.String(),
				SourceActorID: "bench-oor-actor",
				TargetActorID: "bench-serverconn",
				MessageType:   sendReq.MessageType(),
				Payload:       outboxPayload,
				DomainKey:     sendReq.CorrelationKey(),
				Version:       benchClock.Now().UnixNano(),
			}
			if err := store.EnqueueOutbox(
				ctx, outboxParams,
			); err != nil {

				b.Fatalf("enqueue outbox: %v", err)
			}

			decoded, err := codec.Decode(outboxPayload)
			if err != nil {
				b.Fatalf("decode outbox message: %v", err)
			}

			mailboxPayload, err := codec.Encode(decoded)
			if err != nil {
				b.Fatalf("encode mailbox message: %v", err)
			}

			mailboxID, err := uuid.NewV7()
			if err != nil {
				b.Fatalf("new mailbox uuid: %v", err)
			}

			mailboxParams := actor.EnqueueParams{
				ID:             mailboxID.String(),
				MailboxID:      "bench-serverconn",
				MessageType:    decoded.MessageType(),
				Payload:        mailboxPayload,
				AvailableAt:    benchClock.Now(),
				MaxAttempts:    3,
				CorrelationKey: decoded.CorrelationKey(),
			}
			if err := store.EnqueueMessage(
				ctx, mailboxParams,
			); err != nil {

				b.Fatalf("enqueue serverconn mailbox: %v", err)
			}
		}
	})
}

// benchmarkAckRequest returns the smallest OOR transport event that still uses
// the same serverconn wrapping path as submit and finalize events.
func benchmarkAckRequest() *SendIncomingAckRequest {
	return &SendIncomingAckRequest{
		SessionID: SessionID{
			0x01,
			0x02,
			0x03,
		},
	}
}

// benchmarkServerConnMessage converts msg into the serverconn message that OOR
// durably enqueues for remote delivery.
func benchmarkServerConnMessage(ctx context.Context, b *testing.B,
	msg OutboxEvent) serverconn.ServerConnMsg {

	b.Helper()

	behavior := &sessionBehavior{
		cfg: SessionActorConfig{
			Log: fn.Some(btclog.Disabled),
		},
		actorID: "bench-oor-actor",
	}

	sendReq, err := behavior.buildTransportMessage(ctx, msg)
	if err != nil {
		b.Fatalf("build transport message: %v", err)
	}

	return sendReq
}

// newBenchDeliveryStore creates an actor delivery store for benchmark-only
// actor-delivery writes. It accepts ctx so contextcheck can see callers pass
// their benchmark context through the local setup boundary.
func newBenchDeliveryStore(_ context.Context,
	b *testing.B) actor.DeliveryStore {

	b.Helper()

	// The repository test DB helper does not accept a context.
	//nolint:contextcheck
	sqlDB := db.NewTestDB(b)
	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clock.NewDefaultClock(),
		btclog.Disabled,
	)
	if err != nil {
		b.Fatalf("new delivery store: %v", err)
	}

	txAwareStore, ok := store.(*actordelivery.TxAwareActorDeliveryStore)
	if !ok {
		b.Fatalf("expected tx-aware delivery store, got %T", store)
	}

	return txAwareStore.Store
}
