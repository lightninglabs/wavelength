//go:build systest

package systest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// injectedPostAdmissionRoundID is the fixed 16-byte (UUID-shaped) round id the
// fake operator assigns when it admits then fails a round. The same id is
// echoed on both the RoundJoined admission and the ClientRoundFailedResp
// failure so the client re-keys its FSM to this id and then routes the failure
// back to that same re-keyed round (the routing fixed in darepo-client#761).
var injectedPostAdmissionRoundID = [16]byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
}

// postAdmissionRecoveryWindow bounds how long we wait for the server-pushed
// round failure to release the forfeit-reserved VTXO back to LIVE. It is
// generous to tolerate the several async actor hops (ingress → round actor →
// VTXO manager → VTXO actor) under parallel Dockerized systest load.
const postAdmissionRecoveryWindow = 60 * time.Second

// failRoundsAfterAdmission arms the fake operator so that every JoinRound it
// receives is first admitted (RoundJoined) and then failed
// (ClientRoundFailedResp), reproducing a server round-build failure that lands
// after the client has already been admitted.
func (s *fakeMailboxServer) failRoundsAfterAdmission(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failRoundAfterAdmission = true
	s.failRoundReason = reason
}

// maybeFailRoundAfterAdmission, when arming is enabled, pushes a RoundJoined
// admission watermark followed by a round-failure to the client that sent the
// given JoinRound envelope. The admission is sent first so the client FSM
// re-keys to the server-assigned round id and cancels its registration timeout;
// the failure then carries that same id so it routes back to the re-keyed round
// (darepo-client#761) and drives it into ClientFailedState, where the
// pre-signing forfeit release runs.
func (s *fakeMailboxServer) maybeFailRoundAfterAdmission(
	joinEnv *mailboxpb.Envelope) error {

	s.mu.Lock()
	enabled := s.failRoundAfterAdmission
	reason := s.failRoundReason
	s.mu.Unlock()

	if !enabled {
		return nil
	}

	// The client re-keys a pending round to the server id only when the
	// RoundJoined echoes the round's accepted inputs (handleRoundJoined
	// matches by outpoint). Echo both the boarding and forfeited VTXO
	// outpoints from the JoinRound so the admission matches the pending
	// round whether it is a boarding or a leave/refresh.
	var req roundpb.JoinRoundRequest
	if err := joinEnv.Body.UnmarshalTo(&req); err != nil {
		return fmt.Errorf("decode join round for admission: %w", err)
	}

	acceptedBoarding := make(
		[]*roundpb.Outpoint, 0, len(req.BoardingRequests),
	)
	for _, b := range req.BoardingRequests {
		if b.Outpoint != nil {
			acceptedBoarding = append(acceptedBoarding, b.Outpoint)
		}
	}

	acceptedVTXOs := make([]*roundpb.Outpoint, 0, len(req.ForfeitRequests))
	for _, ff := range req.ForfeitRequests {
		if ff.VtxoOutpoint != nil {
			acceptedVTXOs = append(acceptedVTXOs, ff.VtxoOutpoint)
		}
	}

	rid := injectedPostAdmissionRoundID

	// Admit first: the client matches the echoed outpoints to its pending
	// round, re-keys it to this id, and parks in IntentSentState.
	admit := &roundpb.ClientSuccessResp{
		RoundId:                   rid[:],
		AcceptedBoardingOutpoints: acceptedBoarding,
		AcceptedVtxoOutpoints:     acceptedVTXOs,
	}
	if err := s.pushRoundEvent(
		joinEnv, roundpb.MethodJoinAck, admit,
	); err != nil {
		return err
	}

	// Then fail: the same round id routes this back to the re-keyed round,
	// which fails recoverably and (with the fix) releases its
	// forfeit-reserved inputs.
	failed := &roundpb.ClientRoundFailedResp{
		RoundId: rid[:],
		Reason:  reason,
	}

	return s.pushRoundEvent(joinEnv, roundpb.MethodRoundFailed, failed)
}

// pushRoundEvent enqueues a server→client round-protocol push (a KIND_EVENT
// envelope) back to the mailbox that sent reqEnv, mirroring how the real
// operator delivers round lifecycle notifications.
func (s *fakeMailboxServer) pushRoundEvent(reqEnv *mailboxpb.Envelope,
	method string, msg proto.Message) error {

	body, err := anypb.New(msg)
	if err != nil {
		return err
	}

	s.enqueueEnvelope(&mailboxpb.Envelope{
		ProtocolVersion: reqEnv.ProtocolVersion,
		Sender:          s.operatorMailbox,
		Recipient:       reqEnv.Rpc.ReplyTo,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: roundpb.ServiceName,
			Method:  method,
			ReplyTo: s.operatorMailbox,
		},
	})

	return nil
}

// TestForfeitReleasedOnPostAdmissionRoundFailure proves the post-admission
// forfeit-release fix end to end. A cooperative leave reserves the VTXO into
// pending-forfeit and registers the round; the fake operator admits the client
// (so the registration timeout is cancelled and the FSM is re-keyed) and then
// fails the round. The registration timeout is disabled, so the ONLY path back
// to LIVE is the release that runs when the post-admission failure drives the
// round into ClientFailedState. On an unfixed daemon the VTXO would stay
// stranded in pending-forfeit forever and the test would time out.
//
// This also exercises the darepo-client#761 routing fix: the failure carries
// the server-assigned round id and must reach the re-keyed round, not be
// dropped.
func TestForfeitReleasedOnPostAdmissionRoundFailure(t *testing.T) {
	ParallelN(t)

	// Disable the admission timeout so a return to LIVE can only come from
	// the post-admission release, never from the registration-timeout
	// safety net exercised by
	// TestLeaveStrandedVTXORecoversOnAdmissionTimeout.
	fixture := newDirectedSendFixture(t, func(c *darepod.Config) {
		c.EagerRoundJoin = true
		c.RegistrationTimeout = -1
	})

	// Arm the operator to admit-then-fail any round the client joins.
	fixture.mailboxServer.failRoundsAfterAdmission(
		"simulated post-admission round build failure",
	)

	// The seeded VTXO starts LIVE and is the entire spendable balance.
	startVTXOs := listAllVTXOs(t, fixture.client)
	require.Len(t, startVTXOs, 1)
	require.Equal(
		t, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE, startVTXOs[0].Status,
	)
	require.Equal(
		t, testSeededAmountSat, vtxoBalanceSat(t, fixture.client),
	)

	destAddr := newRegtestTaprootAddr(t)

	// Issue the cooperative leave. The reservation into pending-forfeit is
	// synchronous, so the VTXO is already reserved when this returns and
	// the JoinRound that triggers the admit-then-fail is in flight.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	leaveResp, err := fixture.client.LeaveVTXOs(
		ctx, &daemonrpc.LeaveVTXOsRequest{
			Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{
						outpointString(
							fixture.seededOutpoint,
						),
					},
				},
			},
			DefaultDestination: &daemonrpc.LeaveDestination{
				Target: &daemonrpc.LeaveDestination_Address{
					Address: destAddr,
				},
			},
		},
	)
	require.NoError(t, err, "LeaveVTXOs RPC failed")
	require.Equal(t, "queued", leaveResp.Status)

	// The fix: the server-pushed round failure reaches the re-keyed round,
	// fails it recoverably, and releases the VTXO back to LIVE. With the
	// admission timeout disabled, reaching LIVE proves the release came
	// from the post-admission failure path. On the unfixed daemon this
	// never happens and the assertion times out.
	requireVTXOStatusEventually(
		t, fixture.client, fixture.seededOutpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		postAdmissionRecoveryWindow,
	)

	// The spendable balance is restored to its starting value.
	require.Eventually(
		t,
		func() bool {
			return vtxoBalanceSat(t, fixture.client) ==
				testSeededAmountSat
		},
		20*time.Second, 100*time.Millisecond,
		"vtxo balance not restored after post-admission release",
	)
}
