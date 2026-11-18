package lnruntime

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// abortClosePeer exposes only the pre-signature rollback operation.
type abortClosePeer struct {
	ProcessCooperativeClosePeer
	abort func(context.Context) error
}

// AbortCooperativeClose awaits the test's hub acknowledgement.
func (p abortClosePeer) AbortCooperativeClose(ctx context.Context,
	_ arkchannel.ID) error {

	return p.abort(ctx)
}

// unsignedAbortSink records a local abort after a positive remote decision.
type unsignedAbortSink struct {
	archivedCloseSink
}

// RecordLocalEvent applies the same unsigned transition as durable storage.
func (s *unsignedAbortSink) RecordLocalEvent(ctx context.Context,
	_ arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	if err := ctx.Err(); err != nil {
		return arkchannel.Record{}, err
	}
	state, err := arkchannel.RestoreState(s.record.Snapshot)
	if err != nil {
		return arkchannel.Record{}, err
	}
	next, err := state.ProcessEvent(ctx, event, nil)
	if err != nil {
		return arkchannel.Record{}, err
	}
	updated, ok := next.NextState.(arkchannel.State)
	if !ok {
		return arkchannel.Record{}, errors.New("unexpected next state")
	}
	s.record.Snapshot = updated.Snapshot()

	return s.record, nil
}

// TestUnsignedCloseAbortDeadline checks RPC expiry and deadline-free replay.
// An unavailable hub must leave the unsigned attempt durable for recovery.
func TestUnsignedCloseAbortDeadline(t *testing.T) {
	t.Parallel()

	s := archivedCloseSnapshot(t, true)
	s.Phase = arkchannel.PhaseCoopClosing
	s.CooperativeClose = nil
	for _, rpc := range []bool{true, false} {
		name := "replay without hub acknowledgement"
		if rpc {
			name = "expired RPC with hub acknowledgement"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				checkUnsignedCloseDeadline(t, s, rpc)
			})
		})
	}
}

// checkUnsignedCloseDeadline uses virtual time for both the drain and abort.
func checkUnsignedCloseDeadline(t *testing.T, s arkchannel.Snapshot, rpc bool) {
	t.Helper()

	ctx := t.Context()
	budget := unsignedCloseDrainTimeout
	if rpc {
		budget = time.Second
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	start := time.Now()
	sink := &unsignedAbortSink{}
	sink.record.Snapshot = s.Clone()
	wait := func(ctx context.Context, _ arkchannel.ID, _ []byte) error {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.Equal(t, start.Add(budget), deadline)
		<-ctx.Done()

		return ctx.Err()
	}
	calls := 0
	abort := func(ctx context.Context) error {
		calls++
		require.NoError(t, ctx.Err())
		end, ok := ctx.Deadline()
		require.True(t, ok)
		require.Equal(t, unsignedCloseAbortTimeout, time.Until(end))
		if rpc {
			return nil
		}
		<-ctx.Done()

		return ctx.Err()
	}
	process := &ClientCooperativeCloseProcess{
		local: &NativeCooperativeCloseEndpoint{
			party: arkchannel.PartyClient,
			runtime: &Runtime{
				switcher: &htlcswitch.Switch{},
			},
			sink:     sink,
			delivery: CooperativeCloseDeliveryValidatorFunc(wait),
		},
		peer: abortClosePeer{
			abort: abort,
		},
	}
	err := process.NegotiateCooperativeClose(
		ctx, s.Terms.ID, s.Terms, *s.Source, *s.Backing,
		*s.CooperativeCloseRequest,
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, calls)
	if rpc {
		require.Equal(
			t, arkchannel.PhaseActive, sink.record.Snapshot.Phase,
		)
	} else {
		require.Equal(t, s, sink.record.Snapshot)
	}
}

// archivedCloseSink exposes durable facts but rejects any further lifecycle
// events: defense must not reopen or republish an already archived channel.
type archivedCloseSink struct {
	CooperativeCloseStateSink

	record arkchannel.Record
}

// GetChannel returns an isolated copy of the archived channel facts.
func (s *archivedCloseSink) GetChannel(_ context.Context, id arkchannel.ID) (
	arkchannel.Record, error) {

	if id != s.record.Snapshot.Terms.ID {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}
	record := s.record
	record.Snapshot = record.Snapshot.Clone()

	return record, nil
}

// TestHubRefreshPolicyBeforeSigning checks that a valid quote from before a
// rotation cannot authorize the hub to issue a new signature.
func TestHubRefreshPolicyBeforeSigning(t *testing.T) {
	t.Parallel()

	s := archivedCloseSnapshot(t, true)
	request := s.CooperativeCloseRequest.Clone()
	proposal := s.CooperativeClose.Proposal.Clone()
	require.NoError(t, proposal.Validate(s.Terms, *s.Source, request))
	sink := &archivedCloseSink{}
	sink.record.Snapshot = arkchannel.Snapshot{
		Phase: arkchannel.PhaseActive, Terms: s.Terms,
		Source: s.Source, Backing: s.Backing,
	}
	current := testIntentKey(t).PubKey()
	registered := func(encoded []byte) []byte {
		owner, err := arkchannel.CooperativeCloseDeliveryOwner(encoded)
		require.NoError(t, err)
		script, err := arkscript.EncodeStandardVTXOTemplate(
			owner, current, s.Terms.VTXO.MinExitDelay,
		)
		require.NoError(t, err)

		return script
	}
	hub := registered(request.HubDeliveryScript)
	process := &HubCooperativeCloseProcess{
		local: &NativeCooperativeCloseEndpoint{
			party: arkchannel.PartyHub, sink: sink,
			delivery: exactCooperativeCloseDeliveryValidator(
				request.HubDeliveryScript,
			),
		},
		delivery: CooperativeCloseDeliverySourceFunc(func(
			context.Context, arkchannel.ID) ([]byte, error) {

			return hub, nil
		}),
	}
	quote, err := process.GetCooperativeCloseTerms(
		t.Context(), s.Terms.ID,
		registered(request.ClientDeliveryScript),
	)
	require.NoError(t, err)
	require.Equal(t, hub, quote.HubDeliveryScript)
	_, err = process.CompleteCooperativeClose(
		t.Context(), s.Terms.ID, request, proposal,
	)
	require.ErrorContains(t, err, "hub refresh policy changed since quote")
	require.Nil(t, sink.record.Snapshot.CooperativeCloseRequest)
}

// TestClientArchivedCloseSourceSpend proves replay of a source-spend defense
// action never tries to quiesce a channel already removed from native lnd.
func TestClientArchivedCloseSourceSpend(t *testing.T) {
	t.Parallel()

	snapshot := archivedCloseSnapshot(t, true)
	state, err := arkchannel.RestoreState(snapshot)
	require.NoError(t, err)
	transition, err := state.ProcessEvent(
		t.Context(), &arkchannel.SourceSpent{
			OutPoint:     snapshot.Source.OutPoint,
			SpendingTxID: chainhash.Hash{9, 1},
		}, nil,
	)
	require.NoError(t, err)
	nextState, ok := transition.NextState.(arkchannel.State)
	require.True(t, ok)
	snapshot = nextState.Snapshot()
	require.Equal(t, arkchannel.PhaseClosed, snapshot.Phase)
	action, err := arkchannel.PendingAction(snapshot)
	require.NoError(t, err)
	publish, ok := action.(*arkchannel.PublishCooperativeClose)
	require.True(t, ok)

	var validated int
	endpoint := &NativeCooperativeCloseEndpoint{
		party: arkchannel.PartyClient,
		delivery: CooperativeCloseDeliveryValidatorFunc(func(
			_ context.Context, id arkchannel.ID,
			script []byte) error {

			require.Equal(t, snapshot.Terms.ID, id)
			require.Equal(
				t, snapshot.CooperativeCloseRequest.
					ClientDeliveryScript,
				script,
			)
			validated++

			return nil
		}),
	}
	process := &ClientCooperativeCloseProcess{local: endpoint}
	sink := &archivedCloseSink{
		record: arkchannel.Record{
			Revision: 10,
			Snapshot: snapshot,
		},
	}
	require.NoError(t, process.BindChannelEventSink(sink))
	for range 2 {
		require.NoError(
			t,
			process.PublishCooperativeClose(
				t.Context(), snapshot.Terms.ID, publish.Terms,
				publish.Source, publish.Close,
			),
		)
	}
	require.Equal(t, 2, validated)
	require.Equal(t, snapshot, sink.record.Snapshot)
}

// TestClientArchivedCloseRejectsChangedPayout keeps the wallet handoff bound
// to the durable, fully finalized authorization and local replacement owner.
func TestClientArchivedCloseRejectsChangedPayout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*arkchannel.CooperativeClose)
		walletErr error
		wantErr   string
	}{
		{
			name: "different settlement",
			mutate: func(settlement *arkchannel.CooperativeClose) {
				settlement.TxID[0] ^= 1
			},
			wantErr: "does not match finalized settlement",
		},
		{
			name: "changed authorization",
			mutate: func(settlement *arkchannel.CooperativeClose) {
				settlement.Transaction[0] ^= 1
			},
			wantErr: "validate client archived close",
		},
		{
			name:      "replacement not wallet owned",
			walletErr: errors.New("payout is not wallet owned"),
			wantErr:   "payout is not wallet owned",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := archivedCloseSnapshot(t, true)
			settlement := snapshot.CooperativeClose.Clone()
			if test.mutate != nil {
				test.mutate(&settlement)
			}
			checkPayout := func(context.Context, arkchannel.ID,
				[]byte) error {

				return test.walletErr
			}
			endpoint := &NativeCooperativeCloseEndpoint{
				party: arkchannel.PartyClient,
				delivery: CooperativeCloseDeliveryValidatorFunc(
					checkPayout,
				),
			}
			process := &ClientCooperativeCloseProcess{
				local: endpoint,
			}
			sink := &archivedCloseSink{
				record: arkchannel.Record{
					Snapshot: snapshot,
				},
			}
			require.NoError(t, process.BindChannelEventSink(sink))
			err := process.PublishCooperativeClose(
				t.Context(), snapshot.Terms.ID, snapshot.Terms,
				*snapshot.Source, settlement,
			)
			require.ErrorContains(t, err, test.wantErr)
			require.Equal(t, snapshot, sink.record.Snapshot)
		})
	}
}

// TestClientSignedCloseStillRevalidates proves a source conflict must not skip
// native safety checks while the signed refresh has not durably finalized.
func TestClientSignedCloseStillRevalidates(t *testing.T) {
	t.Parallel()

	snapshot := archivedCloseSnapshot(t, true)
	snapshot.Phase = arkchannel.PhaseCoopCloseSigned

	snapshot.SourceConflict = &arkchannel.SourceConflict{
		OutPoint: snapshot.Source.OutPoint,
		SpendingTxID: chainhash.Hash{
			9,
			1,
		},
	}
	action, err := arkchannel.PendingAction(snapshot)
	require.NoError(t, err)
	quiesceErr := errors.New("native payout revalidation required")
	process := &ClientCooperativeCloseProcess{
		local: &NativeCooperativeCloseEndpoint{
			party: arkchannel.PartyClient,
			delivery: CooperativeCloseDeliveryValidatorFunc(func(
				context.Context, arkchannel.ID, []byte) error {

				return quiesceErr
			}),
		},
	}
	sink := &archivedCloseSink{
		record: arkchannel.Record{
			Snapshot: snapshot,
		},
	}
	require.NoError(t, process.BindChannelEventSink(sink))
	executor := &cooperativeCloseActionExecutor{closer: process}
	err = executor.Execute(t.Context(), snapshot.Terms.ID, action)
	require.ErrorIs(t, err, quiesceErr)
	require.ErrorContains(t, err, "revalidate cooperative close state")
	require.Equal(t, snapshot, sink.record.Snapshot)
}

// interruptedClosePeer rejects finalization before any native archival.
type interruptedClosePeer struct {
	ProcessCooperativeClosePeer

	err   error
	calls int
}

// PublishCooperativeClose models a hub whose OOR completion is not yet proven.
func (p *interruptedClosePeer) PublishCooperativeClose(context.Context,
	arkchannel.ID, chainhash.Hash) (chainhash.Hash, error) {

	p.calls++

	return chainhash.Hash{}, p.err
}

// TestClientPublishedCloseStillRequiresPeer preserves the peer completion
// barrier when only local OOR finalization has been recorded.
func TestClientPublishedCloseStillRequiresPeer(t *testing.T) {
	t.Parallel()

	snapshot := archivedCloseSnapshot(t, true)
	snapshot.Phase = arkchannel.PhaseCoopClosePublished

	snapshot.SourceConflict = &arkchannel.SourceConflict{
		OutPoint: snapshot.Source.OutPoint,
		SpendingTxID: chainhash.Hash{
			9,
			1,
		},
	}
	action, err := arkchannel.PendingAction(snapshot)
	require.NoError(t, err)
	require.IsType(t, &arkchannel.FinalizeCooperativeClose{}, action)
	peer := &interruptedClosePeer{
		err: errors.New("hub OOR completion unavailable"),
	}
	process := &ClientCooperativeCloseProcess{
		local: &NativeCooperativeCloseEndpoint{
			party: arkchannel.PartyClient,
		},
		peer: peer,
	}
	sink := &archivedCloseSink{
		record: arkchannel.Record{
			Snapshot: snapshot,
		},
	}
	require.NoError(t, process.BindChannelEventSink(sink))
	executor := &cooperativeCloseActionExecutor{closer: process}
	err = executor.Execute(t.Context(), snapshot.Terms.ID, action)
	require.ErrorIs(t, err, peer.err)
	require.Equal(t, 1, peer.calls)
	require.Equal(t, snapshot, sink.record.Snapshot)
}

// TestHubWithoutPayoutWaitsForNotice pins the one case where consulting the
// hub's own wallet cannot establish completion, even after restart.
func TestHubWithoutPayoutWaitsForNotice(t *testing.T) {
	t.Parallel()

	snapshot := archivedCloseSnapshot(t, false)
	snapshot.Phase = arkchannel.PhaseCoopCloseSigned
	require.Zero(t, snapshot.CooperativeClose.Proposal.HubOutput)
	local := &NativeCooperativeCloseEndpoint{
		sink: &archivedCloseSink{
			record: arkchannel.Record{
				Snapshot: snapshot,
			},
		},
	}
	process := &HubCooperativeCloseProcess{local: local}
	done, err := process.resumeSettlement(
		t.Context(), snapshot.Terms.ID, snapshot.CooperativeClose.TxID,
		false,
	)
	require.NoError(t, err)
	require.False(t, done)
}

// archivedCloseSnapshot creates an archived close without on-chain
// confirmation.
func archivedCloseSnapshot(t *testing.T,
	hubHasPayout bool) arkchannel.Snapshot {

	t.Helper()

	terms, _, clientKey, hubKey := statusPollChannel(t)
	hubArkKey := testIntentKey(t)
	terms.VTXO.HubArkKey = compressedIntentKey(hubArkKey)
	source := *testIntentBinding(t, terms, terms.Capacity+1_000, 0)
	backing := statusPollBacking(t, terms, source, clientKey, hubKey)
	request := arkchannel.CooperativeCloseRequest{
		Initiator:            arkchannel.PartyClient,
		ClientDeliveryScript: clientKey.PubKey().SerializeCompressed(),
		HubDeliveryScript:    hubKey.PubKey().SerializeCompressed(),
	}
	hubBalance := terms.Capacity / 2
	if !hubHasPayout {
		hubBalance = 0
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		terms, source, request, terms.Capacity-hubBalance, hubBalance,
		7,
	)
	require.NoError(t, err)
	key := keychain.KeyDescriptor{PubKey: hubArkKey.PubKey()}
	signer := input.NewMockSigner([]*btcec.PrivateKey{hubArkKey}, nil)
	checkpointDesc, err := template.SignDescriptor(
		terms, arkchannel.PartyHub, key,
	)
	require.NoError(t, err)
	checkpoint, err := psbtutil.Parse(template.Proposal().Transaction)
	require.NoError(t, err)
	checkpointSig, err := signer.SignOutputRaw(
		checkpoint.UnsignedTx, checkpointDesc,
	)
	require.NoError(t, err)
	payoutDesc, err := template.ArkSignDescriptor(
		terms, arkchannel.PartyHub, key,
	)
	require.NoError(t, err)
	payoutSig, err := signer.SignOutputRaw(
		template.ArkTransaction(), payoutDesc,
	)
	require.NoError(t, err)
	settlement, err := template.Complete(
		terms, source, request, checkpointSig, payoutSig,
	)
	require.NoError(t, err)

	return arkchannel.Snapshot{
		Terms: terms, Source: &source, Backing: &backing,
		Phase:                   arkchannel.PhaseClosed,
		ClientFinalized:         true,
		HubFinalized:            true,
		OORFinalized:            true,
		ClientRecoveryReady:     true,
		HubRecoveryReady:        true,
		CooperativeCloseRequest: &request,
		CooperativeClose:        &settlement,
	}
}
