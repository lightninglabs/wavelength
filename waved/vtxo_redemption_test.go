package waved

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/indexer"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type redemptionRPCClient struct {
	requests []*arkrpc.CheckVTXORedeemabilityRequest
}

// SendRPC records selective redemption requests for signer/chunk assertions.
func (c *redemptionRPCClient) SendRPC(_ context.Context,
	method mailboxrpc.ServiceMethod, req proto.Message,
	_ mailboxrpc.RPCOptions) (mailboxrpc.SendResult, error) {

	if method.Method != "CheckVTXORedeemability" {
		return mailboxrpc.SendResult{}, fmt.Errorf("unexpected "+
			"method %s", method.Method)
	}
	redemptionReq, ok := req.(*arkrpc.CheckVTXORedeemabilityRequest)
	if !ok {
		return mailboxrpc.SendResult{}, fmt.Errorf("unexpected "+
			"request %T", req)
	}
	cloned := proto.Clone(redemptionReq)
	clonedRequest, ok := cloned.(*arkrpc.CheckVTXORedeemabilityRequest)
	if !ok {
		return mailboxrpc.SendResult{}, fmt.Errorf("clone returned %T",
			cloned)
	}
	c.requests = append(c.requests, clonedRequest)

	return mailboxrpc.SendResult{
		CorrelationID:  fmt.Sprintf("redemption-%d", len(c.requests)),
		IdempotencyKey: fmt.Sprintf("redemption-%d", len(c.requests)),
	}, nil
}

// AwaitRPC returns an empty sparse response.
func (c *redemptionRPCClient) AwaitRPC(_ context.Context, _ string,
	resp proto.Message) error {

	result, ok := resp.(*arkrpc.CheckVTXORedeemabilityResponse)
	if !ok {
		return fmt.Errorf("unexpected response %T", resp)
	}
	*result = arkrpc.CheckVTXORedeemabilityResponse{}

	return nil
}

type redemptionProofKeyBackend struct {
	participant *btcec.PrivateKey
	requested   []keychain.KeyDescriptor
}

// DeriveKey is unused by selective reconciliation.
func (b *redemptionProofKeyBackend) DeriveKey(context.Context,
	keychain.KeyLocator) (*keychain.KeyDescriptor, error) {

	return nil, fmt.Errorf("unexpected key derivation")
}

// DeriveNextKey is unused by the checker.
func (b *redemptionProofKeyBackend) DeriveNextKey(context.Context,
	keychain.KeyFamily) (*keychain.KeyDescriptor, error) {

	return nil, fmt.Errorf("unexpected next-key derivation")
}

// ProofSigner binds the proof to the descriptor participant key.
func (b *redemptionProofKeyBackend) ProofSigner(
	desc keychain.KeyDescriptor) indexer.SchnorrSigner {

	b.requested = append(b.requested, desc)

	return &indexer.PrivKeySchnorrSigner{Key: b.participant}
}

// TestRedemptionResultsFromRPC verifies the sparse positive-only wire response
// preserves both redeemable sources and completed replacement links.
func TestRedemptionResultsFromRPC(t *testing.T) {
	t.Parallel()

	const claimRoundID = "019f80f6-8094-70fe-abb4-932a2ae9af20"

	source := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	redeemedSource := wire.OutPoint{Hash: chainhash.Hash{3}, Index: 4}
	replacement := wire.OutPoint{Hash: chainhash.Hash{5}, Index: 6}

	results, err := redemptionResultsFromRPC(
		&arkrpc.CheckVTXORedeemabilityResponse{
			ClaimRoundId: claimRoundID,
			RedeemableOutpoints: []*arkrpc.OutPoint{
				redemptionOutpointToRPC(source),
			},
			Redemptions: []*arkrpc.VTXORedemption{{
				SourceOutpoint: redemptionOutpointToRPC(
					redeemedSource,
				),
				ReplacementOutpoint: redemptionOutpointToRPC(
					replacement,
				),
			}},
		},
	)
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.Equal(t, source, results[0].Source)
	require.True(t, results[0].Redeemable)
	require.Equal(t, claimRoundID, results[0].ClaimRoundID)
	require.Nil(t, results[0].Replacement)
	require.Equal(t, redeemedSource, results[1].Source)
	require.False(t, results[1].Redeemable)
	require.Equal(t, replacement, *results[1].Replacement)
}

// TestRedemptionResultsFromRPCRejectsInvalidClaimRoundID verifies claim
// adoption cannot proceed without one canonical, concrete server round.
func TestRedemptionResultsFromRPCRejectsInvalidClaimRoundID(t *testing.T) {
	t.Parallel()

	outpoint := redemptionOutpointToRPC(wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 2,
	})
	tests := []struct {
		name string
		resp *arkrpc.CheckVTXORedeemabilityResponse
	}{
		{
			name: "missing round ID",
			resp: &arkrpc.CheckVTXORedeemabilityResponse{
				RedeemableOutpoints: []*arkrpc.OutPoint{
					outpoint,
				},
			},
		},
		{
			name: "malformed round ID",
			resp: &arkrpc.CheckVTXORedeemabilityResponse{
				ClaimRoundId: "not-a-round-id",
				RedeemableOutpoints: []*arkrpc.OutPoint{
					outpoint,
				},
			},
		},
		{
			name: "round ID without redeemables",
			resp: &arkrpc.CheckVTXORedeemabilityResponse{
				ClaimRoundId: "019f80f6-8094-70fe-" +
					"abb4-932a2ae9af20",
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := redemptionResultsFromRPC(test.resp)
			require.Error(t, err)
		})
	}
}

// TestCheckVTXORedeemabilityUsesParticipantAndChunks proves custom/N-party
// claims are authorized by Descriptor.ClientKey rather than the daemon
// identity, and one signer group is chunked to the indexer request cap.
func TestCheckVTXORedeemabilityUsesParticipantAndChunks(t *testing.T) {
	t.Parallel()

	participant, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	identity, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	outputKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pkScript := append(
		[]byte{0x51, 0x20},
		schnorr.SerializePubKey(outputKey.PubKey())...,
	)
	keyDesc := keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: 91,
			Index:  7,
		},
		PubKey: participant.PubKey(),
	}

	rpcClient := &redemptionRPCClient{}
	backend := &redemptionProofKeyBackend{participant: participant}
	server := &Server{
		indexer: indexer.New(
			rpcClient, &indexer.PrivKeySchnorrSigner{
				Key: identity,
			}, "operator", "client:test",
			fn.None[btclog.Logger](),
		),
		proofKeyBackend: backend,
	}
	descriptors := make(
		[]*vtxo.Descriptor, indexer.MaxVTXORedeemabilityOutpoints+1,
	)
	for i := range descriptors {
		descriptors[i] = &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					byte(i),
					byte(i >> 8),
				},
				Index: uint32(i),
			},
			PkScript:  pkScript,
			ClientKey: keyDesc,
		}
	}

	results, err := server.checkVTXORedeemability(
		t.Context(), descriptors,
	)
	require.NoError(t, err)
	require.Empty(t, results)
	require.Len(t, backend.requested, 1)
	require.Equal(t, keyDesc.KeyLocator, backend.requested[0].KeyLocator)
	require.True(
		t,
		backend.requested[0].PubKey.IsEqual(
			participant.PubKey(),
		),
	)
	require.Len(t, rpcClient.requests, 2)
	require.Len(
		t, rpcClient.requests[0].GetOutpoints(),
		indexer.MaxVTXORedeemabilityOutpoints,
	)
	require.Len(t, rpcClient.requests[1].GetOutpoints(), 1)

	for _, request := range rpcClient.requests {
		require.Len(t, request.GetScripts(), 1)
		proof := request.GetScripts()[0].GetTaprootSchnorr()
		require.NotNil(t, proof)
		signature, err := schnorr.ParseSignature(proof.GetSig64())
		require.NoError(t, err)
		digest := chainhash.TaggedHash(
			[]byte("wavelength/indexer/v1"), proof.GetMessage(),
		)
		require.True(
			t,
			signature.Verify(
				digest[:], participant.PubKey(),
			),
		)
		require.False(t, signature.Verify(digest[:], identity.PubKey()))
	}
}

// TestRedemptionDescriptorFromIndexerPreservesHistoricalPolicy verifies a
// recovered replacement combines authoritative lineage with the exact source
// amount, script and opaque custom policy instead of rebuilding current terms.
func TestRedemptionDescriptorFromIndexerPreservesHistoricalPolicy(
	t *testing.T) {

	t.Parallel()
	participant, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	historicalOperator, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	settlementKeys := []*btcec.PublicKey{
		participant.PubKey(), peer.PubKey(),
		historicalOperator.PubKey(),
	}
	exitKeys := []*btcec.PublicKey{
		participant.PubKey(), peer.PubKey(),
	}
	policy := &arkscript.PolicyTemplate{
		Leaves: []arkscript.LeafTemplate{
			{
				Node: &arkscript.Multisig{
					Keys: settlementKeys,
				},
			},
			{
				Node: &arkscript.CSV{
					Lock: 1008,
					Inner: &arkscript.Multisig{
						Keys: exitKeys,
					},
				},
			},
		},
	}
	policyTemplate, err := policy.Encode()
	require.NoError(t, err)
	pkScript, err := policy.PkScript()
	require.NoError(t, err)
	require.False(t, arkscript.IsStandardVTXOTemplate(policy))

	source := &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				7,
			},
			Index: 8,
		},
		Amount:         btcutil.Amount(75_000),
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
		ClientKey: keychain.KeyDescriptor{
			PubKey: participant.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 71,
				Index:  72,
			},
		},
		OperatorKey:    historicalOperator.PubKey(),
		RelativeExpiry: 1008,
		BatchExpiry:    1_500,
	}
	replacement := wire.OutPoint{
		Hash: chainhash.Hash{
			9,
		},
		Index: 10,
	}
	commitment := chainhash.Hash{11}
	indexed := &arkrpc.VTXO{
		Outpoint:          redemptionOutpointToRPC(replacement),
		ValueSat:          uint64(source.Amount),
		PkScript:          append([]byte(nil), source.PkScript...),
		Status:            arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		RoundId:           "replacement-round",
		CommitmentTxid:    commitment[:],
		CreatedHeight:     1_000,
		BatchExpiryHeight: 2_000,

		// Round-created indexer rows expose the batch sweep delay here,
		// not the custom policy's locally selected CSV path.
		RelativeExpiry: 300,
		ChainDepth:     1,
		AncestryPaths: []*arkrpc.AncestryPath{
			testAncestryPath(commitment),
		},
	}

	descriptor, err := redemptionDescriptorFromIndexer(source, indexed)
	require.NoError(t, err)
	require.Equal(t, replacement, descriptor.Outpoint)
	require.Equal(t, source.Amount, descriptor.Amount)
	require.Equal(t, source.PolicyTemplate, descriptor.PolicyTemplate)
	require.Equal(t, source.PkScript, descriptor.PkScript)
	require.Equal(t, source.RelativeExpiry, descriptor.RelativeExpiry)
	require.True(t, descriptor.OperatorKey.IsEqual(source.OperatorKey))
	require.Equal(t, vtxo.VTXOStatusLive, descriptor.Status)
	require.Equal(t, commitment, descriptor.CommitmentTxID)
	require.Equal(t, int32(1_000), descriptor.CreatedHeight)
	require.Equal(t, int32(2_000), descriptor.BatchExpiry)

	indexed.Status = arkrpc.VTXOStatus_VTXO_STATUS_EXPIRED
	descriptor, err = redemptionDescriptorFromIndexer(source, indexed)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusExpired, descriptor.Status)

	indexed.ValueSat--
	_, err = redemptionDescriptorFromIndexer(source, indexed)
	require.ErrorContains(t, err, "amount changed")
	indexed.ValueSat = uint64(source.Amount)
	indexed.BatchExpiryHeight = source.BatchExpiry
	_, err = redemptionDescriptorFromIndexer(source, indexed)
	require.ErrorContains(t, err, "batch expiry")
	indexed.BatchExpiryHeight = 2_000

	corruptSource := *source
	corruptSource.PkScript = append([]byte(nil), source.PkScript...)
	corruptSource.PkScript[len(corruptSource.PkScript)-1] ^= 1
	_, err = redemptionDescriptorFromIndexer(&corruptSource, indexed)
	require.ErrorContains(t, err, "source policy does not match")
}

// redemptionOutpointToRPC converts a test outpoint to its detached wire form.
func redemptionOutpointToRPC(outpoint wire.OutPoint) *arkrpc.OutPoint {
	return &arkrpc.OutPoint{
		Txid: append([]byte(nil), outpoint.Hash[:]...),
		Vout: outpoint.Index,
	}
}
