package lnruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

var errUnexpectedAuditFundingCall = errors.New("unexpected funding call")

// auditFundingBackend rejects and counts every native funding operation that
// a guarded RPC could reach.
type auditFundingBackend struct {
	NativeFundingBackend

	calls atomic.Int32
}

// ExpectedFundingOutput records an unexpected signer precondition lookup.
func (b *auditFundingBackend) ExpectedFundingOutput(lndfunding.PendingChanID) (
	*wire.TxOut, error) {

	b.calls.Add(1)

	return nil, errUnexpectedAuditFundingCall
}

// RegisterBacking records an unexpected backing registration.
func (b *auditFundingBackend) RegisterBacking(VirtualFunding) error {
	b.calls.Add(1)

	return errUnexpectedAuditFundingCall
}

// FundingFinalized records an unexpected native durability query.
func (b *auditFundingBackend) FundingFinalized(context.Context,
	arkchannel.Terms, arkchannel.Backing) (bool, error) {

	b.calls.Add(1)

	return false, errUnexpectedAuditFundingCall
}

// ChannelActive records an unexpected native link query.
func (b *auditFundingBackend) ChannelActive(context.Context, arkchannel.Terms,
	arkchannel.Backing) (bool, error) {

	b.calls.Add(1)

	return false, errUnexpectedAuditFundingCall
}

// auditFundingSigner counts raw signing attempts while delegating the rest of
// the signer contract to lnd's standard mock.
type auditFundingSigner struct {
	input.Signer

	calls atomic.Int32
}

// SignOutputRaw records an unexpected signing attempt.
func (s *auditFundingSigner) SignOutputRaw(tx *wire.MsgTx,
	desc *input.SignDescriptor) (input.Signature, error) {

	s.calls.Add(1)

	return s.Signer.SignOutputRaw(tx, desc)
}

// auditRecoveryManager counts recovery archive access.
type auditRecoveryManager struct {
	ChannelRecoveryManager

	calls atomic.Int32
}

// InstallRecoveryPackage records an unexpected recovery installation.
func (m *auditRecoveryManager) InstallRecoveryPackage(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding,
	arkchannel.RecoveryPackage) error {

	m.calls.Add(1)

	return errUnexpectedAuditFundingCall
}

// auditRPCExecutor supplies an unreachable action surface to a read-only
// production channel service.
type auditRPCExecutor struct {
	arkchannel.ActionExecutor
}

// auditRecordStore serves one immutable channel record.
type auditRecordStore struct {
	arkchannel.Store

	record arkchannel.Record
}

// Get returns the configured record for its exact channel ID.
func (s *auditRecordStore) Get(_ context.Context, id arkchannel.ID) (
	arkchannel.Record, error) {

	if id != s.record.Snapshot.Terms.ID {
		return arkchannel.Record{}, arkchannel.ErrNotFound
	}

	return s.record, nil
}

// TestFundingPeerRPCRejectsCrossChannelArtifacts proves a valid source from
// another workflow cannot reach signing, backing registration, recovery, or
// native readiness operations under channel A's ID.
func TestFundingPeerRPCRejectsCrossChannelArtifacts(t *testing.T) {
	t.Parallel()

	record, _ := testReceiveIntentRecord(t)
	terms := record.Snapshot.Terms
	sourceA := testIntentBinding(
		t, terms, terms.Capacity+1_000, 0,
	)
	sourceB := testIntentBinding(
		t, terms, terms.Capacity+2_000, 1,
	)
	require.NoError(t, sourceA.Validate(terms))
	require.NoError(t, sourceB.Validate(terms))
	record.Snapshot.Source = sourceA
	record.Snapshot.Backing = &arkchannel.Backing{
		Transaction: []byte{
			1,
			2,
			3,
		},
		ChannelPoint: wire.OutPoint{
			Hash: [32]byte{
				4,
				5,
				6,
			},
		},
	}

	server, backend, signer, recovery := newAuditFundingRPCServer(
		t, record,
	)
	sourceRequest := channelBindingToRPC(*sourceB)
	termsRequest := channelTermsToRPC(terms)

	for _, test := range []struct {
		name string
		call func() error
	}{
		{
			name: "sign backing",
			call: func() error {
				_, err := server.SignBacking(
					t.Context(),
					&arkchannelrpc.SignBackingRequest{
						ChannelId: terms.ID[:],
						Terms:     termsRequest,
						Binding:   sourceRequest,
					},
				)

				return err
			},
		},
		{
			name: "install backing",
			call: func() error {
				_, err := server.InstallBacking(
					t.Context(),
					&arkchannelrpc.InstallBackingRequest{
						ChannelId: terms.ID[:],
						Terms:     termsRequest,
						Binding:   sourceRequest,
					},
				)

				return err
			},
		},
		{
			name: "install recovery",
			call: func() error {
				_, err := server.InstallRecoveryPackage(
					t.Context(),
					&arkchannelrpc.
						InstallRecoveryPackageRequest{
						ChannelId: terms.ID[:],
						Terms:     termsRequest,
						Binding:   sourceRequest,
					},
				)

				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			require.ErrorContains(
				t, err,
				"funding request does not match channel FSM",
			)
		})
	}

	wrongBacking := record.Snapshot.Backing.Clone()
	wrongBacking.Transaction = []byte{9, 9, 9}
	statusRequest := fundingStatusRequest(terms, wrongBacking)
	for _, test := range []struct {
		name string
		call func() error
	}{
		{
			name: "funding finalized",
			call: func() error {
				_, err := server.FundingFinalized(
					t.Context(), statusRequest,
				)

				return err
			},
		},
		{
			name: "channel active",
			call: func() error {
				_, err := server.ChannelActive(
					t.Context(), statusRequest,
				)

				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			require.ErrorContains(
				t, err,
				"funding status does not match channel FSM",
			)
		})
	}

	require.Zero(t, backend.calls.Load())
	require.Zero(t, signer.calls.Load())
	require.Zero(t, recovery.calls.Load())
}

// newAuditFundingRPCServer constructs an authenticated server around the
// supplied production channel service record.
func newAuditFundingRPCServer(t *testing.T, record arkchannel.Record) (
	*FundingPeerRPCServer, *auditFundingBackend, *auditFundingSigner,
	*auditRecoveryManager) {

	t.Helper()

	coordinator, err := arkchannel.NewCoordinator(&auditRecordStore{
		record: record,
	})
	require.NoError(t, err)
	service, err := arkchannel.NewService(
		arkchannel.PartyHub, coordinator, &auditRPCExecutor{},
	)
	require.NoError(t, err)

	return newAuditFundingRPCServerWithService(t, record, service)
}

// newAuditFundingRPCServerWithService constructs an RPC server using one
// caller-owned production channel service.
func newAuditFundingRPCServerWithService(t *testing.T, record arkchannel.Record,
	service *arkchannel.Service) (*FundingPeerRPCServer,
	*auditFundingBackend, *auditFundingSigner, *auditRecoveryManager) {

	t.Helper()

	key := testIntentKey(t)
	backend := &auditFundingBackend{}
	signer := &auditFundingSigner{
		Signer: input.NewMockSigner([]*btcec.PrivateKey{key}, nil),
	}
	endpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyHub, backend, signer, keychain.KeyDescriptor{
			PubKey: key.PubKey(),
		},
	)
	require.NoError(t, err)
	recovery := &auditRecoveryManager{}
	terms := record.Snapshot.Terms
	server, err := NewFundingPeerRPCServer(FundingPeerRPCServerConfig{
		RemoteNode: terms.ClientNodeKey,
		Info: FundingPeerInfo{
			HubNodeKey:       terms.HubNodeKey,
			HubArkKey:        terms.VTXO.HubArkKey,
			HubChannelKey:    terms.VTXO.HubChannelKey,
			HubFunderKey:     terms.VTXO.FunderKey,
			ArkOperatorKey:   terms.VTXO.ArkOperatorKey,
			ChannelDelay:     terms.VTXO.ChannelDelay,
			FunderDelay:      terms.VTXO.FunderDelay,
			MinimumExitDelay: terms.VTXO.MinExitDelay,
		},
		Service:  service,
		Funding:  endpoint,
		Node:     &NativeNode{},
		Recovery: recovery,
	})
	require.NoError(t, err)

	return server, backend, signer, recovery
}

var _ NativeFundingBackend = (*auditFundingBackend)(nil)
var _ arkchannel.Store = (*auditRecordStore)(nil)
