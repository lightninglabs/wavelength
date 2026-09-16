package round

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/stretchr/testify/require"
)

// TestDurableActorSnapshot retains routing across concurrent round assembly.
func TestDurableActorSnapshot(t *testing.T) {
	temporary, err := NewTempRoundKey()
	require.NoError(t, err)
	assigned := RoundID{7}
	txid := chainhash.Hash{8}
	actor := &RoundClientActor{
		rounds: make(map[RoundKeyStr]*RoundFSM),
		pendingQuotes: map[RoundID]*JoinRoundQuoteReceived{
			assigned: {
				RoundID: assigned,
				Quote:   &ClientQuote{},
			},
		},
	}
	for _, key := range []RoundKey{temporary, assigned} {
		env := &ClientEnvironment{
			RoundKey: RoundKeyStr(key.KeyString()),
		}
		machine := protofsm.NewInlineStateMachine(
			ClientStateMachineCfg{
				Logger:       btclog.Disabled,
				InitialState: &PendingRoundAssembly{},
				Env:          env,
			},
		)
		machine.Start(t.Context())
		t.Cleanup(machine.Stop)
		round := &RoundFSM{env: env, FSM: &machine, Key: key}
		if !key.IsTemp() {
			round.RoundID = assigned
			round.TxID = txid
		}
		actor.rounds[RoundKeyStr(key.KeyString())] = round
	}
	raw, err := actor.encodeActorSnapshot(t.Context())
	require.NoError(t, err)
	snapshot, err := decodeActorSnapshot(raw, &ClientEnvironment{})
	require.NoError(t, err)
	require.Len(t, snapshot.rounds, 2)
	require.Equal(
		t,
		RoundKeyStr(
			assigned.KeyString(),
		),
		snapshot.commitmentTxIndex[txid],
	)
	require.Contains(t, snapshot.rounds, RoundKeyStr(temporary.KeyString()))
	require.Equal(t, assigned, snapshot.pendingQuotes[assigned].RoundID)
	again, err := actor.encodeActorSnapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, raw, again)

	// A quote stored under the wrong round must fail before state
	// installation.
	actor.pendingQuotes[assigned].RoundID = RoundID{9}
	raw, err = actor.encodeActorSnapshot(t.Context())
	require.NoError(t, err)
	_, err = decodeActorSnapshot(raw, &ClientEnvironment{})
	require.Error(t, err)
}
