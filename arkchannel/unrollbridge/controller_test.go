package unrollbridge

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/unroll"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// fixedChannelStore serves one durable channel to the policy resolver.
type fixedChannelStore struct {
	record arkchannel.Record
}

// Get returns the configured channel.
func (s *fixedChannelStore) Get(_ context.Context, id arkchannel.ID) (
	arkchannel.Record, error) {

	if id != s.record.Snapshot.Terms.ID {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}

	return s.record, nil
}

// Create is unused by the read-only resolver.
func (*fixedChannelStore) Create(context.Context, arkchannel.Snapshot) (
	arkchannel.Record, error) {

	return arkchannel.Record{}, arkchannel.ErrConflict
}

// GetByPendingChannelID is unused by the read-only resolver.
func (*fixedChannelStore) GetByPendingChannelID(context.Context, [32]byte) (
	arkchannel.Record, error) {

	return arkchannel.Record{}, arkchannel.ErrNotFound
}

// GetByChannelPoint is unused by the read-only resolver.
func (*fixedChannelStore) GetByChannelPoint(context.Context, wire.OutPoint) (
	arkchannel.Record, error) {

	return arkchannel.Record{}, arkchannel.ErrNotFound
}

// ListNonTerminal is unused by the read-only resolver.
func (*fixedChannelStore) ListNonTerminal(context.Context) ([]arkchannel.Record,
	error) {

	return nil, nil
}

// CompareAndSwap is unused by the read-only resolver.
func (*fixedChannelStore) CompareAndSwap(context.Context, arkchannel.ID, uint64,
	arkchannel.Snapshot) (arkchannel.Record, error) {

	return arkchannel.Record{}, arkchannel.ErrConflict
}

// materializerRef captures admission and reports the backing as published.
type materializerRef struct {
	request       *unroll.EnsureUnrollRequest
	txid          chainhash.Hash
	completeAfter int
	statusCalls   int
}

// ID returns the fake actor identity.
func (*materializerRef) ID() string {
	return "channel-unroll-test"
}

// Tell accepts unused one-way messages.
func (*materializerRef) Tell(context.Context, unroll.RegistryMsg) error {
	return nil
}

// TryTell accepts unused one-way messages.
func (*materializerRef) TryTell(context.Context, unroll.RegistryMsg) error {
	return nil
}

// Ask serves admission and status requests.
func (r *materializerRef) Ask(_ context.Context,
	message unroll.RegistryMsg) actor.Future[unroll.RegistryResp] {

	promise := actor.NewPromise[unroll.RegistryResp]()
	var response unroll.RegistryResp
	switch message := message.(type) {
	case *unroll.EnsureUnrollRequest:
		r.request = message
		response = &unroll.EnsureUnrollResp{Created: true}

	case *unroll.GetStatusRequest:
		r.statusCalls++
		phase := unroll.PhaseSweepBroadcast
		if r.completeAfter > 0 && r.statusCalls >= r.completeAfter {
			phase = unroll.PhaseCompleted
		}
		response = &unroll.GetStatusResp{
			Found:          true,
			Phase:          phase,
			SweepTxid:      &r.txid,
			ExitPolicyKind: r.request.ExitPolicyKind,
		}
	}
	promise.Complete(fn.Ok(response))

	return promise.Future()
}

// materializerSink records the published-backing callback.
type materializerSink struct {
	event *arkchannel.BackingPublished
}

// Apply captures the materialization event.
func (s *materializerSink) Apply(_ context.Context, _ arkchannel.ID,
	event arkchannel.Event) (arkchannel.Record, error) {

	s.event, _ = event.(*arkchannel.BackingPublished)

	return arkchannel.Record{}, nil
}

// TestChannelExitPolicyReturnsSignedBacking proves the common unroller's final
// spend is exactly the transaction lnd negotiated before OOR commit.
func TestChannelExitPolicyReturnsSignedBacking(t *testing.T) {
	t.Parallel()

	record := channelRecord(t)
	resolver := Resolver{Channels: &fixedChannelStore{record: record}}
	policy, err := resolver.ResolveExitSpendPolicy(
		t.Context(), unroll.ExitSpendPolicyRequest{
			Kind: ExitPolicyKind,
			Ref:  channelRef(record.Snapshot.Terms.ID),
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, record.Snapshot.Terms.VTXO.ChannelDelay, policy.CSVDelay(),
	)

	tx, err := policy.BuildSpendTx(t.Context(), unroll.ExitSpendRequest{
		TargetOutpoint: record.Snapshot.Source.OutPoint,
		TargetOutput: &wire.TxOut{
			Value:    int64(record.Snapshot.Source.Amount),
			PkScript: record.Snapshot.Source.PkScript,
		},
	})
	require.NoError(t, err)
	require.Equal(t, record.Snapshot.Backing.ChannelPoint.Hash,
		tx.TxHash())
	require.Equal(
		t, record.Snapshot.Source.OutPoint, tx.TxIn[0].PreviousOutPoint,
	)
}

// TestControllerAdmitsChannelPolicy proves materialization uses a durable
// channel policy reference and reports only the expected backing transaction.
func TestControllerAdmitsChannelPolicy(t *testing.T) {
	t.Parallel()

	record := channelRecord(t)
	ref := &materializerRef{txid: record.Snapshot.Backing.ChannelPoint.Hash}
	controller, err := NewController(ref)
	require.NoError(t, err)
	sink := &materializerSink{}
	require.NoError(t, controller.BindChannelEventSink(sink))
	require.NoError(
		t,
		controller.MaterializeChannel(
			t.Context(), record.Snapshot.Terms.ID,
			*record.Snapshot.Source, *record.Snapshot.Backing,
		),
	)
	require.NotNil(t, ref.request)
	require.Equal(t, ExitPolicyKind, ref.request.ExitPolicyKind)
	require.Equal(
		t, channelRef(record.Snapshot.Terms.ID),
		ref.request.ExitPolicyRef,
	)
	require.NotNil(t, sink.event)
	require.Equal(t, ref.txid, sink.event.TxID)
}

// TestCooperativeCloseExitPolicyReturnsDirectSettlement proves the unroller
// receives the exact immediate spend of the channel-policy VTXO, not the
// delayed VTXO-to-channel backing transaction.
func TestCooperativeCloseExitPolicyReturnsDirectSettlement(t *testing.T) {
	t.Parallel()

	record := cooperativeCloseRecord(t)
	resolver := Resolver{Channels: &fixedChannelStore{record: record}}
	policy, err := resolver.ResolveExitSpendPolicy(
		t.Context(), unroll.ExitSpendPolicyRequest{
			Kind: CooperativeCloseExitPolicyKind,
			Ref:  channelRef(record.Snapshot.Terms.ID),
		},
	)
	require.NoError(t, err)
	require.Zero(t, policy.CSVDelay())

	tx, err := policy.BuildSpendTx(t.Context(), unroll.ExitSpendRequest{
		TargetOutpoint: record.Snapshot.Source.OutPoint,
		TargetOutput: &wire.TxOut{
			Value:    int64(record.Snapshot.Source.Amount),
			PkScript: record.Snapshot.Source.PkScript,
		},
	})
	require.NoError(t, err)
	require.Equal(t, record.Snapshot.CooperativeClose.TxID, tx.TxHash())
	require.Equal(
		t, record.Snapshot.Source.OutPoint, tx.TxIn[0].PreviousOutPoint,
	)
	require.NotEqual(
		t, record.Snapshot.Backing.ChannelPoint,
		tx.TxIn[0].PreviousOutPoint,
	)
}

// TestControllerWaitsForCooperativeCloseConfirmation verifies lnd archival
// cannot start merely because the direct settlement reached the mempool.
func TestControllerWaitsForCooperativeCloseConfirmation(t *testing.T) {
	t.Parallel()

	record := cooperativeCloseRecord(t)
	ref := &materializerRef{
		txid:          record.Snapshot.CooperativeClose.TxID,
		completeAfter: 2,
	}
	controller, err := NewController(ref)
	require.NoError(t, err)
	controller.pollInterval = time.Millisecond
	require.NoError(
		t,
		controller.SettleCooperativeClose(
			t.Context(), record.Snapshot.Terms.ID,
			*record.Snapshot.Source,
			*record.Snapshot.CooperativeClose,
		),
	)
	require.NotNil(t, ref.request)
	require.Equal(
		t, CooperativeCloseExitPolicyKind, ref.request.ExitPolicyKind,
	)
	require.Equal(t, 2, ref.statusCalls)
}

// channelRecord creates one durable, OOR-finalized channel fixture.
func channelRecord(t *testing.T) arkchannel.Record {
	record, _ := channelRecordWithKeys(t)

	return record
}

// channelRecordWithKeys creates a durable channel fixture and retains the
// policy keys needed to construct a valid direct cooperative settlement.
func channelRecordWithKeys(t *testing.T) (arkchannel.Record,
	[]*btcec.PrivateKey) {

	t.Helper()

	keys := make([]*btcec.PrivateKey, 8)
	for i := range keys {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		keys[i] = key
	}
	compressed := func(key *btcec.PrivateKey) [33]byte {
		var value [33]byte
		copy(value[:], key.PubKey().SerializeCompressed())

		return value
	}
	terms := arkchannel.Terms{
		ID: arkchannel.ID{
			1,
			2,
			3,
		},
		Kind:   arkchannel.KindPromotion,
		Funder: arkchannel.PartyClient,
		PendingChannelID: [32]byte{
			4,
			5,
			6,
		},
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_001,
		}.ToUint64(),
		Capacity:      btcutil.Amount(100_000),
		ClientNodeKey: compressed(keys[0]),
		HubNodeKey:    compressed(keys[1]),
		VTXO: arkchannel.VTXOTerms{
			ClientArkKey:     compressed(keys[2]),
			HubArkKey:        compressed(keys[3]),
			ArkOperatorKey:   compressed(keys[4]),
			ClientChannelKey: compressed(keys[5]),
			HubChannelKey:    compressed(keys[6]),
			FunderKey:        compressed(keys[7]),
			ChannelDelay:     144,
			FunderDelay:      576,
			MinExitDelay:     144,
		},
	}
	policy, pkScript, err := terms.VTXO.Artifacts()
	require.NoError(t, err)
	amount := terms.Capacity + 1_000
	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: chainhash.Hash{7}},
	})
	arkTx.AddTxOut(&wire.TxOut{Value: int64(amount), PkScript: pkScript})
	var rawArk bytes.Buffer
	require.NoError(t, arkTx.Serialize(&rawArk))
	source := arkchannel.VTXOBinding{
		OORSessionID: [32]byte(arkTx.TxHash()),
		OutPoint: wire.OutPoint{
			Hash: arkTx.TxHash(),
		},
		Amount:         amount,
		ArkTransaction: rawArk.Bytes(),
		PolicyTemplate: policy,
		PkScript:       pkScript,
	}
	backingTx := wire.NewMsgTx(2)
	backingTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: source.OutPoint,
		Sequence:         terms.VTXO.ChannelDelay,
		Witness:          wire.TxWitness{bytes.Repeat([]byte{1}, 64)},
	})
	backingTx.AddTxOut(&wire.TxOut{
		Value:    int64(terms.Capacity),
		PkScript: []byte{0x51, 0x20, 1},
	})
	var rawBacking bytes.Buffer
	require.NoError(t, backingTx.Serialize(&rawBacking))
	backing := arkchannel.Backing{
		Transaction: rawBacking.Bytes(),
		ChannelPoint: wire.OutPoint{
			Hash: backingTx.TxHash(),
		},
	}

	return arkchannel.Record{
		Revision: 5,
		Snapshot: arkchannel.Snapshot{
			Terms:        terms,
			Phase:        arkchannel.PhaseActive,
			Source:       &source,
			Backing:      &backing,
			OORFinalized: true,
		},
	}, keys
}

// cooperativeCloseRecord adds a fully signed canonical settlement to the
// ordinary active-channel fixture.
func cooperativeCloseRecord(t *testing.T) arkchannel.Record {
	t.Helper()

	record, keys := channelRecordWithKeys(t)
	p2trScript := func(key *btcec.PrivateKey) []byte {
		return append(
			[]byte{txscript.OP_1, txscript.OP_DATA_32},
			schnorr.SerializePubKey(key.PubKey())...,
		)
	}
	request := arkchannel.CooperativeCloseRequest{
		Initiator:            arkchannel.PartyClient,
		ClientDeliveryScript: p2trScript(keys[0]),
		HubDeliveryScript:    p2trScript(keys[1]),
		FeeRate:              chainfee.SatPerKWeight(1_000),
	}
	template, err := arkchannel.NewCooperativeCloseTemplate(
		record.Snapshot.Terms, *record.Snapshot.Source, request,
		btcutil.Amount(40_000), btcutil.Amount(60_000), 7,
	)
	require.NoError(t, err)
	proposal := template.Proposal()
	tx := wire.NewMsgTx(2)
	require.NoError(
		t,
		tx.Deserialize(
			bytes.NewReader(proposal.Transaction),
		),
	)
	sign := func(party arkchannel.Party, key *btcec.PrivateKey,
		operator bool) input.Signature {

		keyDesc := keychain.KeyDescriptor{PubKey: key.PubKey()}
		var desc *input.SignDescriptor
		if operator {
			desc, err = template.OperatorSignDescriptor(
				record.Snapshot.Terms, keyDesc,
			)
		} else {
			desc, err = template.SignDescriptor(
				record.Snapshot.Terms, party, keyDesc,
			)
		}
		require.NoError(t, err)
		sig, err := input.NewMockSigner(
			[]*btcec.PrivateKey{key}, nil,
		).SignOutputRaw(tx, desc)
		require.NoError(t, err)

		return sig
	}
	settlement, err := template.Complete(
		record.Snapshot.Terms, *record.Snapshot.Source, request,
		sign(arkchannel.PartyClient, keys[2], false),
		sign(arkchannel.PartyHub, keys[3], false),
		sign(0, keys[4], true),
	)
	require.NoError(t, err)
	record.Snapshot.Phase = arkchannel.PhaseCoopCloseSigned
	record.Snapshot.CooperativeCloseRequest = &request
	record.Snapshot.CooperativeClose = &settlement

	return record
}

// channelRef returns the durable hexadecimal channel policy reference.
func channelRef(id arkchannel.ID) string {
	return fmt.Sprintf("%x", id[:])
}

var _ arkchannel.Store = (*fixedChannelStore)(nil)
var _ actor.ActorRef[
	unroll.RegistryMsg, unroll.RegistryResp,
] = (*materializerRef)(nil)
var _ arkchannel.ChannelEventSink = (*materializerSink)(nil)
