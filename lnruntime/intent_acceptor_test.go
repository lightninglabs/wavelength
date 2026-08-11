package lnruntime

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// staticIntentSource returns one durable intent for acceptor tests.
type staticIntentSource struct {
	record arkchannel.Record
	err    error
}

// FindByPendingChannelID implements ChannelIntentSource.
func (s *staticIntentSource) FindByPendingChannelID(_ context.Context,
	pendingID [32]byte) (arkchannel.Record, error) {

	if s.err != nil {
		return arkchannel.Record{}, s.err
	}
	if s.record.Snapshot.Terms.PendingChannelID != pendingID {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}

	return s.record, nil
}

// TestIntentAcceptorRejectsUnregisteredFunding verifies lnd cannot accept a
// channel by merely presenting plausible channel parameters.
func TestIntentAcceptorRejectsUnregisteredFunding(t *testing.T) {
	t.Parallel()

	record, hubKey := testReceiveIntentRecord(t)
	source := &staticIntentSource{record: record}
	acceptor, err := NewIntentAcceptor(arkchannel.PartyClient, source)
	require.NoError(t, err)

	validRequest := func() *chanacceptor.ChannelAcceptRequest {
		return &chanacceptor.ChannelAcceptRequest{
			Node: hubKey,
			OpenChanMsg: &lnwire.OpenChannel{
				PendingChannelID: record.Snapshot.Terms.
					PendingChannelID,
				FundingAmount: record.Snapshot.Terms.Capacity,
			},
		}
	}

	require.False(t, acceptor.Accept(validRequest()).RejectChannel())

	tests := []struct {
		name   string
		mutate func(*chanacceptor.ChannelAcceptRequest)
	}{
		{
			name: "unknown pending channel",
			mutate: func(req *chanacceptor.ChannelAcceptRequest) {
				req.OpenChanMsg.PendingChannelID[0] ^= 1
			},
		},
		{
			name: "wrong initiator",
			mutate: func(req *chanacceptor.ChannelAcceptRequest) {
				key, keyErr := btcec.NewPrivateKey()
				require.NoError(t, keyErr)
				req.Node = key.PubKey()
			},
		},
		{
			name: "wrong capacity",
			mutate: func(req *chanacceptor.ChannelAcceptRequest) {
				req.OpenChanMsg.FundingAmount++
			},
		},
		{
			name: "pushed client balance",
			mutate: func(req *chanacceptor.ChannelAcceptRequest) {
				req.OpenChanMsg.PushAmount = 1
			},
		},
		{
			name: "public channel",
			mutate: func(req *chanacceptor.ChannelAcceptRequest) {
				req.OpenChanMsg.ChannelFlags =
					lnwire.FFAnnounceChannel
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			req := validRequest()
			test.mutate(req)

			require.True(t, acceptor.Accept(req).RejectChannel())
		})
	}
}

// TestIntentAcceptorRequiresBoundVTXO verifies registration alone cannot
// allocate lnd responder state before the prepared OOR transfer fixes the
// source output.
func TestIntentAcceptorRequiresBoundVTXO(t *testing.T) {
	t.Parallel()

	record, hubKey := testReceiveIntentRecord(t)
	record.Snapshot.Source = nil
	record.Snapshot.Phase = arkchannel.PhaseRequested
	acceptor, err := NewIntentAcceptor(
		arkchannel.PartyClient, &staticIntentSource{
			record: record,
		},
	)
	require.NoError(t, err)
	pendingID := record.Snapshot.Terms.PendingChannelID

	response := acceptor.Accept(&chanacceptor.ChannelAcceptRequest{
		Node: hubKey,
		OpenChanMsg: &lnwire.OpenChannel{
			PendingChannelID: pendingID,
			FundingAmount:    record.Snapshot.Terms.Capacity,
		},
	})
	require.True(t, response.RejectChannel())
}

// testReceiveIntentRecord creates one source-bound hub-funded intent.
func testReceiveIntentRecord(t *testing.T) (arkchannel.Record,
	*btcec.PublicKey) {

	t.Helper()
	clientNode := testIntentKey(t)
	hubNode := testIntentKey(t)
	terms := arkchannel.Terms{
		ID: arkchannel.ID{
			1,
		},
		Kind:   arkchannel.KindReceiveIntent,
		Funder: arkchannel.PartyHub,
		PendingChannelID: [32]byte{
			2,
		},
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_001,
			TxIndex:     1,
		}.ToUint64(),
		Capacity:      btcutil.Amount(100_000),
		ClientNodeKey: compressedIntentKey(clientNode),
		HubNodeKey:    compressedIntentKey(hubNode),
		PaymentHash: [32]byte{
			3,
		},
		VTXO: arkchannel.VTXOTerms{
			ClientArkKey:     compressedIntentKey(testIntentKey(t)),
			HubArkKey:        compressedIntentKey(testIntentKey(t)),
			ArkOperatorKey:   compressedIntentKey(testIntentKey(t)),
			ClientChannelKey: compressedIntentKey(testIntentKey(t)),
			HubChannelKey:    compressedIntentKey(testIntentKey(t)),
			FunderKey:        compressedIntentKey(testIntentKey(t)),
			ChannelDelay:     144,
			FunderDelay:      576,
			MinExitDelay:     144,
		},
	}

	return arkchannel.Record{
		Revision: 2,
		Snapshot: arkchannel.Snapshot{
			Terms:  terms,
			Phase:  arkchannel.PhaseNegotiating,
			Source: testIntentBinding(t, terms, terms.Capacity, 0),
		},
	}, hubNode.PubKey()
}

// testIntentBinding creates a canonical unsigned Ark transaction containing
// the exact channel-policy output.
func testIntentBinding(t *testing.T, terms arkchannel.Terms,
	amount btcutil.Amount, outputIndex uint32) *arkchannel.VTXOBinding {

	t.Helper()
	policy, pkScript, err := terms.VTXO.Artifacts()
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{4, 2}},
	})
	for i := uint32(0); i < outputIndex; i++ {
		tx.AddTxOut(
			&wire.TxOut{
				Value:    int64(i + 1),
				PkScript: []byte{0x51},
			},
		)
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    int64(amount),
		PkScript: pkScript,
	})
	var raw bytes.Buffer
	require.NoError(t, tx.Serialize(&raw))
	sessionID := [32]byte(tx.TxHash())

	return &arkchannel.VTXOBinding{
		OORSessionID: sessionID,
		OutPoint: wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: outputIndex,
		},
		Amount:         amount,
		ArkTransaction: raw.Bytes(),
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}
}

// testIntentKey creates one policy or node key.
func testIntentKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return key
}

// compressedIntentKey converts one test key to the persisted representation.
func compressedIntentKey(key *btcec.PrivateKey) [33]byte {
	var serialized [33]byte
	copy(serialized[:], key.PubKey().SerializeCompressed())

	return serialized
}

var _ ChannelIntentSource = (*staticIntentSource)(nil)
