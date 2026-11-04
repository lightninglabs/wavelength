package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// newTestAssetDepositStore returns a deposit store on a fresh database.
func newTestAssetDepositStore(t *testing.T) *AssetDepositStore {
	t.Helper()

	base := NewTestDB(t)

	return &AssetDepositStore{
		TransactionExecutor: NewTransactionExecutor(
			base.BaseDB,
			func(tx *sql.Tx) *sqlc.Queries {
				return base.WithTx(tx)
			},
			btclog.Disabled,
		),
		now: time.Now,
	}
}

// testAssetDepositRequest returns a complete deposit request.
func testAssetDepositRequest(t *testing.T, id string) *AssetDepositRequest {
	t.Helper()

	_, operatorKey := btcec.PrivKeyFromBytes([]byte{0x02})

	return &AssetDepositRequest{
		RequestID:       id,
		AssetRef:        "asset-ref",
		AssetAmount:     ^uint64(0),
		CarrierValueSat: 10_000,
		TargetConf:      6,
		MaxFeeSat:       5_000,
		OperatorKey:     operatorKey,
		ExitDelay:       144,
		FundingProofs: [][]byte{
			{
				0x01,
				0x02,
			},
			{
				0x03,
			},
		},
	}
}

// testAssetDepositOutput returns a complete published output.
func testAssetDepositOutput(t *testing.T) *AssetDepositOutput {
	t.Helper()

	_, ownerKey := btcec.PrivKeyFromBytes([]byte{0x03})

	return &AssetDepositOutput{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				7,
			},
			Index: 1,
		},
		ValueSat: 10_000,
		FeeSat:   321,
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
		PolicyTemplate: []byte{
			0x0a,
		},
		OwnerKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 42,
				Index:  9,
			},
			PubKey: ownerKey,
		},
		Digest: [32]byte{
			0x0d,
		},
		ScriptKey: ownerKey.SerializeCompressed(),
		TaprootAssetRoot: chainhash.Hash{
			0x0e,
		},
		OPTrueWitness: [][]byte{
			{
				0x51,
			},
			{},
		},
	}
}

// TestAssetDepositStoreLifecycle walks a deposit from request to confirmed
// proof, including the idempotent replays of every step.
func TestAssetDepositStoreLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTestAssetDepositStore(t)
	req := testAssetDepositRequest(t, "deposit-1")

	created, err := store.CreateAssetDeposit(ctx, req)
	require.NoError(t, err)
	require.Equal(t, AssetDepositFunding, created.Status)
	require.Nil(t, created.Output)
	require.True(t, assetDepositRequestsEqual(&created.Request, req))

	// The same request replays, and a different one under the same ID
	// is refused.
	_, err = store.CreateAssetDeposit(ctx, req)
	require.NoError(t, err)
	changed := *req
	changed.AssetAmount = 5
	_, err = store.CreateAssetDeposit(ctx, &changed)
	require.ErrorIs(t, err, ErrAssetDepositMismatch)

	require.NoError(
		t, store.SetAssetDepositError(
			ctx, req.RequestID, "tapd unavailable",
		),
	)
	funding, err := store.ListAssetDepositsByStatus(
		ctx, AssetDepositFunding,
	)
	require.NoError(t, err)
	require.Len(t, funding, 1)
	require.Equal(t, "tapd unavailable", funding[0].LastError)

	// Confirming before publishing is refused.
	err = store.MarkAssetDepositConfirmed(ctx, req.RequestID, []byte{1}, 10)
	require.ErrorIs(t, err, ErrAssetDepositMismatch)

	output := testAssetDepositOutput(t)
	require.NoError(
		t, store.MarkAssetDepositPublished(
			ctx, req.RequestID, output,
		),
	)
	require.NoError(
		t, store.MarkAssetDepositPublished(
			ctx, req.RequestID, output,
		),
	)
	other := *output
	other.Outpoint.Index = 2
	err = store.MarkAssetDepositPublished(ctx, req.RequestID, &other)
	require.ErrorIs(t, err, ErrAssetDepositMismatch)

	published, err := store.GetAssetDeposit(ctx, req.RequestID)
	require.NoError(t, err)
	require.Equal(t, AssetDepositPublished, published.Status)
	require.Empty(t, published.LastError)
	require.Equal(t, output, published.Output)

	proof := []byte("proof")
	require.NoError(
		t, store.MarkAssetDepositConfirmed(
			ctx, req.RequestID, proof, 101,
		),
	)
	require.NoError(
		t, store.MarkAssetDepositConfirmed(
			ctx, req.RequestID, proof, 101,
		),
	)
	err = store.MarkAssetDepositConfirmed(
		ctx, req.RequestID, []byte("other"), 101,
	)
	require.ErrorIs(t, err, ErrAssetDepositMismatch)

	// A confirmed deposit keeps no error.
	require.NoError(
		t, store.SetAssetDepositError(ctx, req.RequestID,
			"late"),
	)
	confirmed, err := store.GetAssetDeposit(ctx, req.RequestID)
	require.NoError(t, err)
	require.Equal(t, AssetDepositConfirmed, confirmed.Status)
	require.Equal(t, proof, confirmed.ProofFile)
	require.EqualValues(t, 101, confirmed.ConfirmedHeight)
	require.Empty(t, confirmed.LastError)

	_, err = store.GetAssetDeposit(ctx, "missing")
	require.ErrorIs(t, err, ErrAssetDepositNotFound)
}

// TestAssetDepositStoreOutpointUnique checks that two deposits cannot claim
// the same output.
func TestAssetDepositStoreOutpointUnique(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTestAssetDepositStore(t)
	output := testAssetDepositOutput(t)

	for _, id := range []string{"a", "b"} {
		_, err := store.CreateAssetDeposit(
			ctx, testAssetDepositRequest(t, id),
		)
		require.NoError(t, err)
	}
	require.NoError(t, store.MarkAssetDepositPublished(ctx, "a", output))
	require.Error(t, store.MarkAssetDepositPublished(ctx, "b", output))

	deposits, err := store.ListAssetDeposits(ctx)
	require.NoError(t, err)
	require.Len(t, deposits, 2)
}

// TestAssetDepositRequestValidation checks the bounds the table enforces.
func TestAssetDepositRequestValidation(t *testing.T) {
	t.Parallel()

	store := newTestAssetDepositStore(t)
	mutations := map[string]func(*AssetDepositRequest){
		"no id": func(r *AssetDepositRequest) {
			r.RequestID = ""
		},
		"no amount": func(r *AssetDepositRequest) {
			r.AssetAmount = 0
		},
		"no carrier": func(r *AssetDepositRequest) {
			r.CarrierValueSat = 0
		},
		"no operator": func(r *AssetDepositRequest) {
			r.OperatorKey = nil
		},
		"no delay": func(r *AssetDepositRequest) {
			r.ExitDelay = 0
		},
		"no proofs": func(r *AssetDepositRequest) {
			r.FundingProofs = nil
		},
	}
	for name, mutate := range mutations {
		req := testAssetDepositRequest(t, "x")
		mutate(req)
		_, err := store.CreateAssetDeposit(t.Context(), req)
		require.Error(t, err, name)
	}
}
