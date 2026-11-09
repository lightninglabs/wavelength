package lnruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
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
