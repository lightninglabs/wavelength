package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

func TestDriveEventCommandRoundTripRetryDueEvent(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{3, 3, 3})
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event:     &RetryDueEvent{},
	}

	cmd, err := durableCommandFromActorMsg(msg)
	require.NoError(t, err)
	require.Equal(t, oorCommandDriveEvent, cmd.Command)

	decoded, err := actorMsgFromDurableCommand(cmd)
	require.NoError(t, err)

	decodedReq, ok := decoded.(*DriveEventRequest)
	require.True(t, ok)
	require.Equal(t, sessionID, decodedReq.SessionID)
	require.IsType(t, &RetryDueEvent{}, decodedReq.Event)
}
