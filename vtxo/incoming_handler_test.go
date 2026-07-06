package vtxo

import (
	"context"
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
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
	saved []*Descriptor
}

func (m *mockVTXOSaver) SaveVTXO(_ context.Context, desc *Descriptor) error {
	m.saved = append(m.saved, desc)

	return nil
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
	require.Equal(t, VTXOStatusLive, desc.Status)
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
