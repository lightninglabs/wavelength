package swaps

import (
	"bytes"
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/swaprecovery"
	"github.com/lightninglabs/wavelength/swaprpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestOutSwapPreimageRecovery verifies reproducibility, metadata tampering,
// wrong-wallet rejection, and fresh hashes when index zero is reused.
func TestOutSwapPreimageRecovery(t *testing.T) {
	t.Parallel()

	daemonConn := &testDaemonConn{
		receiveAuthKey: bytes.Repeat([]byte{3}, 32),
	}
	preimage, params, err := newOutSwapPreimage(t.Context(), daemonConn)
	require.NoError(t, err)
	require.Len(t, params, swaprecovery.ParamsSize)
	require.False(t, bytes.Contains(params, preimage[:]))

	recovered, err := recoverOutSwapPreimage(
		t.Context(), daemonConn, preimage.Hash(), params,
	)
	require.NoError(t, err)
	require.Equal(t, preimage, *recovered)

	corrupt := append([]byte(nil), params...)
	corrupt[len(corrupt)-1] ^= 1
	_, err = recoverOutSwapPreimage(
		t.Context(), daemonConn, preimage.Hash(), corrupt,
	)
	require.ErrorContains(t, err, "preimage hash mismatch")

	otherDaemon := &testDaemonConn{
		receiveAuthKey: bytes.Repeat([]byte{4}, 32),
	}
	_, err = recoverOutSwapPreimage(
		t.Context(), otherDaemon, preimage.Hash(), params,
	)
	require.ErrorContains(t, err, "preimage hash mismatch")

	other, _, err := newOutSwapPreimage(t.Context(), daemonConn)
	require.NoError(t, err)
	require.NotEqual(t, preimage.Hash(), other.Hash())

	_, err = recoverOutSwapPreimage(
		t.Context(), daemonConn, preimage.Hash(), params[:1],
	)
	require.ErrorContains(t, err, "must be 37 bytes")
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
			PubKeyXOnly: schnorr.SerializePubKey(
				clientPriv.PubKey(),
			),
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

	outPreimage, outBlob, err := newOutSwapPreimage(ctx, daemonConn)
	require.NoError(t, err)
	outHash := outPreimage.Hash()
	outRow := recoverableSwapForTest(
		t, swaprpc.
			RecoverableSwapDirection_RECOVERABLE_SWAP_DIRECTION_OUT,
		outHash,
		serverPriv.PubKey(),
		clientPriv.PubKey(),
		operatorPriv.PubKey(),
		outBlob,
	)
	outRow.AmountSat = 43_000
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
	client := newRecoveryClient(t, serverConn, daemonConn)
	client.SetRecoveryPolicy(RecoveryPolicy{AutoEscalate: true})

	result, err := client.RecoverSwapserverVHTLCs(ctx)
	require.NoError(t, err)
	require.NotNil(t, serverConn.lastRecoverableProof)
	require.EqualValues(t, 2, result.RecoveredVHTLCs)
	require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
	require.EqualValues(t, 1, result.RecoveredVHTLCClaims)
	require.Equal(t, 2, daemonConn.armRecoveryCalls)
	require.Equal(t, 2, daemonConn.statusCalls)
	require.Zero(t, daemonConn.listRecoveriesCalls)
	require.Zero(t, daemonConn.escalateCalls)
	pay, err := client.ResumePayViaLightning(ctx, inHash)
	require.NoError(t, err)
	require.Equal(t, PayStateRefundInitiated, pay.State())
	receive, err := client.ResumeReceiveViaLightning(ctx, outHash)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateVHTLCFunded, receive.State())
	require.Equal(t, outPreimage, receive.Preimage)

	seenRefund := false
	seenClaim := false
	for _, req := range daemonConn.armRecoveries {
		switch req.GetAction() {
		case waverpc.
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

	outPreimage, outBlob, err := newOutSwapPreimage(ctx, daemonConn)
	require.NoError(t, err)
	outHash := outPreimage.Hash()
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
	daemonConn.listRecoveriesResp = &waverpc.
		ListVHTLCRecoveriesResponse{
		Statuses: []*waverpc.VHTLCRecoveryStatus{
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
	daemonConn.receiveInfo = &ReceiveInfo{
		PkScript: []byte{
			0x51,
		},
		PubKeyXOnly: schnorr.SerializePubKey(clientPriv.PubKey()),
	}
	client := newRecoveryClient(t, serverConn, daemonConn)
	client.SetRecoveryPolicy(RecoveryPolicy{AutoEscalate: true})

	result, err := client.RecoverSwapserverVHTLCs(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.RecoveredVHTLCs)
	require.EqualValues(t, 1, result.RecoveredVHTLCRefunds)
	require.EqualValues(t, 1, result.RecoveredVHTLCClaims)
	require.Zero(t, daemonConn.armRecoveryCalls)
	require.Equal(t, 2, daemonConn.receiveAllocCalls)
	require.Zero(t, daemonConn.escalateCalls)
	receive, err := client.ResumeReceiveViaLightning(ctx, outHash)
	require.NoError(t, err)
	require.Equal(t, "existing-out-recovery", receive.claimRecoveryID)

	// Once the cooperative destinations are durable, another scan neither
	// allocates again nor replaces sessions already owned by the runtime.
	result, err = client.RecoverSwapserverVHTLCs(ctx)
	require.NoError(t, err)
	require.Zero(t, result.RecoveredVHTLCs)
	require.Equal(t, 2, daemonConn.receiveAllocCalls)
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
			PubKeyXOnly: schnorr.SerializePubKey(
				clientPriv.PubKey(),
			),
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
	client := newRecoveryClient(t, serverConn, daemonConn)

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
	client := newRecoveryClient(t, serverConn, daemonConn)

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
	client := newRecoveryClient(t, serverConn, daemonConn)

	result, err := client.RecoverSwapserverVHTLCs(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.RecoveredVHTLCs)
	require.Zero(t, daemonConn.armRecoveryCalls)
	require.Equal(t, 1, daemonConn.liveLookupCalls)
}

func recoverableStatusForTest(row *swaprpc.RecoverableSwap,
	paymentHash lntypes.Hash, live *VTXOInfo, recoveryID string,
	direction waverpc.VHTLCRecoveryDirection,
	action waverpc.VHTLCRecoveryAction) *waverpc.VHTLCRecoveryStatus {

	return &waverpc.VHTLCRecoveryStatus{
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
	recoveryParams []byte) *swaprpc.RecoverableSwap {

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
		PreimageRecoveryParams: append([]byte(nil), recoveryParams...),
	}
}
