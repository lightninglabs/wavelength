package swaps

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestSealOutSwapRecoveryBlobRoundTrip verifies that the out-swap preimage is
// recoverable with seed-restorable daemon key material but not with corrupted
// ciphertext or a mismatched payment hash.
func TestSealOutSwapRecoveryBlobRoundTrip(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimage := lntypes.Preimage(testHash(91))
	paymentHash := preimage.Hash()
	daemonConn := &testDaemonConn{
		receiveAuthKey: bytes.Repeat([]byte{3}, 32),
	}

	blob, err := sealOutSwapRecoveryBlob(
		t.Context(), daemonConn, clientPriv.PubKey(), paymentHash,
		preimage,
	)
	require.NoError(t, err)
	require.False(t, bytes.Contains(blob, preimage[:]))

	recovered, err := openOutSwapRecoveryBlob(
		t.Context(), daemonConn, clientPriv.PubKey(), paymentHash, blob,
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *recovered)

	corrupt := append([]byte(nil), blob...)
	corrupt[len(corrupt)-1] ^= 0x01
	_, err = openOutSwapRecoveryBlob(
		t.Context(), daemonConn, clientPriv.PubKey(), paymentHash,
		corrupt,
	)
	require.ErrorContains(t, err, "open recovery blob")

	wrongPreimage := lntypes.Preimage(testHash(92))
	wrongHash := wrongPreimage.Hash()
	_, err = openOutSwapRecoveryBlob(
		t.Context(), daemonConn, clientPriv.PubKey(), wrongHash, blob,
	)
	require.ErrorContains(t, err, "open recovery blob")
}

// TestNewSwapOwnerProofValidation verifies owner proofs reject malformed local
// inputs before sending recoverability data to the server.
func TestNewSwapOwnerProofValidation(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		identitySignature: bytes.Repeat([]byte{1}, 63),
	}
	_, err = newSwapOwnerProof(
		t.Context(), daemonConn, clientPriv.PubKey(),
		swapRecoveryAuthList, 1,
	)
	require.ErrorContains(t, err, "signature must be 64 bytes")

	daemonConn = &testDaemonConn{}
	_, err = newSwapOwnerProof(
		t.Context(), daemonConn, clientPriv.PubKey(),
		swapRecoveryAuthList, -1,
	)
	require.ErrorContains(t, err, "non-negative")
}

// TestRecoverSwapserverVHTLCsArmsRefundAndClaim verifies restore discovery
// turns live in-swap and out-swap rows into daemon-owned recovery rows.
func TestRecoverSwapserverVHTLCsArmsRefundAndClaim(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		identityPriv:   clientPriv,
		identityKey:    clientPriv.PubKey(),
		receiveAuthKey: bytes.Repeat([]byte{4}, 32),
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
			PubKeyXOnly: clientPriv.PubKey().X().Bytes(),
		},
		liveByPkScript: make(map[string]*VTXOInfo),
	}

	inHash := lntypes.Hash(testHash(101))
	inRow := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		inHash,
		clientPriv.PubKey(),
		serverPriv.PubKey(),
		operatorPriv.PubKey(),
		nil,
	)
	daemonConn.liveByPkScript[hex.EncodeToString(
		inRow.GetVhtlcPkScript(),
	)] = &VTXOInfo{
		Outpoint:  "in-funding:0",
		AmountSat: 42_000,
	}

	outPreimage := lntypes.Preimage(testHash(102))
	outHash := outPreimage.Hash()
	outBlob, err := sealOutSwapRecoveryBlob(
		ctx, daemonConn, clientPriv.PubKey(), outHash, outPreimage,
	)
	require.NoError(t, err)
	outRow := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT,
		outHash,
		serverPriv.PubKey(),
		clientPriv.PubKey(),
		operatorPriv.PubKey(),
		outBlob,
	)
	daemonConn.liveByPkScript[hex.EncodeToString(
		outRow.GetVhtlcPkScript(),
	)] = &VTXOInfo{
		Outpoint:  "out-funding:0",
		AmountSat: 43_000,
	}

	outDirection := swaprpc.
		RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT
	serverConn := &testSwapServerConn{
		recoverableRows: []*swaprpc.RecoverableSwap{
			inRow,
			outRow,
			{
				Direction: outDirection,
			},
		},
	}
	client := NewSwapClient(serverConn, daemonConn, nil, nil)

	result, err := client.RecoverSwapserverVHTLCs(ctx)
	require.NoError(t, err)
	require.NotNil(t, serverConn.lastRecoverableProof)
	require.EqualValues(t, 2, result.RecoveredVHTLCs)
	require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
	require.EqualValues(t, 1, result.RecoveredVHTLCClaims)
	require.Equal(t, 2, daemonConn.armRecoveryCalls)
	require.Equal(t, 1, daemonConn.escalateCalls)
	require.Equal(
		t, outPreimage[:], daemonConn.lastEscalate.GetClaimPreimage(),
	)

	seenRefund := false
	seenClaim := false
	for _, req := range daemonConn.armRecoveries {
		switch req.GetAction() {
		case daemonrpc.
			VHTLCRecoveryAction_VHTLC_RECOVERY_ACTION_UNSPECIFIED:

			require.Fail(t, "unexpected unspecified action")

		case recoveryActionRefundWithoutReceiver:
			seenRefund = true
			require.Equal(
				t, recoveryDirectionPay, req.GetDirection(),
			)
			require.Equal(t, "in-funding:0", req.GetVtxoOutpoint())

		case recoveryActionClaim:
			seenClaim = true
			require.Equal(
				t, recoveryDirectionReceive, req.GetDirection(),
			)
			require.Equal(t, "out-funding:0", req.GetVtxoOutpoint())
		}
	}
	require.True(t, seenRefund)
	require.True(t, seenClaim)
}

// TestRecoverSwapserverVHTLCsReusesExistingRecovery verifies restore retries
// reuse matching daemon rows instead of allocating new destinations.
func TestRecoverSwapserverVHTLCsReusesExistingRecovery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	daemonConn := &testDaemonConn{
		identityPriv:   clientPriv,
		identityKey:    clientPriv.PubKey(),
		receiveAuthKey: bytes.Repeat([]byte{5}, 32),
		liveByPkScript: make(map[string]*VTXOInfo),
	}

	inHash := lntypes.Hash(testHash(121))
	inRow := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		inHash,
		clientPriv.PubKey(),
		serverPriv.PubKey(),
		operatorPriv.PubKey(),
		nil,
	)
	inLive := &VTXOInfo{
		Outpoint:  "existing-in:0",
		AmountSat: 42_000,
	}
	daemonConn.liveByPkScript[hex.EncodeToString(
		inRow.GetVhtlcPkScript(),
	)] = inLive

	outPreimage := lntypes.Preimage(testHash(122))
	outHash := outPreimage.Hash()
	outBlob, err := sealOutSwapRecoveryBlob(
		ctx, daemonConn, clientPriv.PubKey(), outHash, outPreimage,
	)
	require.NoError(t, err)
	outRow := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT,
		outHash,
		serverPriv.PubKey(),
		clientPriv.PubKey(),
		operatorPriv.PubKey(),
		outBlob,
	)
	outLive := &VTXOInfo{
		Outpoint:  "existing-out:0",
		AmountSat: 42_000,
	}
	daemonConn.liveByPkScript[hex.EncodeToString(
		outRow.GetVhtlcPkScript(),
	)] = outLive
	daemonConn.listRecoveriesResp = &daemonrpc.
		ListVHTLCRecoveriesResponse{
		Statuses: []*daemonrpc.VHTLCRecoveryStatus{
			recoverableStatusForTest(
				inRow, inHash, inLive, "existing-in-recovery",
				recoveryDirectionPay,
				recoveryActionRefundWithoutReceiver,
			),
			recoverableStatusForTest(
				outRow, outHash, outLive,
				"existing-out-recovery",
				recoveryDirectionReceive, recoveryActionClaim,
			),
		},
	}

	serverConn := &testSwapServerConn{
		recoverableRows: []*swaprpc.RecoverableSwap{
			inRow, outRow,
		},
	}
	client := NewSwapClient(serverConn, daemonConn, nil, nil)

	result, err := client.RecoverSwapserverVHTLCs(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.RecoveredVHTLCs)
	require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
	require.EqualValues(t, 1, result.RecoveredVHTLCClaims)
	require.Zero(t, daemonConn.armRecoveryCalls)
	require.Zero(t, daemonConn.receiveAllocCalls)
	require.Equal(t, 1, daemonConn.escalateCalls)
	require.Equal(
		t, "existing-out-recovery",
		daemonConn.lastEscalate.GetRecoveryId(),
	)
	require.Equal(
		t, outPreimage[:], daemonConn.lastEscalate.GetClaimPreimage(),
	)
}

// TestRecoverSwapserverVHTLCsSkipsMalformedRows verifies one bad server row
// does not abort recovery for the remaining rows.
func TestRecoverSwapserverVHTLCsSkipsMalformedRows(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	validHash := lntypes.Hash(testHash(131))
	validRow := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		validHash,
		clientPriv.PubKey(),
		serverPriv.PubKey(),
		operatorPriv.PubKey(),
		nil,
	)
	malformedRow, ok := proto.Clone(validRow).(*swaprpc.RecoverableSwap)
	require.True(t, ok)
	malformedRow.PaymentHash = []byte{1}
	validPkScript := hex.EncodeToString(validRow.GetVhtlcPkScript())

	daemonConn := &testDaemonConn{
		identityPriv: clientPriv,
		identityKey:  clientPriv.PubKey(),
		receiveInfo: &ReceiveInfo{
			PkScript: []byte{
				0x51,
			},
		},
		liveByPkScript: map[string]*VTXOInfo{
			validPkScript: {
				Outpoint:  "valid:0",
				AmountSat: 42_000,
			},
		},
	}
	serverConn := &testSwapServerConn{
		recoverableRows: []*swaprpc.RecoverableSwap{
			malformedRow, validRow,
		},
	}
	client := NewSwapClient(serverConn, daemonConn, nil, nil)

	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, result.RecoveredVHTLCs)
	require.Equal(t, 1, daemonConn.armRecoveryCalls)
}

// TestRecoverSwapserverVHTLCsSkipsOverflowConfig verifies server uint32 script
// parameters are bounds-checked before daemon int32 request construction.
func TestRecoverSwapserverVHTLCsSkipsOverflowConfig(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	row := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		lntypes.Hash(
			testHash(141),
		),
		clientPriv.PubKey(),
		serverPriv.PubKey(),
		operatorPriv.PubKey(),
		nil,
	)
	row.RefundLocktime = 1 << 31

	daemonConn := &testDaemonConn{
		identityPriv: clientPriv,
		identityKey:  clientPriv.PubKey(),
		liveByPkScript: map[string]*VTXOInfo{
			hex.EncodeToString(row.GetVhtlcPkScript()): &VTXOInfo{
				Outpoint:  "overflow:0",
				AmountSat: 42_000,
			},
		},
	}
	serverConn := &testSwapServerConn{
		recoverableRows: []*swaprpc.RecoverableSwap{
			row,
		},
	}
	client := NewSwapClient(serverConn, daemonConn, nil, nil)

	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.RecoveredVHTLCs)
	require.Zero(t, daemonConn.armRecoveryCalls)
}

// TestRecoverSwapserverVHTLCsSkipsUnfundedRows verifies server-discovered rows
// do not arm recovery until the indexer reports the vHTLC as live.
func TestRecoverSwapserverVHTLCsSkipsUnfundedRows(t *testing.T) {
	t.Parallel()

	clientPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	row := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		lntypes.Hash(
			testHash(111),
		),
		clientPriv.PubKey(),
		serverPriv.PubKey(),
		operatorPriv.PubKey(),
		nil,
	)
	serverConn := &testSwapServerConn{
		recoverableRows: []*swaprpc.RecoverableSwap{
			row,
		},
	}
	daemonConn := &testDaemonConn{
		identityPriv: clientPriv,
		identityKey:  clientPriv.PubKey(),
	}
	client := NewSwapClient(serverConn, daemonConn, nil, nil)

	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.RecoveredVHTLCs)
	require.Zero(t, daemonConn.armRecoveryCalls)
	require.Equal(t, 1, daemonConn.liveLookupCalls)
}

func recoverableStatusForTest(row *swaprpc.RecoverableSwap,
	paymentHash lntypes.Hash, live *VTXOInfo, recoveryID string,
	direction daemonrpc.VHTLCRecoveryDirection,
	action daemonrpc.VHTLCRecoveryAction) *daemonrpc.VHTLCRecoveryStatus {

	return &daemonrpc.VHTLCRecoveryStatus{
		RecoveryId: recoveryID,
		RequestId: recoveryRequestID(
			recoveryDirectionString(direction), paymentHash, action,
		),
		SwapId:        append([]byte(nil), paymentHash[:]...),
		Direction:     direction,
		Action:        action,
		State:         recoveryStateArmed,
		VtxoOutpoint:  live.Outpoint,
		VtxoAmountSat: live.AmountSat,
		RefundLocktime: int32(
			row.GetRefundLocktime(),
		),
		UnilateralClaimDelay: int32(
			row.GetUnilateralClaimDelay(),
		),
		UnilateralRefundDelay: int32(
			row.GetUnilateralRefundDelay(),
		),
		UnilateralRefundWithoutReceiverDelay: int32(
			row.GetUnilateralRefundWithoutReceiverDelay(),
		),
	}
}

func recoverableSwapForTest(t *testing.T,
	direction swaprpc.RecoverableSwapDirection, paymentHash lntypes.Hash,
	sender, receiver, operator *btcec.PublicKey,
	encryptedBlob []byte) *swaprpc.RecoverableSwap {

	t.Helper()

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               operator,
		PreimageHash:                         paymentHash,
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 10,
		UnilateralRefundDelay:                20,
		UnilateralRefundWithoutReceiverDelay: 30,
	})
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	return &swaprpc.RecoverableSwap{
		Direction: direction,
		PaymentHash: append(
			[]byte(nil), paymentHash[:]...,
		),
		AmountSat:      42_000,
		StateName:      "test",
		SenderPubkey:   sender.SerializeCompressed(),
		ReceiverPubkey: receiver.SerializeCompressed(),
		OperatorPubkey: operator.SerializeCompressed(),
		PreimageHash: append(
			[]byte(nil), paymentHash[:]...,
		),
		RefundLocktime:                       144,
		UnilateralClaimDelay:                 10,
		UnilateralRefundDelay:                20,
		UnilateralRefundWithoutReceiverDelay: 30,
		VhtlcPkScript: append(
			[]byte(nil), pkScript...,
		),
		RefundAuthorizationAvailable: direction == swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_IN,
		EncryptedRecoveryBlob: append([]byte(nil), encryptedBlob...),
	}
}
