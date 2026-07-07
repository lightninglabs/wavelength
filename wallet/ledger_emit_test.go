package wallet

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/ledger"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// walletWithLedgerSink constructs a minimal Ark actor shell
// with the supplied ledger sink. The fields required by
// emitUTXOCreated are ledgerSink + actorLog (via the logger
// helper), so everything else can stay nil.
func walletWithLedgerSink(sink fn.Option[ledger.Sink]) *Ark {
	return &Ark{
		ledgerSink: sink,
		actorLog:   fn.Some[btclog.Logger](btclog.Disabled),
	}
}

// TestEmitUTXOCreatedForwardsClassification confirms the
// wallet helper forwards every UTXO field verbatim and carries
// the caller-supplied classification through to the ledger
// message. The helper does not infer classifications itself;
// its only jobs are null-safety and shape translation.
func TestEmitUTXOCreatedForwardsClassification(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	utxo := &Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x11,
			},
			Index: 7,
		},
		Amount:        btcutil.Amount(42_000),
		Confirmations: 3,
	}

	a.emitUTXOCreated(
		t.Context(), utxo, 800_123, ledger.ClassificationDeposit,
	)

	select {
	case raw := <-sink.Messages():
		msg, ok := raw.(*ledger.UTXOCreatedMsg)
		require.True(t, ok, "expected UTXOCreatedMsg, got %T", raw)

		require.Equal(
			t, [32]byte(utxo.Outpoint.Hash), msg.OutpointHash,
		)
		require.Equal(t, uint32(7), msg.OutpointIndex)
		require.Equal(t, int64(42_000), msg.AmountSat)
		require.Equal(t, uint32(800_123), msg.BlockHeight)
		require.Equal(
			t, ledger.ClassificationDeposit, msg.Classification,
		)

	default:
		t.Fatalf("no ledger emission")
	}
}

// TestEmitUTXOCreatedNegativeHeight covers the height guard:
// the block height column is unsigned but some backends pass
// -1 for unconfirmed observations; the helper clamps to 0
// rather than wrapping to uint32 max via a direct cast.
func TestEmitUTXOCreatedNegativeHeight(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	utxo := &Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x22,
			},
		},
		Amount: btcutil.Amount(1_000),
	}

	a.emitUTXOCreated(
		t.Context(), utxo, -1, ledger.ClassificationDeposit,
	)

	raw := <-sink.Messages()
	msg, ok := raw.(*ledger.UTXOCreatedMsg)
	require.True(t, ok, "expected UTXOCreatedMsg, got %T", raw)
	require.Equal(
		t, uint32(0), msg.BlockHeight,
		"negative height must clamp to 0, not wrap",
	)
}

// TestEmitUTXOCreatedNilUTXO is a null-safety regression: a
// caller that forgets to populate utxo must not panic. The
// helper returns cleanly without touching the sink.
func TestEmitUTXOCreatedNilUTXO(t *testing.T) {
	t.Parallel()

	sink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"ledger-capture", 4,
	)
	a := walletWithLedgerSink(fn.Some[ledger.Sink](sink))

	a.emitUTXOCreated(
		t.Context(), nil, 800_000, ledger.ClassificationDeposit,
	)

	select {
	case msg := <-sink.Messages():
		t.Fatalf("unexpected emission on nil utxo: %T", msg)

	default:
	}
}

// TestEmitUTXOCreatedNoSink confirms the fn.None sink path is
// a silent no-op. Used by harnesses and unit tests that do
// not register a ledger actor.
func TestEmitUTXOCreatedNoSink(t *testing.T) {
	t.Parallel()

	a := walletWithLedgerSink(fn.None[ledger.Sink]())

	utxo := &Utxo{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				0x33,
			},
		},
		Amount: btcutil.Amount(500),
	}

	// Simply must not panic.
	a.emitUTXOCreated(
		t.Context(), utxo, 100, ledger.ClassificationDeposit,
	)
}
