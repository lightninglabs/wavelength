package oor

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

func buildIncomingResolveResponse(t *testing.T) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, SessionID, []byte,
	uint64) {

	t.Helper()

	arkPSBT, finalCheckpoints, recipients, _, _, _ :=
		buildTestIncomingMaterialization(t)

	arkRaw, err := psbtutil.Serialize(arkPSBT)
	require.NoError(t, err)

	checkpointRaws := make([][]byte, 0, len(finalCheckpoints))
	for _, checkpoint := range finalCheckpoints {
		checkpointRaw, checkpointErr := psbtutil.Serialize(checkpoint)
		require.NoError(t, checkpointErr)

		checkpointRaws = append(checkpointRaws, checkpointRaw)
	}

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	recipient := recipients[0]

	return &arkrpc.ListOORRecipientEventsByScriptResponse{
		Events: []*arkrpc.OORRecipientEvent{
			{
				RecipientPkScript: recipient.PkScript,
				EventId:           7,
				SessionId:         sessionID[:],
				OutputIndex:       recipient.OutputIndex,
				Value:             uint64(recipient.Value),
				ArkPsbt:           arkRaw,
				CheckpointPsbts:   checkpointRaws,
			},
		},
		NextCursor: 8,
	}, sessionID, recipient.PkScript, 7
}
