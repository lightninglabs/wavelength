package swaps

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// receiveDiscoveryWallet keeps each wallet's ordinary script registration
// separate from its materialized coins. No swap state is shared with it.
type receiveDiscoveryWallet struct {
	script []byte
	owned  *vtxo.OwnedReceiveScript
	coins  map[wire.OutPoint]*vtxo.Descriptor
}

// LookupOwnedReceiveScript implements the normal receive-script lookup.
func (w *receiveDiscoveryWallet) LookupOwnedReceiveScript(_ context.Context,
	script []byte) (*vtxo.OwnedReceiveScript, error) {

	if !bytes.Equal(script, w.script) {
		return nil, sql.ErrNoRows
	}

	return w.owned, nil
}

// SaveVTXO records the descriptor produced by the production incoming handler.
func (w *receiveDiscoveryWallet) SaveVTXO(_ context.Context,
	desc *vtxo.Descriptor) error {

	w.coins[desc.Outpoint] = desc

	return nil
}

// discoveryClaimDaemon captures the output requested by the receive FSM at
// the simulated daemon boundary, including its actual amount.
type discoveryClaimDaemon struct {
	*testDaemonConn
	amount int64
}

// SendOORWithCustomInputsToAddress captures the external claim output.
func (d *discoveryClaimDaemon) SendOORWithCustomInputsToAddress(
	ctx context.Context, address string, amount int64,
	inputs []CustomInput) (string, error) {

	d.amount = amount

	return d.testDaemonConn.SendOORWithCustomInputsToAddress(
		ctx, address, amount, inputs,
	)
}

// TestExternalReceiveRecipientDiscoveryAndSpend integrates the receive FSM
// with two wallets' normal incoming handlers and script-level spending. The
// Lightning service, claim submission, indexer delivery, and wallet stores
// are simulated; this is not a live two-daemon settlement test.
func TestExternalReceiveRecipientDiscoveryAndSpend(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client, daemon, _, _ := externalReceiveFixture(t)
	claimDaemon := &discoveryClaimDaemon{testDaemonConn: daemon}
	client.daemon = claimDaemon
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	daemon.operatorKey = operator.PubKey()

	// Both wallets have ordinary, independent receive keys. The recipient
	// registers its script before the invoice exists, as for any Ark
	// receive.
	newWallet := func() (*receiveDiscoveryWallet, *btcec.PrivateKey) {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		policy, err := arkscript.NewVTXOPolicy(
			key.PubKey(), operator.PubKey(), 144,
		)
		require.NoError(t, err)
		script, err := txscript.PayToTaprootScript(policy.OutputKey())
		require.NoError(t, err)

		return &receiveDiscoveryWallet{
			script: script,
			owned: &vtxo.OwnedReceiveScript{
				ClientKey: keychain.KeyDescriptor{
					PubKey: key.PubKey(),
				},
				OperatorPubKey: operator.PubKey(),
				ExitDelay:      144,
			},
			coins: make(map[wire.OutPoint]*vtxo.Descriptor),
		}, key
	}
	originator, originatorKey := newWallet()
	recipient, recipientKey := newWallet()
	daemon.identityKey = originatorKey.PubKey()
	address, err := btcaddr.NewAddressTaproot(
		recipient.script[2:], &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	session, err := client.StartReceiveViaLightningWithOptions(
		ctx, 42_000, ReceiveOptions{
			ClaimAddress: address.EncodeAddress(),
		},
	)
	require.NoError(t, err)
	_, err = session.Wait(ctx)
	require.NoError(t, err)
	require.Equal(t, ReceiveStateCompleted, session.State())
	require.Equal(t, int64(42_000), claimDaemon.amount)
	require.Equal(t, address.EncodeAddress(), daemon.lastClaimAddress)
	require.Zero(t, daemon.receiveAllocCalls)

	// Simulate the operator indexing the output actually requested by the
	// claim. Deliver only an ordinary CREATED event, never swap metadata or
	// a preimage, to both wallets to check ownership isolation as well.
	claimAddress, err := btcaddr.DecodeAddress(
		daemon.lastClaimAddress, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	claimScript, err := txscript.PayToAddrScript(claimAddress)
	require.NoError(t, err)
	claimTx := wire.NewMsgTx(2)
	claimTx.AddTxIn(wire.NewTxIn(&wire.OutPoint{}, nil, nil))
	claimTx.AddTxOut(wire.NewTxOut(claimDaemon.amount, claimScript))
	claimHash := claimTx.TxHash()
	event := &arkrpc.IncomingVTXOEvent{
		EventId: 1,
		Type:    arkrpc.VTXOEventType_VTXO_EVENT_TYPE_CREATED,
		Outpoint: &arkrpc.OutPoint{
			Txid: claimHash[:],
		},
		PkScript:          claimTx.TxOut[0].PkScript,
		ValueSat:          uint64(claimTx.TxOut[0].Value),
		RoundId:           "external-receive-round",
		BatchExpiryHeight: 100_000,
		RelativeExpiry:    144,
	}
	for _, wallet := range []*receiveDiscoveryWallet{
		originator, recipient,
	} {
		handler := vtxo.NewIncomingVTXOHandler(
			vtxo.IncomingVTXOHandlerConfig{
				ScriptStore: wallet,
				VTXOStore:   wallet,
			},
		)
		_, err := handler.Receive(ctx, vtxo.IncomingVTXOMsg{
			Event: event,
		}).Unpack()
		require.NoError(t, err)
	}
	require.Empty(t, originator.coins)
	require.Len(t, recipient.coins, 1)
	desc := recipient.coins[wire.OutPoint{Hash: claimHash}]
	require.NotNil(t, desc)
	require.EqualValues(t, 42_000, desc.Amount)
	require.Equal(t, vtxo.VTXOStatusLive, desc.Status)
	require.Equal(t, recipient.script, desc.PkScript)
	require.Equal(t, recipientKey.PubKey(), desc.ClientKey.PubKey)

	// Spend the discovered output using its materialized policy. The
	// recipient plus operator succeeds; substituting the originator fails.
	policy, err := arkscript.NewVTXOPolicy(
		desc.ClientKey.PubKey, desc.OperatorKey, desc.RelativeExpiry,
	)
	require.NoError(t, err)
	spendInfo, err := policy.CollabSpendInfo()
	require.NoError(t, err)
	spendTx := wire.NewMsgTx(2)
	spendTx.AddTxIn(wire.NewTxIn(&desc.Outpoint, nil, nil))
	spendTx.AddTxOut(wire.NewTxOut(41_000, originator.script))
	fetcher := txscript.NewCannedPrevOutputFetcher(
		desc.PkScript, int64(desc.Amount),
	)
	hashes := txscript.NewTxSigHashes(spendTx, fetcher)
	sign := func(key *btcec.PrivateKey) *schnorr.Signature {
		raw, err := txscript.RawTxInTapscriptSignature(
			spendTx, hashes, 0, int64(desc.Amount), desc.PkScript,
			txscript.NewBaseTapLeaf(spendInfo.WitnessScript),
			txscript.SigHashDefault, key,
		)
		require.NoError(t, err)
		sig, err := schnorr.ParseSignature(raw)
		require.NoError(t, err)

		return sig
	}
	operatorSig := sign(operator)
	verify := func(key *btcec.PrivateKey) error {
		witness, err := spendInfo.CollabWitness(sign(key), operatorSig)
		require.NoError(t, err)
		spendTx.TxIn[0].Witness = witness
		engine, err := txscript.NewEngine(
			desc.PkScript, spendTx, 0, txscript.StandardVerifyFlags,
			nil, hashes, int64(desc.Amount), fetcher,
		)
		require.NoError(t, err)

		return engine.Execute()
	}
	require.NoError(t, verify(recipientKey))
	require.Error(t, verify(originatorKey))
}
