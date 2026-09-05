package waved

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/indexer"
	libtree "github.com/lightninglabs/wavelength/lib/tree"
	libtypes "github.com/lightninglabs/wavelength/lib/types"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// recoveryTestExitDelay is the operator's VTXO exit delay used to build the
// receive scripts under test.
const recoveryTestExitDelay = 144

// recoveryTestAncestryPath builds the minimal valid ancestry path an indexer
// VTXO needs to survive recovery's conversion step.
func recoveryTestAncestryPath(t *testing.T,
	commitmentTxID chainhash.Hash) *arkrpc.AncestryPath {

	t.Helper()

	tree := &libtree.Tree{
		Root: &libtree.Node{},
		BatchOutpoint: wire.OutPoint{
			Hash: commitmentTxID,
		},
	}

	path, err := arkrpc.AncestryPathFromTree(
		tree, commitmentTxID, []uint32{0},
	)
	require.NoError(t, err)

	return path
}

// recoveryKeyBackend derives a distinct deterministic key per key locator so a
// test can tell the families recovery scans apart by the scripts they produce.
type recoveryKeyBackend struct{}

// DeriveKey returns a deterministic key for the requested locator.
func (b *recoveryKeyBackend) DeriveKey(_ context.Context,
	loc keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	seed := byte(uint32(loc.Family)*31 + loc.Index + 1)
	privKey, _ := btcec.PrivKeyFromBytes([]byte{
		seed, seed, seed, seed, seed, seed, seed, seed,
		seed, seed, seed, seed, seed, seed, seed, seed,
		seed, seed, seed, seed, seed, seed, seed, seed,
		seed, seed, seed, seed, seed, seed, seed, seed,
	})

	return &keychain.KeyDescriptor{
		KeyLocator: loc,
		PubKey:     privKey.PubKey(),
	}, nil
}

// DeriveNextKey is unused by the indexed-VTXO scan.
func (b *recoveryKeyBackend) DeriveNextKey(_ context.Context,
	_ keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	return nil, nil
}

// ProofSigner returns a canned proof signer for the requested wallet key.
func (b *recoveryKeyBackend) ProofSigner(
	keyDesc keychain.KeyDescriptor) indexer.SchnorrSigner {

	return &testOwnedReceiveScriptSigner{
		keyDesc: keyDesc,
		tagSig:  make([]byte, 64),
	}
}

// recoveryIndexerStub answers indexer queries from a fixed script-to-VTXO map,
// so a scan only finds a VTXO if it actually asked about the right script.
type recoveryIndexerStub struct {
	mu        sync.Mutex
	byScript  map[string]*arkrpc.VTXO
	lastReq   proto.Message
	askedFor  [][]byte
	registers int
}

// SendRPC records the outgoing request and returns a static correlation pair.
func (s *recoveryIndexerStub) SendRPC(_ context.Context,
	_ mailboxrpc.ServiceMethod, req proto.Message,
	_ mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastReq = proto.Clone(req)
	switch typed := s.lastReq.(type) {
	case *arkrpc.RegisterReceiveScriptRequest:
		s.registers++

	case *arkrpc.ListVTXOsByScriptsRequest:
		for _, scope := range typed.GetScripts() {
			s.askedFor = append(
				s.askedFor,
				append(
					[]byte(nil), scope.GetPkScript()...,
				),
			)
		}
	}

	return mailboxrpc.SendResult{
		CorrelationID:  "corr-1",
		IdempotencyKey: "idemp-1",
	}, nil
}

// AwaitRPC answers the recorded request. A list query returns the configured
// VTXO for the queried script and nothing for any other script.
func (s *recoveryIndexerStub) AwaitRPC(_ context.Context, _ string,
	resp proto.Message) error {

	s.mu.Lock()
	defer s.mu.Unlock()

	listResp, ok := resp.(*arkrpc.ListVTXOsByScriptsResponse)
	if !ok {
		return nil
	}

	listReq, ok := s.lastReq.(*arkrpc.ListVTXOsByScriptsRequest)
	if !ok {
		return nil
	}

	for _, scope := range listReq.GetScripts() {
		indexed, ok := s.byScript[string(scope.GetPkScript())]
		if !ok {
			continue
		}

		listResp.Vtxos = append(listResp.Vtxos, indexed)
	}

	return nil
}

// scriptQueried reports whether the scan asked the indexer about pkScript.
func (s *recoveryIndexerStub) scriptQueried(pkScript []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, asked := range s.askedFor {
		if string(asked) == string(pkScript) {
			return true
		}
	}

	return false
}

// newRecoveryScanServer builds an RPC server wired to a real VTXO store and a
// stub indexer holding one live VTXO on the supplied script.
func newRecoveryScanServer(t *testing.T, liveScript []byte,
	live *arkrpc.VTXO) (*RPCServer, *recoveryIndexerStub) {

	t.Helper()

	walletReady := make(chan struct{})
	close(walletReady)

	stub := &recoveryIndexerStub{
		byScript: map[string]*arkrpc.VTXO{
			string(liveScript): live,
		},
	}
	// The indexer validates registration expiry against the wall clock.
	clk := clock.NewTestClock(time.Now())
	sqliteDB := db.NewTestDB(t)

	// The scan only touches vtxoStore; leaving the concrete db field unset
	// keeps this buildable under both database tags.
	return NewRPCServer(&Server{
		walletReady: walletReady,
		clk:         clk,
		vtxoStore: db.NewStore(
			sqliteDB.DB, sqliteDB.Queries, sqliteDB.Backend(),
			btclog.Disabled,
		).NewVTXOStore(clk),
		proofKeyBackend: &recoveryKeyBackend{},
		expiryAuthenticator: func(context.Context, []vtxo.Ancestry) (
			int32, error) {

			return 965281, nil
		},
		indexer: indexer.New(
			stub, nil, "test-server", "client:test",
			fn.None[btclog.Logger](),
		),
	}), stub
}

// TestRecoverIndexedVTXOsCoversOORReceiveScripts verifies seed recovery finds
// a live VTXO sitting on an OOR receive script, which the VTXO-owner family
// scan alone misses.
func TestRecoverIndexedVTXOsCoversOORReceiveScripts(t *testing.T) {
	t.Parallel()

	operatorKey := testKeyDescriptor(t, 200)
	terms := &libtypes.OperatorTerms{
		PubKey:        operatorKey.PubKey,
		VTXOExitDelay: recoveryTestExitDelay,
	}

	backend := &recoveryKeyBackend{}
	oorKey, err := backend.DeriveKey(t.Context(), keychain.KeyLocator{
		Family: oorReceiveKeyFamily,
		Index:  0,
	})
	require.NoError(t, err)

	oorScript, err := BuildPubKeyVTXOReceiveScript(
		oorKey.PubKey, terms.PubKey, terms.VTXOExitDelay,
	)
	require.NoError(t, err)

	commitmentTxID := chainhash.Hash{0xaa}
	outpointTxID := chainhash.Hash{0xbb}
	live := &arkrpc.VTXO{
		Outpoint: &arkrpc.OutPoint{
			Txid: outpointTxID[:],
			Vout: 0,
		},
		ValueSat:          28674,
		PkScript:          oorScript,
		Status:            arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		RoundId:           "round-1",
		CommitmentTxid:    commitmentTxID[:],
		CreatedHeight:     964273,
		BatchExpiryHeight: 965281,
		RelativeExpiry:    recoveryTestExitDelay,
		AncestryPaths: []*arkrpc.AncestryPath{
			recoveryTestAncestryPath(t, commitmentTxID),
		},
	}

	// The VTXO-owner family alone never queries the OOR receive script.
	ownerOnly, ownerStub := newRecoveryScanServer(t, oorScript, live)
	var ownerResult WalletRecoveryResult
	require.NoError(
		t,
		ownerOnly.recoverIndexedVTXOs(
			t.Context(), terms, libtypes.VTXOOwnerKeyFamily, 1,
			&ownerResult,
		),
	)
	require.Zero(t, ownerResult.VTXOs)
	require.False(t, ownerStub.scriptQueried(oorScript))

	// The full family list finds and persists it.
	full, fullStub := newRecoveryScanServer(t, oorScript, live)
	var fullResult WalletRecoveryResult
	for _, family := range recoveryIndexedVTXOFamilies {
		require.NoError(
			t,
			full.recoverIndexedVTXOs(
				t.Context(), terms, family, 1, &fullResult,
			),
		)
	}
	require.Equal(t, uint32(1), fullResult.VTXOs)
	require.True(t, fullStub.scriptQueried(oorScript))

	outpoint, err := recoveryOutpoint(live.GetOutpoint())
	require.NoError(t, err)

	saved, err := full.server.vtxoStore.GetVTXO(t.Context(), outpoint)
	require.NoError(t, err)
	require.NotNil(t, saved)
	require.Equal(t, oorScript, saved.PkScript)
}
