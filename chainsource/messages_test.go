package chainsource

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/stretchr/testify/require"
)

// TestMessageTypes tests that all message types implement the correct
// interfaces and return expected MessageType strings. This is mostly in place
// so code coverage doesn't report that we have zero coverage on these methods.
func TestMessageTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		msg               interface{}
		expectedType      string
		isChainSourceMsg  bool
		isChainSourceResp bool
		isConfMsg         bool
		isConfResp        bool
		isSpendMsg        bool
		isSpendResp       bool
		isEpochMsg        bool
		isEpochResp       bool
	}{
		{
			name: "FeeEstimateRequest",
			msg: &FeeEstimateRequest{
				TargetConf: 6,
			},
			expectedType:     "FeeEstimateRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "FeeEstimateResponse",
			msg:               &FeeEstimateResponse{},
			expectedType:      "FeeEstimateResponse",
			isChainSourceResp: true,
		},
		{
			name:             "BestHeightRequest",
			msg:              &BestHeightRequest{},
			expectedType:     "BestHeightRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "BestHeightResponse",
			msg:               &BestHeightResponse{},
			expectedType:      "BestHeightResponse",
			isChainSourceResp: true,
		},
		{
			name: "TestMempoolAcceptRequest",
			msg: &TestMempoolAcceptRequest{
				Txs: []*wire.MsgTx{
					wire.NewMsgTx(2),
				},
			},
			expectedType:     "TestMempoolAcceptRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "TestMempoolAcceptResponse",
			msg:               &TestMempoolAcceptResponse{},
			expectedType:      "TestMempoolAcceptResponse",
			isChainSourceResp: true,
		},
		{
			name: "BroadcastTxRequest",
			msg: &BroadcastTxRequest{
				Tx: wire.NewMsgTx(2),
			},
			expectedType:     "BroadcastTxRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "BroadcastTxResponse",
			msg:               &BroadcastTxResponse{},
			expectedType:      "BroadcastTxResponse",
			isChainSourceResp: true,
		},
		{
			name: "SubmitPackageRequest",
			msg: &SubmitPackageRequest{
				Parents: []*wire.MsgTx{
					wire.NewMsgTx(3),
				},
				Child: wire.NewMsgTx(3),
			},
			expectedType:     "SubmitPackageRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "SubmitPackageResponse",
			msg:               &SubmitPackageResponse{},
			expectedType:      "SubmitPackageResponse",
			isChainSourceResp: true,
		},
		{
			name: "RegisterConfRequest",
			msg: &RegisterConfRequest{
				CallerID: "test",
				Txid:     &chainhash.Hash{},
				PkScript: []byte{
					0x00,
					0x14,
				},
				TargetConfs: 1,
			},
			expectedType:     "RegisterConfRequest",
			isConfMsg:        true,
			isChainSourceMsg: true,
		},
		{
			name:              "RegisterConfResponse",
			msg:               &RegisterConfResponse{},
			expectedType:      "RegisterConfResponse",
			isConfResp:        true,
			isChainSourceResp: true,
		},
		{
			name: "ConfirmationEvent",
			msg: ConfirmationEvent{
				Txid:        chainhash.Hash{},
				BlockHeight: 100,
				BlockHash:   chainhash.Hash{},
				NumConfs:    6,
			},
			expectedType: "ConfirmationEvent",
		},
		{
			name: "RegisterSpendRequest",
			msg: &RegisterSpendRequest{
				Outpoint: &wire.OutPoint{},
				PkScript: []byte{
					0x00,
					0x14,
				},
			},
			expectedType:     "RegisterSpendRequest",
			isSpendMsg:       true,
			isChainSourceMsg: true,
		},
		{
			name:              "RegisterSpendResponse",
			msg:               &RegisterSpendResponse{},
			expectedType:      "RegisterSpendResponse",
			isSpendResp:       true,
			isChainSourceResp: true,
		},
		{
			name: "SpendEvent",
			msg: SpendEvent{
				Outpoint:          wire.OutPoint{},
				SpendingTxid:      chainhash.Hash{},
				SpendingTx:        wire.NewMsgTx(2),
				SpenderInputIndex: 0,
				SpendingHeight:    100,
			},
			expectedType: "SpendEvent",
		},
		{
			name:             "SubscribeBlocksRequest",
			msg:              &SubscribeBlocksRequest{},
			expectedType:     "SubscribeBlocksRequest",
			isEpochMsg:       true,
			isChainSourceMsg: true,
		},
		{
			name:              "SubscribeBlocksResponse",
			msg:               &SubscribeBlocksResponse{},
			expectedType:      "SubscribeBlocksResponse",
			isEpochResp:       true,
			isChainSourceResp: true,
		},
		{
			name: "BlockEpoch",
			msg: BlockEpoch{
				Height:    100,
				Hash:      chainhash.Hash{},
				Timestamp: 1234567890,
			},
			expectedType: "BlockEpoch",
		},
		{
			name: "UnregisterConfRequest",
			msg: &UnregisterConfRequest{
				CallerID: "test",
				Txid:     &chainhash.Hash{},
				PkScript: []byte{
					0x00,
					0x14,
				},
				TargetConfs: 1,
			},
			expectedType:     "UnregisterConfRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "UnregisterConfResponse",
			msg:               &UnregisterConfResponse{},
			expectedType:      "UnregisterConfResponse",
			isChainSourceResp: true,
		},
		{
			name: "UnregisterSpendRequest",
			msg: &UnregisterSpendRequest{
				CallerID: "test",
				Outpoint: &wire.OutPoint{},
				PkScript: []byte{
					0x00,
					0x14,
				},
			},
			expectedType:     "UnregisterSpendRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "UnregisterSpendResponse",
			msg:               &UnregisterSpendResponse{},
			expectedType:      "UnregisterSpendResponse",
			isChainSourceResp: true,
		},
		{
			name: "UnsubscribeBlocksRequest",
			msg: &UnsubscribeBlocksRequest{
				CallerID: "test",
			},
			expectedType:     "UnsubscribeBlocksRequest",
			isChainSourceMsg: true,
		},
		{
			name:              "UnsubscribeBlocksResponse",
			msg:               &UnsubscribeBlocksResponse{},
			expectedType:      "UnsubscribeBlocksResponse",
			isChainSourceResp: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			type msgTyperIface interface {
				MessageType() string
			}
			msgTyper, ok := tt.msg.(msgTyperIface)
			if ok {
				msgType := msgTyper.MessageType()
				require.Equal(
					t, tt.expectedType, msgType, "Messag"+
						"eType() should return %q",
					tt.expectedType,
				)
			} else {
				t.Errorf("message type %T does not implement "+
					"MessageType()", tt.msg)
			}

			_, ok = tt.msg.(actor.Message)
			require.True(
				t, ok, "%T should implement actor.Message",
				tt.msg,
			)

			if tt.isChainSourceMsg {
				msg, ok := tt.msg.(ChainSourceMsg)
				require.True(
					t, ok, "%T should implement "+
						"ChainSourceMsg", tt.msg,
				)
				msg.chainSourceMsgSealed()
			}

			if tt.isChainSourceResp {
				resp, ok := tt.msg.(ChainSourceResp)
				require.True(
					t, ok, "%T should implement "+
						"ChainSourceResp", tt.msg,
				)
				resp.chainSourceRespSealed()
			}

			if tt.isConfMsg {
				msg, ok := tt.msg.(ConfMsg)
				require.True(
					t, ok, "%T should implement ConfMsg",
					tt.msg,
				)
				msg.confMsgSealed()
			}

			if tt.isConfResp {
				resp, ok := tt.msg.(ConfResp)
				require.True(
					t, ok, "%T should implement ConfResp",
					tt.msg,
				)
				resp.confRespSealed()
			}

			if tt.isSpendMsg {
				msg, ok := tt.msg.(SpendMsg)
				require.True(
					t, ok, "%T should implement SpendMsg",
					tt.msg,
				)
				msg.spendMsgSealed()
			}

			if tt.isSpendResp {
				resp, ok := tt.msg.(SpendResp)
				require.True(
					t, ok, "%T should implement SpendResp",
					tt.msg,
				)
				resp.spendRespSealed()
			}

			if tt.isEpochMsg {
				msg, ok := tt.msg.(EpochMsg)
				require.True(
					t, ok, "%T should implement EpochMsg",
					tt.msg,
				)
				msg.epochMsgSealed()
			}

			if tt.isEpochResp {
				resp, ok := tt.msg.(EpochResp)
				require.True(
					t, ok, "%T should implement EpochResp",
					tt.msg,
				)
				resp.epochRespSealed()
			}
		})
	}
}
