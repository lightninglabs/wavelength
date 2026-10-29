package waved

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/internal/expiryfixture"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// indexedRecoveryFixture funds a vHTLC from a foreign signed round leaf via
// a checkpoint and Ark transaction, without seeding any local descriptors.
func indexedRecoveryFixture(t *testing.T) (vhtlcrecovery.RecoveryJob,
	*arkrpc.VTXO) {

	t.Helper()
	sender, _ := btcec.PrivKeyFromBytes([]byte{1})
	receiver, _ := btcec.PrivKeyFromBytes([]byte{2})
	operator, _ := btcec.PrivKeyFromBytes([]byte{3})
	hash := sha256.Sum256([]byte("recovery preimage"))
	job := vhtlcrecovery.RecoveryJob{
		ID: "foreign-claim", Action: vhtlcrecovery.ActionClaim,
		VTXOAmountSat:  20000,
		SenderPubkey:   sender.PubKey().SerializeCompressed(),
		ReceiverPubkey: receiver.PubKey().SerializeCompressed(),
		ServerPubkey:   operator.PubKey().SerializeCompressed(),
		PreimageHash:   hash[:], RefundLocktime: 500,
		UnilateralClaimDelay: 6, UnilateralRefundDelay: 12,
		UnilateralRefundWithoutReceiverDelay: 18,
		SignerKeyFamily:                      7, SignerKeyIndex: 2,
	}
	policy, err := recoveryPolicy(job)
	require.NoError(t, err)
	targetScript, err := policy.PkScript()
	require.NoError(t, err)
	key, pub := btcec.PrivKeyFromBytes([]byte{4})
	parentScript, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(pub),
	)
	require.NoError(t, err)
	candidate, _ := expiryfixture.Round(
		t, 20000, parentScript, 144, 100, 12,
	)
	var parentID chainhash.Hash
	copy(parentID[:], candidate.Outpoint.Txid)
	parent := wire.OutPoint{Hash: parentID}
	previous := &wire.TxOut{Value: 20000, PkScript: parentScript}
	checkpoint := signedRecoveryPacket(
		t, parent, previous, parentScript, key,
	)
	checkpointID := checkpoint.UnsignedTx.TxHash()
	ark := signedRecoveryPacket(
		t, wire.OutPoint{
			Hash: checkpointID,
		},
		previous,
		targetScript,
		key,
	)
	arkID := ark.UnsignedTx.TxHash()
	job.VTXOOutpoint = wire.OutPoint{Hash: arkID}
	arkRaw, err := psbtutil.Serialize(ark)
	require.NoError(t, err)
	checkpointRaw, err := psbtutil.Serialize(checkpoint)
	require.NoError(t, err)
	candidate.Outpoint = &arkrpc.OutPoint{Txid: arkID[:]}
	candidate.PkScript = targetScript
	candidate.ChainDepth = 1
	candidate.AncestryPackages = []*arkrpc.OORSessionPackage{{
		SessionId: arkID[:], ArkPsbt: arkRaw,
		CheckpointPsbts: [][]byte{
			checkpointRaw,
		},
	}}

	return job, candidate
}

// signedRecoveryPacket creates one signed bridge using actual prevout data.
func signedRecoveryPacket(t *testing.T, parent wire.OutPoint,
	previous *wire.TxOut, script []byte,
	key *btcec.PrivateKey) *psbt.Packet {

	t.Helper()
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: parent,
		Sequence: wire.MaxTxInSequenceNum})
	tx.AddTxOut(&wire.TxOut{Value: previous.Value, PkScript: script})
	tx.AddTxOut(arkscript.AnchorOutput())
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	fetcher := txscript.NewCannedPrevOutputFetcher(
		previous.PkScript, previous.Value,
	)
	sig, err := txscript.RawTxInTaprootSignature(
		tx, txscript.NewTxSigHashes(tx, fetcher), 0, previous.Value,
		previous.PkScript, nil, txscript.SigHashDefault, key,
	)
	require.NoError(t, err)
	packet.Inputs[0].TaprootKeySpendSig = sig
	packet.Inputs[0].WitnessUtxo = previous

	return packet
}

// newIndexedRecoveryMaterializer gives each test empty real persistence stores.
func newIndexedRecoveryMaterializer(t *testing.T,
	candidate *arkrpc.VTXO) *vhtlcRecoveryTargetMaterializer {

	t.Helper()
	testDB := db.NewTestDB(t)
	clk := clock.NewTestClock(time.Now())
	store := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	)
	m := newVHTLCRecoveryTargetMaterializer(
		store.NewVTXOStore(clk), store.NewOORArtifactStore(clk),
		btclog.Disabled,
	)
	m.fetchTarget = func(context.Context, vhtlcrecovery.RecoveryJob) (
		*arkrpc.VTXO, error) {

		return candidate, nil
	}
	m.authenticateExpiry = func(context.Context, []vtxo.Ancestry) (
		vtxo.AuthenticatedBatchEvidence, error) {

		return vtxo.AuthenticatedBatchEvidence{
			BatchExpiry:              244,
			EarliestCommitmentHeight: 100,
		}, nil
	}

	return m
}

// TestIndexedRecoverySurvivesRestart proves a foreign funded target yields a
// complete local proof and remains recoverable without the indexer afterwards.
func TestIndexedRecoverySurvivesRestart(t *testing.T) {
	t.Parallel()
	job, candidate := indexedRecoveryFixture(t)
	candidate.CreatedHeight = 245
	m := newIndexedRecoveryMaterializer(t, candidate)
	require.NoError(t, m.EnsureRecoveryTarget(t.Context(), job))
	desc, err := m.vtxos.GetVTXO(t.Context(), job.VTXOOutpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusUnilateralExit, desc.Status)
	require.Equal(t, uint32(6), desc.RelativeExpiry)
	require.Equal(t, int32(100), desc.CreatedHeight)
	require.Equal(t, int32(244), desc.BatchExpiry)
	live, err := m.vtxos.ListLiveVTXOs(t.Context())
	require.NoError(t, err)
	require.Empty(t, live)
	assembler := &unroll.LocalProofAssembler{
		VTXOStore: m.vtxos, ArtifactStore: m.packages,
	}
	_, err = assembler.EnsureProof(t.Context(), job.VTXOOutpoint)
	require.NoError(t, err)

	restarted := newVHTLCRecoveryTargetMaterializer(
		m.vtxos, m.packages, btclog.Disabled,
	)
	restarted.fetchTarget = func(context.Context,
		vhtlcrecovery.RecoveryJob) (*arkrpc.VTXO, error) {

		return nil, fmt.Errorf("indexer offline")
	}
	require.NoError(t, restarted.EnsureRecoveryTarget(t.Context(), job))
	_, err = assembler.EnsureProof(t.Context(), job.VTXOOutpoint)
	require.NoError(t, err)
}

// TestIndexedRecoveryRejectsUntrustedFunding ensures malformed remote data and
// unconfirmed expiry never commit a target descriptor or any funding package.
func TestIndexedRecoveryRejectsUntrustedFunding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*vhtlcrecovery.RecoveryJob, *arkrpc.VTXO,
			*vhtlcRecoveryTargetMaterializer)
	}{
		{name: "wrong outpoint", mutate: func(
			_ *vhtlcrecovery.RecoveryJob, v *arkrpc.VTXO,
			_ *vhtlcRecoveryTargetMaterializer) {

			v.Outpoint.Vout++
		}},
		{name: "wrong amount", mutate: func(
			j *vhtlcrecovery.RecoveryJob, _ *arkrpc.VTXO,
			_ *vhtlcRecoveryTargetMaterializer) {

			j.VTXOAmountSat++
		}},
		{name: "wrong policy", mutate: func(
			_ *vhtlcrecovery.RecoveryJob, v *arkrpc.VTXO,
			_ *vhtlcRecoveryTargetMaterializer) {

			v.PkScript[2] ^= 1
		}},
		{name: "broken funding signature", mutate: func(
			_ *vhtlcrecovery.RecoveryJob, v *arkrpc.VTXO,
			_ *vhtlcRecoveryTargetMaterializer) {

			pkg := v.AncestryPackages[0]
			packet, err := psbtutil.Parse(pkg.ArkPsbt)
			require.NoError(t, err)
			packet.Inputs[0].TaprootKeySpendSig[0] ^= 1
			pkg.ArkPsbt, err = psbtutil.Serialize(packet)
			require.NoError(t, err)
		}},
		{name: "missing creating package", mutate: func(
			_ *vhtlcrecovery.RecoveryJob, v *arkrpc.VTXO,
			_ *vhtlcRecoveryTargetMaterializer) {

			v.AncestryPackages = nil
		}},
		{name: "unknown construction", mutate: func(
			_ *vhtlcrecovery.RecoveryJob, v *arkrpc.VTXO,
			_ *vhtlcRecoveryTargetMaterializer) {

			v.ConstructionVersion = 99
		}},
		{name: "expiry not confirmed", mutate: func(
			_ *vhtlcrecovery.RecoveryJob, _ *arkrpc.VTXO,
			m *vhtlcRecoveryTargetMaterializer) {

			m.authenticateExpiry = func(context.Context,
				[]vtxo.Ancestry) (
				vtxo.AuthenticatedBatchEvidence, error) {

				return vtxo.AuthenticatedBatchEvidence{},
					fmt.Errorf("commitment not confirmed")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job, candidate := indexedRecoveryFixture(t)
			m := newIndexedRecoveryMaterializer(t, candidate)
			tc.mutate(&job, candidate, m)
			require.Error(
				t,
				m.EnsureRecoveryTarget(
					t.Context(), job,
				),
			)
			_, err := m.vtxos.GetVTXO(t.Context(), job.VTXOOutpoint)
			require.ErrorIs(t, err, vtxo.ErrVTXONotFound)
			packages, err := m.packages.ListPackages(
				t.Context(), nil,
			)
			require.NoError(t, err)
			require.Empty(t, packages)
		})
	}
}

// TestFetchIndexedRecoveryTargetUsesPolicy authenticates as the recovery key
// without requiring a pre-existing receive-script registration.
func TestFetchIndexedRecoveryTargetUsesPolicy(t *testing.T) {
	t.Parallel()
	job, candidate := indexedRecoveryFixture(t)
	rpc, stub := newRecoveryScanServer(t, candidate.PkScript, candidate)
	got, err := rpc.server.fetchIndexedRecoveryTarget(t.Context(), job)
	require.NoError(t, err)
	require.Equal(t, candidate.Outpoint, got.Outpoint)
	require.Equal(t, 0, stub.registers)
	req, ok := stub.lastReq.(*arkrpc.ListVTXOsByScriptsRequest)
	require.True(t, ok)
	require.Len(t, req.GetScripts(), 1)
	proof := req.GetScripts()[0].GetTaprootSchnorr()
	require.NotEmpty(t, proof.GetMessage())
	policy, err := recoveryPolicy(job)
	require.NoError(t, err)
	template, err := policy.Template.Encode()
	require.NoError(t, err)
	// The authenticated policy proof commits to the canonical template,
	// so the operator can check the signer is a settlement participant.
	require.Contains(t, string(proof.GetMessage()), string(template))
}

// TestRecoveryDescriptorAcceptsOperatorParity preserves Taproot's x-only
// identity when round metadata and the recovery tuple use opposite parities.
func TestRecoveryDescriptorAcceptsOperatorParity(t *testing.T) {
	t.Parallel()
	job, candidate := indexedRecoveryFixture(t)
	m := newIndexedRecoveryMaterializer(t, candidate)
	require.NoError(t, m.EnsureRecoveryTarget(t.Context(), job))
	meta, err := m.vtxos.GetVTXO(t.Context(), job.VTXOOutpoint)
	require.NoError(t, err)
	job.ServerPubkey[0] ^= 1
	desc, err := recoveryDescriptor(job, meta)
	require.NoError(t, err)
	require.Equal(t, meta.PolicyTemplate, desc.PolicyTemplate)

	_, different := btcec.PrivKeyFromBytes([]byte{77})
	meta.OperatorKey = different
	_, err = recoveryDescriptor(job, meta)
	require.ErrorContains(t, err, "operator key mismatch")
}
