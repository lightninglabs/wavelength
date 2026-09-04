package vtxo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
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
	saved    []*Descriptor
	failures int
	err      error
}

func (m *mockVTXOSaver) SaveVTXO(_ context.Context, desc *Descriptor) error {
	if m.failures > 0 {
		m.failures--

		return m.err
	}
	m.saved = append(m.saved, desc)

	return nil
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
			return IncomingVTXOExtras{BatchExpiry: 800_144}, nil
		},
	})

	var txid chainhash.Hash
	txid[0] = 0x42
	msg := IncomingVTXOMsg{Event: newTestEvent(
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
				CreatedHeight: 799_500,
				BatchExpiry:   800_000,
			}, nil
		},
	})

	var txid chainhash.Hash
	txid[0] = 0x01

	evt := newTestEvent(txid, 0, pkScript, 50_000, "round-1")
	msg := IncomingVTXOMsg{Event: evt}

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
	msg := IncomingVTXOMsg{Event: evt}

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
	msg := IncomingVTXOMsg{Event: evt}

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
						keychain.KeyDescriptor) (IncomingVTXOExtras,
						error) {

						return IncomingVTXOExtras{
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
				t.Context(), IncomingVTXOMsg{
					Event: evt,
				},
			)
			_, resultErr := result.Unpack()

			require.NoError(t, resultErr)
			require.Len(t, saver.saved, 1)
			require.Equal(t, int32(800_144),
				saver.saved[0].BatchExpiry)
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
	handler := NewIncomingVTXOHandler(IncomingVTXOHandlerConfig{
		ScriptStore: &mockScriptLookup{scripts: map[string]*OwnedReceiveScript{
			string(pkScript): {
				ClientKey:      keychain.KeyDescriptor{PubKey: clientKey.PubKey()},
				OperatorPubKey: operatorKey.PubKey(),
				ExitDelay:      144,
			},
		}},
		VTXOStore: saver,
	})

	var txid chainhash.Hash
	txid[0] = 0x43
	_, err = handler.Receive(t.Context(), IncomingVTXOMsg{
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

	msg := IncomingVTXOMsg{Event: nil}

	result := handler.Receive(t.Context(), msg)
	_, resultErr := result.Unpack()
	require.NoError(t, resultErr)
}
