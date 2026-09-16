package round

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestDurableRound retains temporary ownership before server admission.
func TestDurableRound(t *testing.T) {
	key, err := NewTempRoundKey()
	require.NoError(t, err)
	env := &ClientEnvironment{RoundKey: RoundKeyStr(key.KeyString())}
	machine := protofsm.NewInlineStateMachine(
		ClientStateMachineCfg{
			Logger:       btclog.Disabled,
			InitialState: &PendingRoundAssembly{},
			Env:          env,
		},
	)
	machine.Start(t.Context())
	defer machine.Stop()
	original := &RoundFSM{
		env: env, FSM: &machine, Key: key,
		Admission: &roundpb.ServiceAdmission{},
		Operation: &roundpb.OperationStatus{
			OperationId: []byte{
				1,
				2,
				3,
			},
		},
	}
	raw, err := encodeDurableRound(t.Context(), original)
	require.NoError(t, err)
	restored, err := decodeDurableRound(raw, &ClientEnvironment{})
	require.NoError(t, err)
	require.Equal(t, key, restored.round.Key)
	require.NotNil(t, restored.round.Admission)
	require.True(
		t, proto.Equal(original.Operation, restored.round.Operation),
	)
	require.IsType(t, &PendingRoundAssembly{}, restored.state)
	require.False(t, restored.lostSignerSessions)
	require.Nil(t, restored.round.FSM)
}

// TestDurableRoundColdSigning makes lost external sessions visible to startup.
func TestDurableRoundColdSigning(t *testing.T) {
	id := RoundID{1}
	env := &ClientEnvironment{RoundKey: RoundKeyStr(id.KeyString())}
	machine := protofsm.NewInlineStateMachine(
		ClientStateMachineCfg{
			Logger:       btclog.Disabled,
			InitialState: &NoncesSentState{},
			Env:          env,
		},
	)
	machine.Start(t.Context())
	defer machine.Stop()
	original := &RoundFSM{env: env, FSM: &machine, Key: id, RoundID: id}
	raw, err := encodeDurableRound(t.Context(), original)
	require.NoError(t, err)
	restored, err := decodeDurableRound(raw, &ClientEnvironment{})
	require.NoError(t, err)
	require.True(t, restored.lostSignerSessions)
	require.Nil(t, restored.round.FSM)
	require.IsType(t, &NoncesSentState{}, restored.state)
	original.RoundID = RoundID{2}
	raw, err = encodeDurableRound(t.Context(), original)
	require.NoError(t, err)
	_, err = decodeDurableRound(raw, &ClientEnvironment{})
	require.ErrorContains(t, err, "round key and ID differ")
}
