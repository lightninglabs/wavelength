package indexer

import (
	"testing"

	"github.com/lightninglabs/darepo-client/arkrpc"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestIndexEventMessageServiceMethod verifies that pushed indexer
// events route through the ArkService ingress keys expected by the
// client event router.
func TestIndexEventMessageServiceMethod(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name     string
		event    proto.Message
		expected mailboxrpc.ServiceMethod
	}{
		{
			name:  "incoming oor",
			event: &arkrpc.IncomingOOREvent{},
			expected: mailboxrpc.ServiceMethod{
				Service: arkServiceName,
				Method:  MethodIncomingOOR,
			},
		},
		{
			name:  "incoming vtxo",
			event: &arkrpc.IncomingVTXOEvent{},
			expected: mailboxrpc.ServiceMethod{
				Service: arkServiceName,
				Method:  MethodIncomingVTXO,
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			msg := &indexerEventMessage{
				clientID: clientconn.ClientID("client"),
				event:    testCase.event,
			}

			require.Equal(t, testCase.expected, msg.ServiceMethod())
		})
	}
}
