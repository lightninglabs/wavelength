package lnruntime

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// authorizationSigner checks the durable row from inside the signing boundary.
type authorizationSigner struct {
	input.Signer
	check func(*wire.MsgTx) error
}

// SignOutputRaw observes the claim before performing the real test signature.
func (s authorizationSigner) SignOutputRaw(tx *wire.MsgTx,
	desc *input.SignDescriptor) (input.Signature, error) {

	if err := s.check(tx); err != nil {
		return nil, err
	}

	return s.Signer.SignOutputRaw(tx, desc)
}

// TestFundingAuthorizationRestart verifies that an unsigned claim survives a
// fresh database handle and endpoint, without authorizing a new reservation.
func TestFundingAuthorizationRestart(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	terms, source, key, _ := statusPollChannel(t)
	path := filepath.Join(t.TempDir(), "funding.db")
	open := func() (*arkchannel.Service, func() error) {
		raw := clientdb.NewTestDBHandleFromPath(t, path)
		db := clientdb.NewStore(
			raw.DB, raw.Queries, raw.Backend(), btclog.Disabled,
		)
		store := db.NewArkChannelStore(clock.NewDefaultClock())
		c, err := arkchannel.NewCoordinator(
			store.ForPeer(terms.ClientNodeKey),
		)
		require.NoError(t, err)
		s, err := arkchannel.NewService(
			arkchannel.PartyClient, c,
			&statusPollBootstrapExecutor{},
		)
		require.NoError(t, err)

		return s, raw.DB.Close
	}
	service, closeDB := open()
	_, err := service.RegisterPromotion(ctx, terms)
	require.NoError(t, err)
	initial, err := service.RecordPreparedOOR(ctx, terms.ID, source)
	require.NoError(t, err)
	backend := newStatusPollFundingBackend(
		t, terms.PendingChannelID, terms.Capacity,
	)
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(backend.psbt), false,
	)
	require.NoError(t, err)
	var signed int
	signer := authorizationSigner{
		Signer: input.NewMockSigner([]*btcec.PrivateKey{key}, nil),
		check: func(tx *wire.MsgTx) error {
			signed++
			r, err := service.GetChannel(ctx, terms.ID)
			require.NoError(t, err)
			require.NotNil(t, r.AuthorizedBackingTxID)
			require.Equal(t, tx.TxHash(), *r.AuthorizedBackingTxID)
			require.Nil(t, r.Snapshot.Backing)
			require.Equal(t, initial.Revision+1, r.Revision)
			if signed == 1 {
				return fmt.Errorf("signer unavailable")
			}

			return nil
		},
	}
	var returned []byte
	sign := func(funding NativeFundingBackend, packet *psbt.Packet) error {
		endpoint, err := NewNativeFundingEndpoint(
			arkchannel.PartyClient, funding, signer,
			keychain.KeyDescriptor{
				PubKey: key.PubKey(),
			},
		)
		require.NoError(t, err)
		require.NoError(t, endpoint.BindChannelEventSink(service))
		sig, err := endpoint.SignBacking(
			ctx, terms.ID, terms, source, packet,
		)
		if err != nil {
			return err
		}
		if returned == nil {
			returned = sig.Serialize()
		} else {
			require.Equal(t, returned, sig.Serialize())
		}

		return nil
	}
	require.ErrorContains(t, sign(backend, packet), "signer unavailable")
	require.NoError(t, closeDB())
	service, closeDB = open()
	// A restart has no lnd wallet reservation to choose a new output.
	empty := &auditFundingBackend{}
	require.NoError(t, sign(empty, packet))
	require.NoError(t, closeDB())
	service, _ = open()
	require.NoError(t, sign(empty, packet))
	require.Equal(t, 2, signed)
	replayed, err := service.GetChannel(ctx, terms.ID)
	require.NoError(t, err)
	require.Len(t, replayed.FundingSignature, 64)
	require.NoError(
		t,
		service.AuthorizeFundingBacking(
			ctx, terms, source, wire.OutPoint{
				Hash: *replayed.AuthorizedBackingTxID,
			},
			nil,
		),
	)
	require.Zero(t, empty.calls.Load())
	other, err := cloneFundingPSBT(packet)
	require.NoError(t, err)
	other.UnsignedTx.TxOut[0].PkScript = []byte{0x51}
	require.ErrorContains(t, sign(empty, other), "different backing")
	require.Zero(t, empty.calls.Load())
}

// SignAuthorizedBacking returns the first component-test signature.
func (s *fundingNegotiationSink) SignAuthorizedBacking(_ context.Context,
	_ arkchannel.Terms, txid chainhash.Hash, sign func() ([]byte, error)) (
	[]byte, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	if bound := s.record.AuthorizedBackingTxID; bound == nil ||
		*bound != txid {
		return nil, fmt.Errorf("unexpected signing authorization")
	}
	if s.record.FundingSignature == nil {
		sig, err := sign()
		if err != nil {
			return nil, err
		}
		s.record.FundingSignature = sig
	}

	return s.record.FundingSignature, nil
}

// TestFundingAuthorizationCAS verifies that concurrent native observations
// can claim exactly one template and that installation cannot create a claim.
func TestFundingAuthorizationCAS(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	terms, source, _, _ := statusPollChannel(t)
	s, err := arkchannel.NewService(
		arkchannel.PartyClient, statusPollCoordinator(t),
		&statusPollBootstrapExecutor{},
	)
	require.NoError(t, err)
	_, err = s.RegisterPromotion(ctx, terms)
	require.NoError(t, err)
	before, err := s.RecordPreparedOOR(ctx, terms.ID, source)
	require.NoError(t, err)
	a := wire.OutPoint{Hash: chainhash.Hash{1}}
	b := wire.OutPoint{Hash: chainhash.Hash{2}}
	require.ErrorContains(
		t, s.AuthorizeFundingBacking(ctx, terms, source, a, nil),
		"no signing authorization",
	)
	// Hold both workers at the same observed revision before the CAS.
	ready := make(chan struct{}, 2)
	proceed := make(chan struct{})
	results := make(chan error, 2)
	verify := func() error {
		ready <- struct{}{}
		<-proceed

		return nil
	}
	for _, point := range []wire.OutPoint{a, b} {
		go func() {
			results <- s.AuthorizeFundingBacking(
				ctx, terms, source, point, verify,
			)
		}()
	}
	<-ready
	<-ready
	close(proceed)
	first, second := <-results, <-results
	require.NotEqual(t, first == nil, second == nil)
	if first == nil {
		first = second
	}
	require.ErrorContains(t, first, "different backing")
	after, err := s.GetChannel(ctx, terms.ID)
	require.NoError(t, err)
	require.Equal(t, before.Revision+1, after.Revision)
	require.NotNil(t, after.AuthorizedBackingTxID)
	require.ErrorContains(
		t,
		s.AuthorizeFundingBacking(
			ctx, terms, source, wire.OutPoint{
				Hash: *after.AuthorizedBackingTxID,
			},
			nil,
		),
		"no released signature",
	)
}

// TestFundingAuthorizationCancellation fences an in-flight signature at the
// same durable revision as a normal cancellation.
func TestFundingAuthorizationCancellation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	terms, source, _, _ := statusPollChannel(t)
	service, err := arkchannel.NewService(
		arkchannel.PartyClient, statusPollCoordinator(t),
		&statusPollBootstrapExecutor{},
	)
	require.NoError(t, err)
	_, err = service.RegisterPromotion(ctx, terms)
	require.NoError(t, err)
	_, err = service.RecordPreparedOOR(ctx, terms.ID, source)
	require.NoError(t, err)
	point := wire.OutPoint{Hash: chainhash.Hash{3}}
	require.NoError(
		t, service.AuthorizeFundingBacking(
			ctx, terms, source, point,
			func() error { return nil },
		),
	)
	released, err := service.SignAuthorizedBacking(ctx, terms, point.Hash,
		func() ([]byte, error) {
			_, err := service.RecordLocalEvent(
				ctx, terms.ID, &arkchannel.Fail{
					Reason: "funding cancelled",
				},
			)

			return make([]byte, 64), err
		},
	)
	require.ErrorContains(t, err, "not authorized")
	require.Nil(t, released)
	record, err := service.GetChannel(ctx, terms.ID)
	require.NoError(t, err)
	require.Equal(t, arkchannel.PhaseCancelling, record.Snapshot.Phase)
	require.Nil(t, record.FundingSignature)
}

// AuthorizeFundingBacking implements the local claim on the in-memory
// negotiation sink used by native funding component tests.
func (s *fundingNegotiationSink) AuthorizeFundingBacking(_ context.Context,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	point wire.OutPoint, verify func() error) error {

	s.mu.Lock()
	defer s.mu.Unlock()
	if terms != s.record.Snapshot.Terms ||
		!sameFundingBinding(source, *s.record.Snapshot.Source) {
		return fmt.Errorf("unexpected funding intent")
	}
	if bound := s.record.AuthorizedBackingTxID; bound != nil {
		if *bound != point.Hash {
			return fmt.Errorf("unexpected backing")
		}

		return nil
	}
	if verify == nil {
		return fmt.Errorf("funding authorization required")
	}
	if err := verify(); err != nil {
		return err
	}
	s.record.AuthorizedBackingTxID = &point.Hash

	return nil
}
