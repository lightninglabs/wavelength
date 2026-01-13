package db

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// testRoundID creates a deterministic RoundID from a string seed for tests.
func testRoundID(seed string) rounds.RoundID {
	h := chainhash.HashH([]byte(seed))
	id, _ := uuid.FromBytes(h[:16])

	return rounds.RoundID(id)
}

// testOutpointHash creates a deterministic hash from a string seed for tests.
func testOutpointHash(t *testing.T, seed string) chainhash.Hash {
	t.Helper()

	return chainhash.HashH([]byte(seed))
}

// createTestFinalTx creates a simple test commitment transaction.
// The seed parameter is used to create unique transactions.
//
//nolint:unused
func createTestFinalTx(t *testing.T, seed string) *wire.MsgTx {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  testOutpointHash(t, seed),
			Index: 0,
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000000,
		PkScript: []byte{0x51, 0x20, 0xab, 0xcd},
	})

	return tx
}

// createTestVTXOTree creates a simple VTXO tree for testing.
//
//nolint:unused
func createTestVTXOTree(t *testing.T, batchOutput int) *tree.Tree {
	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash:  chainhash.Hash{byte(batchOutput)},
			Index: uint32(batchOutput),
		},
		BatchOutput: &wire.TxOut{
			Value:    1000000,
			PkScript: []byte{0x51, 0x20},
		},
		Root: &tree.Node{
			Input: wire.OutPoint{
				Hash:  chainhash.Hash{0x03},
				Index: 0,
			},
			Outputs: []*wire.TxOut{
				{Value: 500000, PkScript: []byte{0x00, 0x14}},
			},
			CoSigners: []*btcec.PublicKey{
				clientKey.PubKey(),
				operatorKey.PubKey(),
			},
			Children: make(map[uint32]*tree.Node),
		},
	}
}

// createTestConnectorDescriptor creates a test connector descriptor.
//
//nolint:unused
func createTestConnectorDescriptor(
	outputIdx int, numLeaves int) *rounds.ConnectorTreeDescriptor {

	return &rounds.ConnectorTreeDescriptor{
		OutputIndex:   outputIdx,
		NumLeaves:     numLeaves,
		ForfeitScript: []byte{0x51, 0x20, byte(outputIdx)},
	}
}

// createTestClientRegistration creates a test client registration.
//
//nolint:unused
func createTestClientRegistration(t *testing.T,
	clientID clientconn.ClientID) *rounds.ClientRegistration {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a boarding input without Tapscript (complex serialization).
	boardingInput := &rounds.BoardingInput{
		Outpoint: &wire.OutPoint{
			Hash:  testOutpointHash(t, "boarding"),
			Index: 0,
		},
		Tapscript: nil, // Skip Tapscript to avoid gob PublicKey issues
		Value:     btcutil.Amount(100000),
		PkScript:  []byte{0x51, 0x20},
		ClientKey: clientKey.PubKey(),
		OperatorKeyDesc: &keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	}

	// Create a leave output.
	leaveOutput := &wire.TxOut{
		Value:    50000,
		PkScript: []byte{0x00, 0x14, 0x01, 0x02},
	}

	// Create VTXO descriptors.
	vtxoKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	vtxoDescriptor := &tree.VTXODescriptor{
		Amount:      btcutil.Amount(40000),
		PkScript:    []byte{0x51, 0x20, 0x03, 0x04},
		CoSignerKey: vtxoKey.PubKey(),
	}

	return &rounds.ClientRegistration{
		ClientID:       clientID,
		BoardingInputs: []*rounds.BoardingInput{boardingInput},
		LeaveOutputs:   []*wire.TxOut{leaveOutput},
		VTXODescriptors: map[rounds.SigningKeyHex]*tree.VTXODescriptor{
			{0x01}: vtxoDescriptor,
		},
		ForfeitInputs: []*rounds.ForfeitInput{},
	}
}

// createTestRound creates a complete test round.
//
//nolint:unused
func createTestRound(t *testing.T, roundID rounds.RoundID) *rounds.Round {
	t.Helper()

	// Use roundID as seed to create unique FinalTx for each round.
	finalTx := createTestFinalTx(t, roundID.String())
	vtxoTree := createTestVTXOTree(t, 0)

	return &rounds.Round{
		RoundID: roundID,
		FinalTx: finalTx,
		VTXOTrees: map[int]*tree.Tree{
			0: vtxoTree,
		},
		ConnectorDescriptors: []*rounds.ConnectorTreeDescriptor{
			createTestConnectorDescriptor(1, 4),
		},
		ForfeitInfos: make(map[wire.OutPoint]*rounds.ForfeitInfo),
		ClientRegistrations: map[clientconn.ClientID]*rounds.
			ClientRegistration{
			"client1": createTestClientRegistration(t, "client1"),
		},
	}
}

// createTestVTXO creates a test VTXO.
func createTestVTXO(t *testing.T, roundID rounds.RoundID,
	idx int) *rounds.VTXO {

	t.Helper()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return &rounds.VTXO{
		Outpoint: wire.OutPoint{
			Hash:  testOutpointHash(t, "vtxo"),
			Index: uint32(idx),
		},
		RoundID:          roundID,
		BatchOutputIndex: 0,
		Descriptor: &tree.VTXODescriptor{
			Amount:      btcutil.Amount(100000 * (idx + 1)),
			PkScript:    []byte{0x51, 0x20, byte(idx)},
			CoSignerKey: clientKey.PubKey(),
		},
		Status: rounds.VTXOStatusPending,
	}
}
