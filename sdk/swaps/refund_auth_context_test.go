package swaps

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// refundContextDaemon observes whether external authorization accidentally
// narrows or cancels the local preparation and detached-send lifetime.
type refundContextDaemon struct {
	*testDaemonConn
	prepareDeadline time.Time
	sendDeadline    time.Time
	sendErr         error
}

// PrepareOORWithCustomInputs captures the caller's preparation deadline.
func (d *refundContextDaemon) PrepareOORWithCustomInputs(ctx context.Context,
	key []byte, amount int64, inputs []CustomInput) (*PreparedOOR, error) {

	d.prepareDeadline, _ = ctx.Deadline()

	return d.testDaemonConn.PrepareOORWithCustomInputs(
		ctx, key, amount, inputs,
	)
}

// SendOORWithCustomInputs verifies cancellation of the authorization context
// does not cancel a successful send before its ID can be persisted.
func (d *refundContextDaemon) SendOORWithCustomInputs(ctx context.Context,
	key []byte, amount int64, inputs []CustomInput) (string, error) {

	d.sendDeadline, _ = ctx.Deadline()
	d.sendErr = ctx.Err()
	if d.sendErr != nil {
		return "", d.sendErr
	}

	return d.testDaemonConn.SendOORWithCustomInputs(
		ctx, key, amount, inputs,
	)
}

// TestCooperativeRefundBoundsOnlyAuthorization keeps the peer deadline narrow
// while preparation, accepted submission and durable ID persistence retain
// the original caller's lifetime.
func TestCooperativeRefundBoundsOnlyAuthorization(t *testing.T) {
	t.Parallel()
	client, daemon, server, row := newRefundRecoveryFixture(t)
	remote := &restoredRefundServer{
		testSwapServerConn: server,
		authorization: &InSwapRefundAuthorization{
			Signature: TaprootScriptSignature{
				PubKey:        row.GetReceiverPubkey(),
				WitnessScript: []byte("witness"),
				Signature:     []byte("authorized-refund"),
			},
		},
	}
	client.server = remote
	local := &refundContextDaemon{testDaemonConn: daemon}
	client.daemon = local
	daemon.sendSessionID = "accepted-cooperative-refund"
	daemon.blockHeight = 50
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := client.RecoverSwapserverVHTLCs(ctx)
	require.NoError(t, err)
	hash, err := lntypes.MakeHash(row.GetPaymentHash())
	require.NoError(t, err)
	session, err := client.ResumePayViaLightning(ctx, hash)
	require.NoError(t, err)
	refunded, err := session.tryCooperativeRefund(ctx)
	require.NoError(t, err)
	require.True(t, refunded)
	require.True(t, remote.bounded)
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, local.prepareDeadline)
	require.Equal(t, deadline, local.sendDeadline)
	require.NoError(t, local.sendErr)
	resumed, err := client.ResumePayViaLightning(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, daemon.sendSessionID, resumed.refundSessionID)
	require.Equal(t, 1, daemon.sendCustomCalls)
}
