package waved

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	btclog "github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/oor"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// testReceiveScriptStore is a minimal in-memory owned receive-script store used
// by receive-script unit tests.
type testReceiveScriptStore struct {
	records []db.OwnedReceiveScriptRecord
}

// UpsertOwnedReceiveScript stores or replaces one owned receive-script record.
func (s *testReceiveScriptStore) UpsertOwnedReceiveScript(_ context.Context,
	rec db.OwnedReceiveScriptRecord) error {

	for i := range s.records {
		if string(s.records[i].PkScript) == string(rec.PkScript) {
			s.records[i] = rec

			return nil
		}
	}

	s.records = append(s.records, rec)

	return nil
}

// LookupOwnedReceiveScript returns one tracked owned receive-script record.
func (s *testReceiveScriptStore) LookupOwnedReceiveScript(_ context.Context,
	pkScript []byte) (*db.OwnedReceiveScriptRecord, error) {

	for i := range s.records {
		if string(s.records[i].PkScript) != string(pkScript) {
			continue
		}

		rec := s.records[i]

		return &rec, nil
	}

	return nil, sql.ErrNoRows
}

// ListOwnedReceiveScripts returns all tracked owned receive-script records.
func (s *testReceiveScriptStore) ListOwnedReceiveScripts(_ context.Context) (
	[]db.OwnedReceiveScriptRecord, error) {

	records := make([]db.OwnedReceiveScriptRecord, len(s.records))
	copy(records, s.records)

	return records, nil
}

// TestEnsureDefaultOORReceiveKeyReusesPersistedKey verifies that the helper
// prefers the most recent persisted wallet-managed receive key.
func TestEnsureDefaultOORReceiveKeyReusesPersistedKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	olderKey := testKeyDescriptor(t, 1)
	newerKey := testKeyDescriptor(t, 2)
	store := &testReceiveScriptStore{
		records: []db.OwnedReceiveScriptRecord{
			{
				PkScript: []byte{
					0x51,
				},
				ClientKey:  olderKey,
				Source:     db.OwnedReceiveScriptSourceWallet,
				CreatedAt:  time.Unix(10, 0),
				LastUsedAt: fn.None[time.Time](),
			},
			{
				PkScript: []byte{
					0x52,
				},
				ClientKey:  newerKey,
				Source:     db.OwnedReceiveScriptSourceWallet,
				CreatedAt:  time.Unix(20, 0),
				LastUsedAt: fn.None[time.Time](),
			},
		},
	}

	derived := false
	keyDesc, err := EnsureDefaultOORReceiveKey(
		ctx, store, func(context.Context) (*keychain.KeyDescriptor,
			error) {

			derived = true

			return nil, nil
		},
	)
	require.NoError(t, err)
	require.False(t, derived)
	require.NotNil(t, keyDesc)
	require.Equal(
		t, newerKey.PubKey.SerializeCompressed(),
		keyDesc.PubKey.SerializeCompressed(),
	)
}

// TestCreateOORReceiveScriptDerivesFreshKeys verifies that each allocation
// derives a new wallet key, registers the matching script, and persists a
// distinct ownership record.
func TestCreateOORReceiveScriptDerivesFreshKeys(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &testReceiveScriptStore{}
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	keys := []keychain.KeyDescriptor{
		testKeyDescriptor(t, 11),
		testKeyDescriptor(t, 12),
	}

	nextKey := 0
	rpcClient := &testReceiveScriptRPCClient{}
	idx := indexer.New(
		rpcClient, nil, "test-server", "client:test",
		fn.None[btclog.Logger](),
	)

	signerFactory := func(
		keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {

		return &testOwnedReceiveScriptSigner{
			keyDesc: keyDesc,
			tagSig:  []byte("test-tag-signature"),
		}
	}

	_, pkScriptA, err := CreateOORReceiveScript(
		ctx, idx, store,
		func(context.Context) (*keychain.KeyDescriptor, error) {
			key := keys[nextKey]
			nextKey++

			return &key, nil
		},
		signerFactory, operatorPriv.PubKey(), 144,
		"test-oor-receive-a",
	)
	require.NoError(t, err)

	_, pkScriptB, err := CreateOORReceiveScript(
		ctx, idx, store,
		func(context.Context) (*keychain.KeyDescriptor, error) {
			key := keys[nextKey]
			nextKey++

			return &key, nil
		},
		signerFactory, operatorPriv.PubKey(), 144,
		"test-oor-receive-b",
	)
	require.NoError(t, err)

	require.NotEqual(t, pkScriptA, pkScriptB)
	require.Len(t, store.records, 2)
	require.Equal(
		t, keys[0].PubKey.SerializeCompressed(),
		store.records[0].ClientKey.PubKey.SerializeCompressed(),
	)
	require.Equal(
		t, keys[1].PubKey.SerializeCompressed(),
		store.records[1].ClientKey.PubKey.SerializeCompressed(),
	)
	require.Len(t, rpcClient.registerReqs, 2)
	require.Equal(t, pkScriptA, rpcClient.registerReqs[0].PkScript)
	require.Equal(t, pkScriptB, rpcClient.registerReqs[1].PkScript)
}

// TestNewOwnedReceiveScriptSignerUsesMatchingKey verifies that proofs are
// produced with the wallet key associated with the queried pkScript.
func TestNewOwnedReceiveScriptSignerUsesMatchingKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	keyA := testKeyDescriptor(t, 21)
	keyB := testKeyDescriptor(t, 22)
	store := &testReceiveScriptStore{
		records: []db.OwnedReceiveScriptRecord{
			{
				PkScript: []byte{
					0x51,
					0x20,
					0x01,
				},
				ClientKey:      keyA,
				Source:         db.OwnedReceiveScriptSourceWallet, //nolint:ll
				CreatedAt:      time.Unix(10, 0),
				LastUsedAt:     fn.None[time.Time](),
				OperatorPubKey: testKeyDescriptor(t, 31).PubKey,
			},
			{
				PkScript: []byte{
					0x51,
					0x20,
					0x02,
				},
				ClientKey:      keyB,
				Source:         db.OwnedReceiveScriptSourceWallet, //nolint:ll
				CreatedAt:      time.Unix(11, 0),
				LastUsedAt:     fn.None[time.Time](),
				OperatorPubKey: testKeyDescriptor(t, 32).PubKey,
			},
		},
	}

	signer := NewOwnedReceiveScriptSigner(
		store,
		func(keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {
			return &testOwnedReceiveScriptSigner{
				keyDesc: keyDesc,
				tagSig: []byte{
					keyDesc.PubKey.SerializeCompressed()[0],
					keyDesc.PubKey.SerializeCompressed()[1],
				},
			}
		},
	)

	pubKeySource, ok := signer.(interface {
		ProofPubKey([]byte) (*btcec.PublicKey, error)
	})
	require.True(t, ok)

	proofPubA, err := pubKeySource.ProofPubKey(store.records[0].PkScript)
	require.NoError(t, err)
	require.Equal(
		t, keyA.PubKey.SerializeCompressed(),
		proofPubA.SerializeCompressed(),
	)

	msgSigner, ok := signer.(interface {
		SignSchnorrMessage(context.Context, []byte, []byte,
			[]byte) ([]byte, error)
	})
	require.True(t, ok)

	sigA, err := msgSigner.SignSchnorrMessage(
		ctx, store.records[0].PkScript, []byte("msg"), []byte("tag"),
	)
	require.NoError(t, err)

	sigB, err := msgSigner.SignSchnorrMessage(
		ctx, store.records[1].PkScript, []byte("msg"), []byte("tag"),
	)
	require.NoError(t, err)

	require.NotEqual(t, sigA, sigB)
}

// TestEnsureDefaultOORReceiveKeyDerivesWhenMissing verifies that the helper
// falls back to the provided wallet derivation path when no stored key exists.
func TestEnsureDefaultOORReceiveKeyDerivesWhenMissing(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &testReceiveScriptStore{}
	expected := testKeyDescriptor(t, 3)

	keyDesc, err := EnsureDefaultOORReceiveKey(
		ctx, store, func(context.Context) (*keychain.KeyDescriptor,
			error) {

			return &expected, nil
		},
	)
	require.NoError(t, err)
	require.NotNil(t, keyDesc)
	require.Equal(
		t, expected.PubKey.SerializeCompressed(),
		keyDesc.PubKey.SerializeCompressed(),
	)
}

// TestResolveOwnedReceiveScriptKeyNotOwned verifies that an unregistered
// receive script is reported using the OOR recipient-not-owned sentinel so
// mixed-recipient packages can skip foreign outputs.
func TestResolveOwnedReceiveScriptKeyNotOwned(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	recipient := oor.ArkRecipientOutput{
		OutputIndex: 0,
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
		Value: 1000,
	}

	_, err := ResolveOwnedReceiveScriptKey(
		ctx, &testReceiveScriptStore{}, recipient,
	)
	require.ErrorIs(t, err, oor.ErrIncomingRecipientNotOwned)
}

// testKeyDescriptor creates a deterministic test key descriptor.
func testKeyDescriptor(t *testing.T, seed byte) keychain.KeyDescriptor {
	t.Helper()

	privKey, _ := btcec.PrivKeyFromBytes(
		[]byte{
			seed, seed, seed, seed, seed, seed, seed, seed,
			seed, seed, seed, seed, seed, seed, seed, seed,
			seed, seed, seed, seed, seed, seed, seed, seed,
			seed, seed, seed, seed, seed, seed, seed, seed,
		},
	)

	return keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: oorReceiveKeyFamily,
			Index:  uint32(seed),
		},
		PubKey: privKey.PubKey(),
	}
}

// testOwnedReceiveScriptSigner is a minimal signer stub keyed by one wallet
// descriptor.
type testOwnedReceiveScriptSigner struct {
	keyDesc keychain.KeyDescriptor
	tagSig  []byte
}

// SignSchnorr returns a deterministic 64-byte signature for raw-digest paths.
func (s *testOwnedReceiveScriptSigner) SignSchnorr(_ []byte, _ [32]byte) (
	[]byte, error) {

	return make([]byte, 64), nil
}

// SignSchnorrMessage returns a deterministic tagged-message signature.
func (s *testOwnedReceiveScriptSigner) SignSchnorrMessage(_ context.Context,
	_ []byte, _ []byte, _ []byte) ([]byte, error) {

	return append([]byte(nil), s.tagSig...), nil
}

// ProofPubKey returns the wallet key bound to this signer.
func (s *testOwnedReceiveScriptSigner) ProofPubKey(_ []byte) (*btcec.PublicKey,
	error) {

	return s.keyDesc.PubKey, nil
}

// testReceiveScriptRPCClient records mailbox RPC requests for registration.
type testReceiveScriptRPCClient struct {
	registerReqs []*arkrpc.RegisterReceiveScriptRequest
}

// SendRPC records RegisterReceiveScript requests and returns a fixed result.
func (c *testReceiveScriptRPCClient) SendRPC(_ context.Context,
	method mailboxrpc.ServiceMethod, req proto.Message,
	_ mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	if method.Service == "arkrpc.IndexerService" &&
		method.Method == "RegisterReceiveScript" {

		regReq, ok := req.(*arkrpc.RegisterReceiveScriptRequest)
		if !ok {
			return mailboxrpc.SendResult{}, fmt.Errorf(
				"unexpected register request type: %T", req)
		}

		c.registerReqs = append(c.registerReqs, regReq)
	}

	return mailboxrpc.SendResult{
		CorrelationID:  "corr-id",
		IdempotencyKey: "idem-key",
	}, nil
}

// AwaitRPC populates the response expected by the generated mailbox client.
func (c *testReceiveScriptRPCClient) AwaitRPC(_ context.Context, _ string,
	resp proto.Message) error {

	registerResp, ok := resp.(*arkrpc.RegisterReceiveScriptResponse)
	if !ok {
		return fmt.Errorf("unexpected response type: %T", resp)
	}

	*registerResp = arkrpc.RegisterReceiveScriptResponse{}

	return nil
}

// stubSchnorrSigner is a narrow test double that returns a canned error from
// SignSchnorr so we can exercise fallback-signer error propagation without
// standing up a real signer factory.
type stubSchnorrSigner struct {
	err error
}

func (s *stubSchnorrSigner) SignSchnorr(_ []byte, _ [32]byte) ([]byte, error) {
	return nil, s.err
}

// TestFallbackSchnorrSignerPreservesPrimaryError verifies that when both the
// primary and fallback signers fail, the wrapper preserves both errors so the
// original cause is recoverable via errors.Is.
func TestFallbackSchnorrSignerPreservesPrimaryError(t *testing.T) {
	t.Parallel()

	primaryErr := fmt.Errorf("primary: %w", errors.New("script not found"))
	fallbackErr := fmt.Errorf("fallback: %w", errors.New("key unavailable"))

	signer := NewFallbackSchnorrSigner(
		&stubSchnorrSigner{
			err: primaryErr,
		}, &stubSchnorrSigner{
			err: fallbackErr,
		},
	)

	_, err := signer.SignSchnorr(nil, [32]byte{})
	require.Error(t, err)
	require.ErrorIs(
		t, err, primaryErr, "primary error must remain recoverable",
	)
	require.ErrorIs(
		t, err, fallbackErr,
		"fallback error must be reported alongside primary",
	)
}
