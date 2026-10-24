package vtxo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/metrics"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// mockScriptLookup implements OwnedScriptLookup for testing.
type mockScriptLookup struct {
	scripts map[string]*OwnedReceiveScript
}

func (m *mockScriptLookup) LookupOwnedReceiveScript(_ context.Context,
	pkScript []byte) (*OwnedReceiveScript, error) {

	rec, ok := m.scripts[string(pkScript)]
	if !ok {
		return nil, sql.ErrNoRows
	}

	return rec, nil
}

// mockVTXOSaver implements VTXOSaver for testing.
type mockVTXOSaver struct {
	saved                  []*Descriptor
	failures               int
	err                    error
	succeedOnDuplicateSave bool
}

type recordingIncomingExec struct {
	commits  int
	failures int
	err      error
}

// Read executes the durable test read callback.
func (e *recordingIncomingExec) Read(ctx context.Context,
	fn func(context.Context, struct{}) error) error {

	return fn(ctx, struct{}{})
}

// Stage records the pending test write callback.
func (e *recordingIncomingExec) Stage(ctx context.Context,
	fn func(context.Context, struct{}) error) error {

	return fn(ctx, struct{}{})
}

// Commit injects an acknowledgement failure or applies the staged callback.
func (e *recordingIncomingExec) Commit(ctx context.Context,
	fn func(context.Context, struct{}) error) error {

	if e.failures > 0 {
		e.failures--

		return e.err
	}
	e.commits++

	return fn(ctx, struct{}{})
}

// SaveVTXO injects a write failure before retaining a descriptor copy.
func (m *mockVTXOSaver) SaveVTXO(_ context.Context, desc *Descriptor) error {
	if m.failures > 0 {
		m.failures--

		return m.err
	}
	for _, saved := range m.saved {
		if saved.Outpoint == desc.Outpoint {
			if m.succeedOnDuplicateSave {
				return nil
			}

			return errors.New("duplicate VTXO")
		}
	}
	m.saved = append(m.saved, desc)

	return nil
}

// GetVTXO returns the canonical test row for crash-window redelivery.
func (m *mockVTXOSaver) GetVTXO(_ context.Context, outpoint wire.OutPoint) (
	*Descriptor, error) {

	for _, saved := range m.saved {
		if saved.Outpoint == outpoint {
			return saved, nil
		}
	}

	return nil, ErrVTXONotFound
}

// TestIncomingVTXOHandlerRetriesAuthenticatedExpiryWrite proves a failed save
// does not consume the event or lose the authenticated scalar. Redelivery
// recomputes the proof and persists the same expiry before notification.
func TestIncomingVTXOHandlerRetriesAuthenticatedExpiryWrite(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pkScript := []byte{0x51, 0x20, 0xaa, 0xcc}
	lookup := &mockScriptLookup{scripts: map[string]*OwnedReceiveScript{
		string(pkScript): {
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorPubKey: operatorKey.PubKey(),
			ExitDelay:      144,
		},
	}}
	saveErr := errors.New("database unavailable")
	saver := &mockVTXOSaver{failures: 1, err: saveErr}
	fetches := 0
	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		ScriptStore: lookup,
		VTXOStore:   saver,
		AncestryFetcher: func(context.Context, wire.OutPoint, []byte,
			keychain.KeyDescriptor) (IncomingVTXOExtras, error) {

			fetches++

			return IncomingVTXOExtras{
				Output: &wire.TxOut{
					Value:    50_000,
					PkScript: pkScript,
				},
				Ancestry: []Ancestry{
					{},
				},
				BatchExpiry: 800_144,
			}, nil
		},
	})

	var txid chainhash.Hash
	txid[0] = 0x42
	msg := &IncomingVTXOMsg{Event: newTestEvent(
		txid, 0, pkScript, 50_000, "round-retry",
	)}

	_, err = handler.Receive(t.Context(), msg).Unpack()
	require.ErrorIs(t, err, saveErr)
	require.Empty(t, saver.saved)

	_, err = handler.Receive(t.Context(), msg).Unpack()
	require.NoError(t, err)
	require.Len(t, saver.saved, 1)
	require.Equal(t, int32(800_144), saver.saved[0].BatchExpiry)
	require.Equal(t, 2, fetches)
}

// TestIncomingVTXODurableBehaviorPostponesAuthenticationFailure proves a
// transient proof failure leaves the durable event unconsumed. A later retry
// persists the VTXO and commits the mailbox acknowledgement.
func TestIncomingVTXODurableBehaviorPostponesAuthenticationFailure(
	t *testing.T) {

	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pkScript := []byte{0x51, 0x20, 0xbb, 0xcc}
	lookup := &mockScriptLookup{scripts: map[string]*OwnedReceiveScript{
		string(pkScript): {
			ClientKey: keychain.KeyDescriptor{
				PubKey: clientKey.PubKey(),
			},
			OperatorPubKey: operatorKey.PubKey(),
			ExitDelay:      144,
		},
	}}
	saver := &mockVTXOSaver{}
	fetches := 0
	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		ScriptStore: lookup,
		VTXOStore:   saver,
		AncestryFetcher: func(context.Context, wire.OutPoint, []byte,
			keychain.KeyDescriptor) (IncomingVTXOExtras, error) {

			fetches++
			if fetches == 1 {
				return IncomingVTXOExtras{},
					errors.New("chain backend catching up")
			}

			return IncomingVTXOExtras{
				Output: &wire.TxOut{
					Value:    50_000,
					PkScript: pkScript,
				},
				Ancestry: []Ancestry{
					{},
				},
				BatchExpiry: 900_144,
			}, nil
		},
	})
	behavior := &incomingVTXODurableBehavior{handler: handler}
	exec := &recordingIncomingExec{}

	var txid chainhash.Hash
	txid[0] = 0x52
	msg := &IncomingVTXOMsg{Event: newTestEvent(
		txid, 0, pkScript, 50_000, "round-durable-retry",
	)}

	_, err = behavior.Receive(t.Context(), msg, exec).Unpack()
	require.ErrorIs(t, err, actor.ErrPostponed)
	require.Zero(t, exec.commits)
	require.Empty(t, saver.saved)

	exec.failures = 1
	exec.err = errors.New("ack transaction unavailable")
	_, err = behavior.Receive(t.Context(), msg, exec).Unpack()
	require.ErrorIs(t, err, actor.ErrPostponed)
	require.Zero(t, exec.commits)
	require.Len(t, saver.saved, 1)

	_, err = behavior.Receive(t.Context(), msg, exec).Unpack()
	require.NoError(t, err)
	require.Equal(t, 1, exec.commits)
	require.Len(t, saver.saved, 1)
	require.Equal(t, int32(900_144), saver.saved[0].BatchExpiry)
}

// TestIncomingVTXODurableBehaviorReloadsTerminatedReplay proves a production
// upsert followed by redelivery uses the stored status and does not notify the
// manager to revive a VTXO that terminated after the first delivery.
func TestIncomingVTXODurableBehaviorReloadsTerminatedReplay(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pkScript := []byte{0x51, 0x20, 0xbc, 0xcd}
	saver := &mockVTXOSaver{succeedOnDuplicateSave: true}
	manager := &capturingManagerRef{}
	ownedScript := &OwnedReceiveScript{
		ClientKey: keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
		OperatorPubKey: operatorKey.PubKey(),
		ExitDelay:      144,
	}
	handler := NewIncomingVTXOHandler(
		IncomingVTXOHandlerConfig{
			ScriptStore: &mockScriptLookup{
				scripts: map[string]*OwnedReceiveScript{
					string(pkScript): ownedScript,
				},
			},
			VTXOStore:   saver,
			VTXOManager: manager,
			AncestryFetcher: func(context.Context, wire.OutPoint,
				[]byte, keychain.KeyDescriptor) (
				IncomingVTXOExtras, error) {

				return IncomingVTXOExtras{
					Output: &wire.TxOut{
						Value:    50_000,
						PkScript: pkScript,
					},
					Ancestry: []Ancestry{
						{},
					},
					BatchExpiry: 900_144,
				}, nil
			},
		},
	)
	behavior := &incomingVTXODurableBehavior{handler: handler}
	exec := &recordingIncomingExec{
		failures: 1,
		err:      errors.New("ack transaction unavailable"),
	}

	var txid chainhash.Hash
	txid[0] = 0x54
	msg := &IncomingVTXOMsg{Event: newTestEvent(
		txid, 0, pkScript, 50_000, "round-terminated-replay",
	)}

	_, err = behavior.Receive(t.Context(), msg, exec).Unpack()
	require.ErrorIs(t, err, actor.ErrPostponed)
	require.Len(t, manager.captured(), 1)
	require.Len(t, saver.saved, 1)

	saver.saved[0].Status = VTXOStatusSpent
	_, err = behavior.Receive(t.Context(), msg, exec).Unpack()
	require.NoError(t, err)
	require.Len(t, manager.captured(), 1)
	require.Equal(t, VTXOStatusSpent, saver.saved[0].Status)
}

// TestIncomingVTXOCodecRoundTrip proves the complete push event survives the
// durable-mailbox encoding used for restart redelivery.
func TestIncomingVTXOCodecRoundTrip(t *testing.T) {
	t.Parallel()

	var txid chainhash.Hash
	txid[0] = 0x53
	msg := &IncomingVTXOMsg{Event: newTestEvent(
		txid, 3, []byte{0x51, 0x20, 0xdd}, 75_000, "round-codec",
	)}
	codec := NewIncomingVTXOCodec()

	raw, err := codec.Encode(msg)
	require.NoError(t, err)
	decoded, err := codec.Decode(raw)
	require.NoError(t, err)
	decodedMsg, ok := decoded.(*IncomingVTXOMsg)
	require.True(t, ok)
	require.True(t, proto.Equal(msg.Event, decodedMsg.Event))
}

// newTestEvent creates an IncomingVTXOEvent with the given parameters.
func newTestEvent(txid chainhash.Hash, vout uint32, pkScript []byte,
	valueSat uint64, roundID string) *arkrpc.IncomingVTXOEvent {

	return &arkrpc.IncomingVTXOEvent{
		EventId: 1,
		Type:    arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED,
		Outpoint: &arkrpc.OutPoint{
			Txid: txid[:],
			Vout: vout,
		},
		PkScript:          pkScript,
		ValueSat:          valueSat,
		RoundId:           roundID,
		BatchExpiryHeight: 800_000,
		RelativeExpiry:    144,
		CommitmentTxid:    txid[:],
	}
}

// TestIncomingVTXOHandlerOwnedScript verifies that a VTXO_CREATED
// event for an owned script results in a persisted VTXO and
// manager notification.
func TestIncomingVTXOHandlerOwnedScript(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20, 0xaa, 0xbb}

	lookup := &mockScriptLookup{
		scripts: map[string]*OwnedReceiveScript{
			string(pkScript): {
				ClientKey: keychain.KeyDescriptor{
					PubKey: privKey.PubKey(),
					KeyLocator: keychain.KeyLocator{
						Family: 44,
						Index:  0,
					},
				},
				OperatorPubKey: operatorPriv.PubKey(),
				ExitDelay:      144,
			},
		},
	}
	saver := &mockVTXOSaver{}

	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		ScriptStore: lookup,
		VTXOStore:   saver,
		AncestryFetcher: func(context.Context, wire.OutPoint, []byte,
			keychain.KeyDescriptor) (IncomingVTXOExtras, error) {

			return IncomingVTXOExtras{
				Output: &wire.TxOut{
					Value:    50_000,
					PkScript: pkScript,
				},
				CreatedHeight: 799_500,
				BatchExpiry:   800_000,
				Ancestry: []Ancestry{
					{},
				},
			}, nil
		},
	})

	var txid chainhash.Hash
	txid[0] = 0x01

	evt := newTestEvent(txid, 0, pkScript, 50_000, "round-1")
	msg := &IncomingVTXOMsg{Event: evt}

	result := handler.Receive(t.Context(), msg)
	_, resultErr := result.Unpack()
	require.NoError(t, resultErr)

	require.Len(t, saver.saved, 1)

	desc := saver.saved[0]
	require.Equal(t, txid, desc.Outpoint.Hash)
	require.Equal(t, uint32(0), desc.Outpoint.Index)
	require.Equal(t, int64(50_000), int64(desc.Amount))
	require.Equal(t, pkScript, desc.PkScript)
	require.Equal(t, "round-1", desc.RoundID)
	require.Equal(t, int32(799_500), desc.CreatedHeight)
	require.Equal(t, VTXOStatusLive, desc.Status)

	expiryCfg := DefaultExpiryConfig()
	expiryCfg.MaxPaymentCLTV = 300
	require.False(t, expiryCfg.CanReserveMaxPaymentCLTV(desc))
}

// TestIncomingVTXOHandlerUnownedScript verifies that a VTXO_CREATED
// event for an unowned script is silently ignored.
func TestIncomingVTXOHandlerUnownedScript(t *testing.T) {
	t.Parallel()

	lookup := &mockScriptLookup{
		scripts: map[string]*OwnedReceiveScript{},
	}
	saver := &mockVTXOSaver{}

	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		ScriptStore: lookup,
		VTXOStore:   saver,
	})

	var txid chainhash.Hash
	txid[0] = 0x02

	evt := newTestEvent(
		txid, 0, []byte{0x51, 0x20, 0xff}, 10_000, "round-2",
	)
	msg := &IncomingVTXOMsg{Event: evt}

	result := handler.Receive(t.Context(), msg)
	_, resultErr := result.Unpack()
	require.NoError(t, resultErr)

	require.Empty(t, saver.saved)
}

// TestIncomingVTXOHandlerNonCreatedEvent verifies that non-CREATED
// event types are ignored.
func TestIncomingVTXOHandlerNonCreatedEvent(t *testing.T) {
	t.Parallel()

	saver := &mockVTXOSaver{}
	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		VTXOStore: saver,
	})

	evt := &arkrpc.IncomingVTXOEvent{
		EventId: 2,
		Type:    arkrpc.VTXOEventType_VTXO_EVENT_TYPE_STATUS_CHANGED,
	}
	msg := &IncomingVTXOMsg{Event: evt}

	result := handler.Receive(t.Context(), msg)
	_, resultErr := result.Unpack()
	require.NoError(t, resultErr)

	require.Empty(t, saver.saved)
}

// TestIncomingVTXOHandlerIgnoresEventBatchExpiry verifies that the thin push's
// unauthenticated scalar is never copied onto the descriptor.
func TestIncomingVTXOHandlerIgnoresEventBatchExpiry(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript := []byte{0x51, 0x20, 0xaa, 0xbb}

	output := &wire.TxOut{Value: 50_000, PkScript: pkScript}
	newLookup := func() *mockScriptLookup {
		return &mockScriptLookup{
			scripts: map[string]*OwnedReceiveScript{
				string(pkScript): {
					ClientKey: keychain.KeyDescriptor{
						PubKey: privKey.PubKey(),
						KeyLocator: keychain.KeyLocator{
							Family: 44,
							Index:  0,
						},
					},
					OperatorPubKey: operatorPriv.PubKey(),
					ExitDelay:      144,
				},
			},
		}
	}

	var txid chainhash.Hash
	txid[0] = 0x01

	for _, expiry := range []int32{0, -1} {
		t.Run(fmt.Sprintf("expiry %d", expiry), func(t *testing.T) {
			t.Parallel()

			saver := &mockVTXOSaver{}
			handler := NewIncomingVTXOHandler(
				IncomingVTXOHandlerConfig{
					ScriptStore: newLookup(),
					VTXOStore:   saver,
					AncestryFetcher: func(context.Context,
						wire.OutPoint, []byte,
						keychain.KeyDescriptor) (
						IncomingVTXOExtras, error) {

						return IncomingVTXOExtras{
							Output: output,
							Ancestry: []Ancestry{
								{},
							},
							BatchExpiry: 800_144,
						}, nil
					},
				},
			)

			evt := newTestEvent(
				txid, 0, pkScript, 50_000, "round-1",
			)
			evt.BatchExpiryHeight = expiry

			result := handler.Receive(
				t.Context(), &IncomingVTXOMsg{
					Event: evt,
				},
			)
			_, resultErr := result.Unpack()

			require.NoError(t, resultErr)
			require.Len(t, saver.saved, 1)
			require.Equal(
				t, int32(800_144), saver.saved[0].BatchExpiry,
			)
			require.Equal(
				t, uint32(144), saver.saved[0].RelativeExpiry,
			)
		})
	}
}

// TestIncomingVTXOHandlerRequiresAuthenticatedExpiry verifies that a thin
// push cannot create a new live row without the local authentication path.
func TestIncomingVTXOHandlerRequiresAuthenticatedExpiry(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pkScript := []byte{0x51, 0x20, 0xaa, 0xdd}
	saver := &mockVTXOSaver{}
	handler := NewIncomingVTXOHandler(
		IncomingVTXOHandlerConfig{
			ScriptStore: &mockScriptLookup{
				scripts: map[string]*OwnedReceiveScript{
					string(pkScript): {
						ClientKey: keychain.
							KeyDescriptor{
							PubKey: clientKey.
								PubKey(),
						},
						OperatorPubKey: operatorKey.
							PubKey(),
						ExitDelay: 144,
					},
				},
			},
			VTXOStore: saver,
		},
	)

	var txid chainhash.Hash
	txid[0] = 0x43
	_, err = handler.Receive(t.Context(), &IncomingVTXOMsg{
		Event: newTestEvent(txid, 0, pkScript, 50_000, "round-auth"),
	}).Unpack()
	require.ErrorContains(t, err, "ancestry fetcher not configured")
	require.Empty(t, saver.saved)
}

// TestIncomingVTXOHandlerNilEvent verifies that a nil event is
// handled gracefully.
func TestIncomingVTXOHandlerNilEvent(t *testing.T) {
	t.Parallel()

	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{})

	msg := &IncomingVTXOMsg{Event: nil}

	result := handler.Receive(t.Context(), msg)
	_, resultErr := result.Unpack()
	require.NoError(t, resultErr)
}

// TestIncomingVTXODurableBehaviorPostponeHorizon asserts a retryable failure is
// postponed only while the delivery is inside its horizon. Past the horizon the
// event is failed for real, so an outpoint that never appears in the indexer
// cannot keep a durable event, a paginated scan, and a warning log redriving
// forever.
func TestIncomingVTXODurableBehaviorPostponeHorizon(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pkScript := []byte{0x51, 0x20, 0xbb, 0xdd}
	saver := &mockVTXOSaver{}

	sink := actor.NewChannelTellOnlyRef[metrics.Msg]("test-metrics", 4)
	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		ScriptStore: &mockScriptLookup{
			scripts: map[string]*OwnedReceiveScript{
				string(pkScript): {
					ClientKey: keychain.KeyDescriptor{
						PubKey: clientKey.PubKey(),
					},
					OperatorPubKey: operatorKey.PubKey(),
					ExitDelay:      144,
				},
			},
		},
		VTXOStore: saver,
		AncestryFetcher: func(context.Context, wire.OutPoint, []byte,
			keychain.KeyDescriptor) (IncomingVTXOExtras, error) {

			return IncomingVTXOExtras{}, fmt.Errorf("%w: outpoint "+
				"never indexed", ErrIncomingAncestryNotFound)
		},
		MetricsSink: fn.Some[metrics.Sink](sink),
	})
	behavior := &incomingVTXODurableBehavior{handler: handler}
	exec := &recordingIncomingExec{}

	var txid chainhash.Hash
	txid[0] = 0x61
	msg := &IncomingVTXOMsg{Event: newTestEvent(
		txid, 0, pkScript, 50_000, "round-horizon",
	)}

	// Inside the horizon the delivery is parked, not failed.
	freshCtx := actor.WithDeliveryEnqueuedAtForTest(
		t.Context(), time.Now().Add(-time.Minute),
	)
	_, err = behavior.Receive(freshCtx, msg, exec).Unpack()
	require.ErrorIs(t, err, actor.ErrPostponed)
	require.ErrorIs(t, err, ErrIncomingAncestryNotFound)

	msg1, ok := sink.AwaitMessage(time.Second)
	require.True(t, ok)
	postponed, ok := msg1.(*metrics.DeliveryPostponeMsg)
	require.True(t, ok)
	require.Equal(t, metrics.PostponeCauseMissing, postponed.Cause)
	require.False(t, postponed.Expired)

	// Past the horizon the same failure is handed to the ordinary retry
	// path instead of being postponed again.
	staleCtx := actor.WithDeliveryEnqueuedAtForTest(
		t.Context(),
		time.Now().Add(-incomingVTXOPostponeHorizon-time.Minute),
	)
	_, err = behavior.Receive(staleCtx, msg, exec).Unpack()
	require.Error(t, err)
	require.NotErrorIs(t, err, actor.ErrPostponed)
	require.ErrorIs(t, err, ErrIncomingAncestryNotFound)

	msg2, ok := sink.AwaitMessage(time.Second)
	require.True(t, ok)
	expired, ok := msg2.(*metrics.DeliveryPostponeMsg)
	require.True(t, ok)
	require.Equal(t, metrics.PostponeCauseMissing, expired.Cause)
	require.True(t, expired.Expired)

	require.Empty(t, saver.saved)
	require.Zero(t, exec.commits)
}

// newPostponeTestBehavior builds a durable incoming-event behavior whose
// ancestry lookup always fails with fetchErr, plus the store it persists to and
// the sink it reports postpones to.
func newPostponeTestBehavior(t *testing.T, pkScript []byte, fetchErr error) (
	*incomingVTXODurableBehavior, *mockVTXOSaver,
	*actor.ChannelTellOnlyRef[metrics.Msg]) {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	saver := &mockVTXOSaver{}

	sink := actor.NewChannelTellOnlyRef[metrics.Msg]("test-metrics", 4)
	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		ScriptStore: &mockScriptLookup{
			scripts: map[string]*OwnedReceiveScript{
				string(pkScript): {
					ClientKey: keychain.KeyDescriptor{
						PubKey: clientKey.PubKey(),
					},
					OperatorPubKey: operatorKey.PubKey(),
					ExitDelay:      144,
				},
			},
		},
		VTXOStore: saver,
		AncestryFetcher: func(context.Context, wire.OutPoint, []byte,
			keychain.KeyDescriptor) (IncomingVTXOExtras, error) {

			return IncomingVTXOExtras{}, fetchErr
		},
		MetricsSink: fn.Some[metrics.Sink](sink),
	})

	return &incomingVTXODurableBehavior{handler: handler}, saver, sink
}

// TestIncomingVTXOPostponeHorizonPerCause asserts the horizon is sized to what
// each cause is actually waiting for. An indexer that has not caught up, or a
// chain source that could not be queried, gives up at the ten-minute bound. A
// commitment waiting for its first confirmation is still postponed well past
// that bound, because a single Bitcoin block can take longer than it, and only
// fails at the confirmation-sized bound.
func TestIncomingVTXOPostponeHorizonPerCause(t *testing.T) {
	t.Parallel()

	pkScript := []byte{0x51, 0x20, 0xbb, 0xdd}
	unconfirmed := fmt.Errorf("resolve commitment: %w: not yet mined",
		ErrCommitmentUnconfirmed)

	tests := []struct {
		name      string
		fetchErr  error
		age       time.Duration
		wantCause string
		expired   bool
	}{
		{
			name: "missing outpoint inside the bound",
			fetchErr: fmt.Errorf("%w: outpoint never "+
				"indexed", ErrIncomingAncestryNotFound),
			age:       time.Minute,
			wantCause: metrics.PostponeCauseMissing,
		},
		{
			name: "missing outpoint past the bound",
			fetchErr: fmt.Errorf("%w: outpoint never "+
				"indexed", ErrIncomingAncestryNotFound),
			age:       incomingVTXOPostponeHorizon + time.Minute,
			wantCause: metrics.PostponeCauseMissing,
			expired:   true,
		},
		{
			name: "indexer error past the bound",
			fetchErr: fmt.Errorf("%w: page failed",
				ErrIncomingAncestryIndexer),
			age:       incomingVTXOPostponeHorizon + time.Minute,
			wantCause: metrics.PostponeCauseIndexerError,
			expired:   true,
		},
		{
			// Past the indexer bound but inside the
			// confirmation bound: the event is still
			// confirming normally, so it must keep waiting.
			name:      "unconfirmed past the indexer bound",
			fetchErr:  unconfirmed,
			age:       incomingVTXOPostponeHorizon + time.Minute,
			wantCause: metrics.PostponeCauseChainUnconfirmed,
		},
		{
			name:     "unconfirmed past the confirmation bound",
			fetchErr: unconfirmed,
			age: incomingVTXOConfirmationHorizon +
				time.Minute,
			wantCause: metrics.PostponeCauseChainUnconfirmed,
			expired:   true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			behavior, saver, sink := newPostponeTestBehavior(
				t, pkScript, test.fetchErr,
			)
			exec := &recordingIncomingExec{}

			var txid chainhash.Hash
			txid[0] = 0x62
			msg := &IncomingVTXOMsg{Event: newTestEvent(
				txid, 0, pkScript, 50_000, "round-horizon",
			)}
			ctx := actor.WithDeliveryEnqueuedAtForTest(
				t.Context(), time.Now().Add(-test.age),
			)

			_, err := behavior.Receive(ctx, msg, exec).Unpack()
			require.ErrorIs(t, err, test.fetchErr)
			if test.expired {
				require.NotErrorIs(t, err, actor.ErrPostponed)
			} else {
				require.ErrorIs(t, err, actor.ErrPostponed)
			}

			reported, ok := sink.AwaitMessage(time.Second)
			require.True(t, ok)
			decision, ok := reported.(*metrics.DeliveryPostponeMsg)
			require.True(t, ok)
			require.Equal(t, test.wantCause, decision.Cause)
			require.Equal(t, test.expired, decision.Expired)

			require.Empty(t, saver.saved)
			require.Zero(t, exec.commits)
		})
	}
}

// TestIncomingPostponeCause asserts the postpone metric label is a fixed bucket
// derived from the lookup's own sentinels, never from the pushed event.
func TestIncomingPostponeCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "missing",
			err: fmt.Errorf("authenticate: %w",
				ErrIncomingAncestryNotFound),
			want: metrics.PostponeCauseMissing,
		},
		{
			name: "chain unconfirmed",
			err: fmt.Errorf("authenticate batch expiry: %w",
				ErrCommitmentUnconfirmed),
			want: metrics.PostponeCauseChainUnconfirmed,
		},
		{
			name: "indexer error",
			err:  ErrIncomingAncestryIndexer,
			want: metrics.PostponeCauseIndexerError,
		},
		{
			name: "store failure",
			err:  errors.New("database unavailable"),
			want: metrics.PostponeCauseOther,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, test.want, incomingPostponeCause(test.err),
			)
		})
	}
}
