package round

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// testRoundIDForMsg creates a deterministic RoundID from a string seed.
func testRoundIDForMsg(seed string) RoundID {
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return RoundID(id)
}

// TestSubmitVTXOForfeitSigsToProtoCarriesParticipantSigs verifies that a
// custom-policy forfeit can carry additional non-operator participant
// signatures for the same forfeited VTXO. Standard VTXOs still use the legacy
// ClientVtxoSig field, while vHTLC refresh/refund-style leaves need this
// repeated set so the operator can assemble an N-of-N tapscript witness.
func TestSubmitVTXOForfeitSigsToProtoCarriesParticipantSigs(t *testing.T) {
	t.Parallel()

	localPriv, localPub := btcec.PrivKeyFromBytes([]byte{1})
	otherPriv, otherPub := btcec.PrivKeyFromBytes([]byte{2})
	localSig, err := schnorr.Sign(localPriv, make([]byte, 32))
	require.NoError(t, err)
	otherSig, err := schnorr.Sign(otherPriv, make([]byte, 32))
	require.NoError(t, err)

	outpoint := wire.OutPoint{Index: 7}
	participantSigs := []*types.ForfeitParticipantSig{{
		PubKey:    localPub,
		Signature: localSig,
	}, {
		PubKey:    otherPub,
		Signature: otherSig,
	}}

	msg := &SubmitVTXOForfeitSigsToServer{
		RoundID: testRoundIDForMsg("multi-sig-forfeit"),
		ForfeitTxs: map[wire.OutPoint]*types.ForfeitTxSig{
			outpoint: {
				UnsignedTx:          wire.NewMsgTx(2),
				ClientVTXOSig:       localSig,
				ParticipantVTXOSigs: participantSigs,
				SpendPath: &arkscript.SpendPath{
					SpendInfo: &arkscript.SpendInfo{
						WitnessScript: []byte{
							0x51,
						},
						ControlBlock: []byte{
							0xc0,
						},
					},
				},
			},
		},
	}

	protoMsg := msg.ToProto().UnwrapOrFail(t)
	req, ok := protoMsg.(*roundpb.SubmitVTXOForfeitSigsRequest)
	require.True(t, ok)
	require.Len(t, req.GetForfeitTxs(), 1)

	forfeit := req.GetForfeitTxs()[0]
	require.NotEmpty(t, forfeit.GetClientVtxoSig())
	require.Len(t, forfeit.GetParticipantSigs(), 2)
	require.Equal(
		t, localPub.SerializeCompressed(),
		forfeit.GetParticipantSigs()[0].GetPubkey(),
	)
	require.Equal(
		t, otherPub.SerializeCompressed(),
		forfeit.GetParticipantSigs()[1].GetPubkey(),
	)
	require.Equal(
		t, localSig.Serialize(),
		forfeit.GetParticipantSigs()[0].GetSignature(),
	)
	require.Equal(
		t, otherSig.Serialize(),
		forfeit.GetParticipantSigs()[1].GetSignature(),
	)
}

// TestSubmitVTXOForfeitSigsToProtoAcceptsParticipantOnly verifies that a
// custom-policy forfeit is not forced through the legacy single-signature
// field. vHTLC refresh leaves identify each required signer by key, so the
// participant signature list is the authoritative signature carrier.
func TestSubmitVTXOForfeitSigsToProtoAcceptsParticipantOnly(t *testing.T) {
	t.Parallel()

	localPriv, localPub := btcec.PrivKeyFromBytes([]byte{3})
	otherPriv, otherPub := btcec.PrivKeyFromBytes([]byte{4})
	localSig, err := schnorr.Sign(localPriv, make([]byte, 32))
	require.NoError(t, err)
	otherSig, err := schnorr.Sign(otherPriv, make([]byte, 32))
	require.NoError(t, err)

	outpoint := wire.OutPoint{Index: 11}
	participantSigs := []*types.ForfeitParticipantSig{{
		PubKey:    localPub,
		Signature: localSig,
	}, {
		PubKey:    otherPub,
		Signature: otherSig,
	}}

	msg := &SubmitVTXOForfeitSigsToServer{
		RoundID: testRoundIDForMsg("participant-only-forfeit"),
		ForfeitTxs: map[wire.OutPoint]*types.ForfeitTxSig{
			outpoint: {
				UnsignedTx:          wire.NewMsgTx(2),
				ParticipantVTXOSigs: participantSigs,
				SpendPath: &arkscript.SpendPath{
					SpendInfo: &arkscript.SpendInfo{
						WitnessScript: []byte{
							0x51,
						},
						ControlBlock: []byte{
							0xc0,
						},
					},
				},
			},
		},
	}

	protoMsg := msg.ToProto().UnwrapOrFail(t)
	req, ok := protoMsg.(*roundpb.SubmitVTXOForfeitSigsRequest)
	require.True(t, ok)
	require.Len(t, req.GetForfeitTxs(), 1)

	forfeit := req.GetForfeitTxs()[0]
	require.Empty(t, forfeit.GetClientVtxoSig())
	require.Len(t, forfeit.GetParticipantSigs(), 2)
	require.Equal(
		t, localPub.SerializeCompressed(),
		forfeit.GetParticipantSigs()[0].GetPubkey(),
	)
	require.Equal(
		t, otherPub.SerializeCompressed(),
		forfeit.GetParticipantSigs()[1].GetPubkey(),
	)
}

// TestOutboxMessagesToProto ensures that ToProto() methods compile and return
// non-nil proto messages for all client outbox request types.
func TestOutboxMessagesToProto(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey := privKey.PubKey()

	t.Run("JoinRoundRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		msg := &JoinRoundRequest{
			BoardingRequests: []types.BoardingRequest{
				{
					PolicyTemplate: func() []byte {
						policy := stdTpl(
							t, pubKey, pubKey, 144,
						)

						return policy
					}(),
				},
			},
			VTXORequests: []types.VTXORequest{},
		}

		result := msg.ToProto().UnwrapOrFail(t)
		require.NotNil(t, result)
	})

	t.Run("JoinRoundRequest_ToProto custom paths", func(t *testing.T) {
		t.Parallel()

		authSpend := testMessageSpendPath(1)
		forfeitSpend := testMessageSpendPath(2)
		outpoint := wire.OutPoint{Index: 11}

		msg := &JoinRoundRequest{
			ForfeitRequests: []*types.ForfeitRequest{{
				VTXOOutpoint: &outpoint,
				AuthSpend:    authSpend,
				ForfeitSpend: forfeitSpend,
			}},
		}

		result := msg.ToProto().UnwrapOrFail(t)
		pb, ok := result.(*roundpb.JoinRoundRequest)
		require.True(t, ok)
		require.Len(t, pb.GetForfeitRequests(), 1)
		require.Equal(
			t, mustEncodeMessageSpendPath(t, authSpend),
			pb.GetForfeitRequests()[0].GetAuthSpendPath(),
		)
		require.Equal(
			t, mustEncodeMessageSpendPath(t, forfeitSpend),
			pb.GetForfeitRequests()[0].GetForfeitSpendPath(),
		)
	})

	t.Run("SubmitNoncesRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		txid := chainhash.HashH([]byte("test-tx"))
		signerKey := NewSignerKey(pubKey)
		var nonce tree.Musig2PubNonce
		copy(nonce[:], []byte{0x01, 0x02})

		msg := &SubmitNoncesRequest{
			RoundID: testRoundIDForMsg("round-001"),
			Nonces: map[SignerKey]map[tree.TxID]tree.Musig2PubNonce{
				signerKey: {
					txid: nonce,
				},
			},
		}

		result := msg.ToProto().UnwrapOrFail(t)
		require.NotNil(t, result)
	})

	t.Run("SubmitPartialSigRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		fakeTxid := chainhash.HashH([]byte("test-tx"))
		signerKey := NewSignerKey(pubKey)

		// Create a test partial signature.
		var scalar btcec.ModNScalar
		scalar.SetInt(12345)
		partialSig := &musig2.PartialSignature{S: &scalar}

		msg := &SubmitPartialSigRequest{
			RoundID: testRoundIDForMsg("round-001"),
			Signatures: map[SignerKey]map[tree.TxID]*musig2.PartialSignature{ //nolint:ll
				signerKey: {
					fakeTxid: partialSig,
				},
			},
		}

		result := msg.ToProto().UnwrapOrFail(t)
		require.NotNil(t, result)
	})

	t.Run("SubmitForfeitSigRequest_ToProto", func(t *testing.T) {
		t.Parallel()

		msg := &SubmitForfeitSigRequest{
			RoundID: testRoundIDForMsg("round-001"),
			Signatures: []*types.BoardingInputSignature{
				{
					InputIndex: 0,
					Outpoint:   wire.OutPoint{},
				},
			},
		}

		result := msg.ToProto().UnwrapOrFail(t)
		require.NotNil(t, result)
	})

	t.Run("JoinRoundAcceptOutbox_ToProto", func(t *testing.T) {
		t.Parallel()

		var quoteID [32]byte
		for i := range quoteID {
			quoteID[i] = byte(i + 1)
		}

		roundID := testRoundIDForMsg("round-accept")
		msg := &JoinRoundAcceptOutbox{
			RoundID: roundID,
			QuoteID: quoteID,
		}

		raw := msg.ToProto().UnwrapOrFail(t)
		pb, ok := raw.(*roundpb.JoinRoundAccept)
		require.True(
			t, ok, "expected *roundpb.JoinRoundAccept, got %T", raw,
		)
		require.Equal(t, roundID.String(), pb.GetRoundId())
		require.Equal(t, quoteID[:], pb.GetQuoteId())

		// ServiceMethod advertises the envelope route the server
		// ingress is listening on — mismatches here break the
		// handshake in production.
		sm := msg.ServiceMethod()
		require.Equal(t, roundpb.ServiceName, sm.Service)
		require.Equal(t, roundpb.MethodAcceptQuote, sm.Method)
	})

	t.Run("JoinRoundRejectOutbox_ToProto", func(t *testing.T) {
		t.Parallel()

		var quoteID [32]byte
		for i := range quoteID {
			quoteID[i] = byte(0xff - i)
		}

		roundID := testRoundIDForMsg("round-reject")
		msg := &JoinRoundRejectOutbox{
			RoundID: roundID,
			QuoteID: quoteID,
			Reason:  "fee above client cap",
		}

		raw := msg.ToProto().UnwrapOrFail(t)
		pb, ok := raw.(*roundpb.JoinRoundReject)
		require.True(
			t, ok, "expected *roundpb.JoinRoundReject, got %T", raw,
		)
		require.Equal(t, roundID.String(), pb.GetRoundId())
		require.Equal(t, quoteID[:], pb.GetQuoteId())
		require.Equal(t, "fee above client cap", pb.GetReason())

		sm := msg.ServiceMethod()
		require.Equal(t, roundpb.ServiceName, sm.Service)
		require.Equal(t, roundpb.MethodRejectQuote, sm.Method)
	})
}

func testMessageSpendPath(sequence uint32) *arkscript.SpendPath {
	return &arkscript.SpendPath{
		RequiredSequence: sequence,
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: []byte{
				0x51,
				byte(sequence),
			},
			ControlBlock: []byte{
				0xc0,
			},
		},
	}
}

func mustEncodeMessageSpendPath(t *testing.T,
	spend *arkscript.SpendPath) []byte {

	t.Helper()

	raw, err := spend.Encode()
	require.NoError(t, err)

	return raw
}

// TestJoinRoundQuoteReceivedFromProto covers the inbound wire
// conversion from a roundpb.JoinRoundQuote envelope into the client
// FSM's JoinRoundQuoteReceived event. Verifies every field carried on
// the quote round-trips correctly, including the positional VTXO and
// leave amount slices that downstream CommitmentTxReceivedState
// amount-validation relies on.
func TestJoinRoundQuoteReceivedFromProto(t *testing.T) {
	t.Parallel()

	roundID := testRoundIDForMsg("round-quote")

	var quoteID [32]byte
	for i := range quoteID {
		quoteID[i] = byte(i + 1)
	}

	pb := &roundpb.JoinRoundQuote{
		RoundId:        roundID.String(),
		QuoteId:        quoteID[:],
		SealPassNumber: 2,
		OperatorFeeSat: 1_234,
		QuoteExpiresAt: 1_700_000_000,
		RejectReason:   roundpb.QuoteReason_QUOTE_OK,
		VtxoQuotes: []*roundpb.VTXOQuote{
			{
				PkScript: []byte{
					0x51,
					0x20,
					0xa0,
				},
				AmountSat: 50_000,
				RecipientKey: []byte{
					0x02,
					0x01,
				},
			},
			{
				PkScript: []byte{
					0x51,
					0x20,
					0xb0,
				},
				AmountSat: 30_000,
				RecipientKey: []byte{
					0x02,
					0x02,
				},
			},
		},
		LeaveQuotes: []*roundpb.LeaveQuote{
			{
				PkScript: []byte{
					0x00,
					0x14,
					0xcc,
				},
				AmountSat: 20_000,
			},
		},
	}

	var got JoinRoundQuoteReceived
	require.NoError(t, got.FromProto(pb))

	require.Equal(t, roundID, got.RoundID)
	require.NotNil(t, got.Quote)
	require.Equal(t, quoteID, got.Quote.QuoteID)
	require.Equal(t, uint32(2), got.Quote.SealPass)
	require.Equal(t, int64(1_234), got.Quote.OperatorFeeSat)
	require.Equal(t, int64(1_700_000_000), got.Quote.QuoteExpiresAt)
	require.Equal(t,
		roundpb.QuoteReason_QUOTE_OK,
		got.Quote.RejectReason,
	)
	require.Equal(t,
		[]VTXOQuoteEntry{
			{
				PkScript:     []byte{0x51, 0x20, 0xa0},
				AmountSat:    50_000,
				RecipientKey: []byte{0x02, 0x01},
			},
			{
				PkScript:     []byte{0x51, 0x20, 0xb0},
				AmountSat:    30_000,
				RecipientKey: []byte{0x02, 0x02},
			},
		},
		got.Quote.VTXOQuotes,
	)
	require.Equal(t,
		[]LeaveQuoteEntry{
			{
				PkScript:  []byte{0x00, 0x14, 0xcc},
				AmountSat: 20_000,
			},
		},
		got.Quote.LeaveQuotes,
	)
}

// TestJoinRoundQuoteReceivedFromProtoRejectsBadQuoteID covers the
// length-validation guard: a wire-provided quote_id that is not
// exactly 32 bytes must fail the FromProto step so the FSM does not
// silently truncate and accept on the wrong identifier.
func TestJoinRoundQuoteReceivedFromProtoRejectsBadQuoteID(t *testing.T) {
	t.Parallel()

	roundID := testRoundIDForMsg("round-bad-quote")
	pb := &roundpb.JoinRoundQuote{
		RoundId: roundID.String(),
		// Only 16 bytes — must be rejected.
		QuoteId: make([]byte, 16),
	}

	var got JoinRoundQuoteReceived
	err := got.FromProto(pb)
	require.Error(t, err)
	require.Contains(t, err.Error(), "quote_id")
}

// TestOutboxMessagesClientOutMsgSealed ensures that all outbox message types
// implement the ClientOutMsg sealed interface. The clientOutMsgSealed() method
// acts as a compile-time marker preventing external types from implementing
// ClientOutMsg, so this test verifies the marker exists on all expected types.
func TestOutboxMessagesClientOutMsgSealed(t *testing.T) {
	t.Parallel()

	txid := chainhash.HashH([]byte("test-txid"))

	t.Run("JoinRoundRequest", func(t *testing.T) {
		t.Parallel()
		msg := &JoinRoundRequest{}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitNoncesRequest", func(t *testing.T) {
		t.Parallel()
		roundID := testRoundIDForMsg("round-001")
		msg := &SubmitNoncesRequest{RoundID: roundID}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitPartialSigRequest", func(t *testing.T) {
		t.Parallel()
		roundID := testRoundIDForMsg("round-001")
		msg := &SubmitPartialSigRequest{RoundID: roundID}
		msg.clientOutMsgSealed()
	})

	t.Run("SubmitForfeitSigRequest", func(t *testing.T) {
		t.Parallel()
		roundID := testRoundIDForMsg("round-001")
		msg := &SubmitForfeitSigRequest{RoundID: roundID}
		msg.clientOutMsgSealed()
	})

	t.Run("RegisterConfirmationRequest", func(t *testing.T) {
		t.Parallel()
		msg := &RegisterConfirmationRequest{
			CallerID: "caller-001",
			PkScript: []byte{
				0x00,
				0x14,
			},
			TargetConfs: 6,
		}
		msg.clientOutMsgSealed()
	})

	t.Run("VTXOCreatedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &VTXOCreatedNotification{
			VTXOs: []*ClientVTXO{},
		}
		msg.clientOutMsgSealed()
	})

	t.Run("RoundCompletedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &RoundCompletedNotification{
			RoundID: testRoundIDForMsg("round-001"),
			TxID:    txid,
		}
		msg.clientOutMsgSealed()
	})

	t.Run("RoundCheckpointedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &RoundCheckpointedNotification{
			RoundID: testRoundIDForMsg("round-001"),
		}
		msg.clientOutMsgSealed()
	})

	t.Run("RoundFailedNotification", func(t *testing.T) {
		t.Parallel()
		msg := &RoundFailedNotification{
			RoundID:       fn.Some(testRoundIDForMsg("round-001")),
			Reason:        "validation failed",
			Recoverable:   true,
			OriginalError: nil,
		}
		msg.clientOutMsgSealed()
	})

	t.Run("DropCustomForfeitReservation", func(t *testing.T) {
		t.Parallel()
		outpoint := wire.OutPoint{
			Hash: chainhash.Hash{
				91,
			},
			Index: 91,
		}
		msg := &DropCustomForfeitReservation{
			Outpoints: []wire.OutPoint{
				outpoint,
			},
		}
		msg.clientOutMsgSealed()
	})
}
