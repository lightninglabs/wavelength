package round

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/baselib/protofsm"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/bip322"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/lightninglabs/wavelength/wallet"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestAutomaticRefreshProofBudget verifies that independent cohorts never
// coalesce past the verifier budget. Deferred claims are returned for retry;
// after each simulated completion every original claim is submitted once with
// a same-value, same-owner replacement. Real settlement is covered separately.
func TestAutomaticRefreshProofBudget(t *testing.T) {
	for _, count := range []int{1, 127, 128, 129, 264, 265} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			assertAutomaticRefreshProofBudget(t, count)
		})
	}
}

// assertAutomaticRefreshProofBudget drains the supplied wallet size through
// the real round actor, modelling completion between bounded admissions.
func assertAutomaticRefreshProofBudget(t *testing.T, count int) {
	t.Helper()
	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())
	owner := h.newKeyDescriptor()
	policy, err := arkscript.EncodeStandardVTXOTemplate(
		owner.PubKey, h.operatorPubKey, 144,
	)
	require.NoError(t, err)
	remaining := make(
		map[wire.OutPoint]*RefreshVTXORequest, count,
	)
	for i := range count {
		op := wire.OutPoint{
			Hash: chainhash.HashH(
				[]byte(
					fmt.Sprint(i),
				),
			),
		}
		h.vtxoStore.
			On("GetVTXO", mock.Anything, op).
			Return(
				&ClientVTXO{
					Outpoint: op,
					Amount: btcutil.Amount(
						50_000,
					),
				},
				nil,
			)
		remaining[op] = &RefreshVTXORequest{
			VTXOOutpoint: op, Amount: 50_000, Automatic: true,
			PolicyTemplate: policy, OwnerKey: owner,
		}
	}
	for attempt := 0; len(remaining) > 0; attempt++ {
		require.Less(t, attempt, (count+127)/128)
		var requests []*RefreshVTXORequest
		for _, req := range remaining {
			requests = append(requests, req)
		}
		releasesBefore := len(
			h.vtxoManager.getMessages(),
		)
		for start := 0; start < len(requests); start += 32 {
			result := h.receive(
				&RefreshVTXOCohortRequest{
					Requests: requests[start:min(
						start+
							32,
						len(
							requests,
						),
					)],
				},
			)
			require.NoError(t, result.Err())
		}
		states := h.queryState()
		require.Len(t, states, 1)
		for key, info := range states {
			assembly, ok := info.State.(*PendingRoundAssembly)
			require.True(t, ok)
			require.LessOrEqual(
				t, len(assembly.Forfeits), 128,
			)
			require.Len(
				t, assembly.VTXOs, len(assembly.Forfeits),
			)
			deferred := make(map[wire.OutPoint]bool)
			releases := h.vtxoManager.getMessages()[releasesBefore:]
			for _, msg := range releases {
				release, ok :=
					msg.(*actormsg.ReleaseForfeitRequest)
				require.True(t, ok)
				for _, op := range release.Outpoints {
					require.False(
						t, deferred[op],
					)
					deferred[op] = true
				}
			}
			require.Equal(
				t, len(remaining), len(assembly.Forfeits)+
					len(deferred),
			)
			result := h.receive(
				&TimeoutMsg{
					TimeoutID: makeTimeoutID(
						RoundKeyStr(
							key,
						),
						TimeoutPhaseRefreshRegistration,
					),
				},
			)
			require.NoError(t, result.Err())
			messages := h.serverMessages()
			require.Len(t, messages, attempt+1)
			message := messages[attempt]
			send, ok := message.(*serverconn.SendClientEventRequest)
			require.True(t, ok)
			join, ok := send.Message.(*JoinRoundRequest)
			require.True(t, ok)
			for _, forfeit := range join.ForfeitRequests {
				op := *forfeit.VTXOOutpoint
				require.Contains(
					t, remaining, op,
					"claim submitted twice",
				)
				require.False(
					t, deferred[op],
					"adopted claim released",
				)
				delete(remaining, op)
			}
			for _, output := range join.VTXORequests {
				require.EqualValues(
					t, 50_000,
					output.Amount,
				)
				require.True(
					t, output.OwnerKey.PubKey.IsEqual(
						owner.PubKey,
					),
				)
			}
			// A late cohort must not start a second
			// registration while this request is
			// active, even though its assembly
			// timer already fired.
			for _, req := range remaining {
				result := h.receive(req)
				require.NoError(t, result.Err())
				require.Len(
					t, h.queryState(), 1,
				)
				require.Len(
					t, h.serverMessages(),
					attempt+1,
				)

				break
			}
			// Model successful completion, which
			// removes the round actor entry.
			h.actor.rounds[RoundKeyStr(key)].FSM.Stop()
			delete(h.actor.rounds, RoundKeyStr(key))
		}
	}
}

// TestRefreshProofBudgetIncludesBoarding proves that boarding inputs consume
// the same proof budget and a full assembly is preserved when refresh defers.
func TestRefreshProofBudgetIncludesBoarding(t *testing.T) {
	h := newActorTestHarness(t)
	h.setupMockRoundStoreForStart()
	require.NoError(t, h.start())
	r, err := h.actor.createNewRound(h.ctx)
	require.NoError(t, err)
	var boarding []BoardingIntent
	for i := range 127 {
		boarding = append(
			boarding, BoardingIntent{
				BoardingIntent: wallet.BoardingIntent{
					Outpoint: wire.OutPoint{
						Index: uint32(i),
					},
				},
			},
		)
	}
	require.NoError(
		t,
		h.actor.askEventAndProcessOutbox(
			h.ctx, r, &IntentPackage{
				Intents: Intents{Boarding: boarding},
			},
		),
	)
	same, err := h.actor.findRefreshRound(h.ctx, 1)
	require.NoError(t, err)
	require.Same(t, r, same)
	_, err = h.actor.findRefreshRound(h.ctx, 2)
	require.ErrorIs(t, err, errRefreshRoundBusy)
	require.Len(t, h.queryState(), 1)
}

// TestJoinAuthProofInputBoundary reproduces a correctly signed large request
// rejected by the unchanged verifier. The 128-input request passes full script
// verification; larger requests must be partitioned before auth construction.
func TestJoinAuthProofInputBoundary(t *testing.T) {
	for _, count := range []int{128, 129, 264, 265} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newJoinAuthTestFixture(t)
			var intents Intents
			prevouts := make(map[wire.OutPoint]*wire.TxOut, count)
			for i := range count {
				boarding := f.newBoardingIntent(t, 50_000)
				boarding.Outpoint.Index = uint32(i)
				op := boarding.Outpoint
				boarding.Request.Outpoint = &op
				intents.Boarding = append(
					intents.Boarding, boarding,
				)
				script, err := txscript.PayToAddrScript(
					boarding.Address.Address,
				)
				require.NoError(t, err)
				prevouts[op] = &wire.TxOut{
					Value:    50_000,
					PkScript: script,
				}
			}
			intents.VTXOs = []types.VTXORequest{
				f.newVTXORequest(
					t, btcutil.Amount(count*50_000),
				),
			}
			auth, err := buildJoinRoundAuth(
				f.ctx, f.env, f.identifierKeyDesc(), intents,
				intents.VTXOs, nil, nil,
			)
			require.NoError(t, err)
			result := f.validateAuth(t, auth, prevouts)
			if count == 128 {
				require.Equal(
					t, bip322.VerificationStateValid,
					result.State, result.Reason,
				)
			} else {
				require.Equal(
					t, bip322.VerificationStateInvalid,
					result.State,
				)
				require.Contains(
					t, result.Reason, fmt.Sprintf("proof "+
						"input count %d "+
						"exceeds max 128", count),
				)
			}
		})
	}
}

// TestRefreshWaitsOnlyForUnsealedRounds prevents a large wallet from replacing
// its own registration while allowing eligible claims to progress once the
// preceding round has a commitment, even if its confirmation is delayed.
func TestRefreshWaitsOnlyForUnsealedRounds(t *testing.T) {
	for _, state := range []ClientState{
		&IntentSentState{}, &QuoteReceivedState{}, &RoundJoinedState{},
		&CommitmentTxReceivedState{}, &InputSigSentState{},
		&ClientFailedState{},
	} {
		t.Run(fmt.Sprintf("%T", state), func(t *testing.T) {
			h := newActorTestHarness(t)
			h.setupMockRoundStoreForStart()
			require.NoError(t, h.start())
			fsm := protofsm.NewStateMachine(ClientStateMachineCfg{
				Logger: btclog.Disabled,
				ErrorReporter: newLoggerErrorReporter(
					btclog.Disabled,
				),
				InitialState: state, Env: h.actor.env,
			})
			h.actor.startRoundFSM(h.ctx, &fsm)
			t.Cleanup(fsm.Stop)
			key := TempRoundKey(uuid.New())
			keyStr := RoundKeyStr(key.KeyString())
			h.actor.rounds[keyStr] = &RoundFSM{
				FSM: &fsm,
				Key: key,
			}
			_, err := h.actor.findRefreshRound(h.ctx, 32)
			switch state.(type) {
			case *IntentSentState, *QuoteReceivedState,
				*RoundJoinedState:

				require.ErrorIs(t, err, errRefreshRoundBusy)

			default:
				require.NoError(t, err)
			}
		})
	}
}
