package db

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestSerializeVTXODescriptor tests round-trip serialization of
// VTXODescriptor.
func TestSerializeVTXODescriptor(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	desc := &tree.VTXODescriptor{
		Amount:      btcutil.Amount(100000),
		PkScript:    []byte{0x51, 0x20, 0x12, 0x34},
		CoSignerKey: clientKey.PubKey(),
	}

	// Serialize.
	data, err := SerializeVTXODescriptor(desc)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Deserialize.
	decoded, err := DeserializeVTXODescriptor(data)
	require.NoError(t, err)

	// Verify fields match.
	require.Equal(t, desc.Amount, decoded.Amount)
	require.Equal(t, desc.PkScript, decoded.PkScript)
	require.True(t, desc.CoSignerKey.IsEqual(decoded.CoSignerKey))
}

// TestSerializeBoardingInput tests round-trip serialization of BoardingInput.
func TestSerializeBoardingInput(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	input := &rounds.BoardingInput{
		Outpoint: &wire.OutPoint{
			Hash:  chainhash.Hash{0x01, 0x02, 0x03},
			Index: 1,
		},
		Tapscript: nil, // Keep nil for simplicity
		Value:     btcutil.Amount(50000),
		PkScript:  []byte{0x00, 0x14, 0xab, 0xcd},
		ClientKey: clientKey.PubKey(),
		OperatorKeyDesc: &keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: 10,
				Index:  5,
			},
		},
	}

	// Serialize.
	data, err := serializeBoardingInput(input)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Deserialize.
	decoded, err := deserializeBoardingInput(data)
	require.NoError(t, err)

	// Verify fields match.
	require.Equal(t, input.Outpoint.Hash, decoded.Outpoint.Hash)
	require.Equal(t, input.Outpoint.Index, decoded.Outpoint.Index)
	require.Equal(t, input.Value, decoded.Value)
	require.Equal(t, input.PkScript, decoded.PkScript)
	require.True(t, input.ClientKey.IsEqual(decoded.ClientKey))
	require.True(
		t, input.OperatorKeyDesc.PubKey.IsEqual(
			decoded.OperatorKeyDesc.PubKey,
		),
	)
	require.Equal(
		t, input.OperatorKeyDesc.KeyLocator,
		decoded.OperatorKeyDesc.KeyLocator,
	)
}

// TestSerializeTapscript tests round-trip serialization of Tapscript.
func TestSerializeTapscript(t *testing.T) {
	t.Parallel()

	// Create a test control block.
	internalKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a valid inclusion proof (must be multiple of 32 bytes).
	inclusionProof := make([]byte, 32)
	for i := range inclusionProof {
		inclusionProof[i] = byte(i)
	}

	controlBlock := &txscript.ControlBlock{
		InternalKey:     internalKey.PubKey(),
		OutputKeyYIsOdd: true,
		LeafVersion:     txscript.BaseLeafVersion,
		InclusionProof:  inclusionProof,
	}

	// Create test tap leaves.
	leaves := []txscript.TapLeaf{
		{
			LeafVersion: txscript.BaseLeafVersion,
			Script:      []byte{0x51}, // OP_TRUE
		},
		{
			LeafVersion: txscript.BaseLeafVersion,
			Script:      []byte{0x00, 0x14, 0xab, 0xcd},
		},
	}

	tapscript := &waddrmgr.Tapscript{
		Type:           1,
		ControlBlock:   controlBlock,
		Leaves:         leaves,
		RevealedScript: []byte{0x51},
		RootHash: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
			0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
			0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20},
	}

	// Serialize.
	data, err := serializeTapscript(tapscript)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Deserialize.
	decoded, err := deserializeTapscript(data)
	require.NoError(t, err)

	// Verify fields match.
	require.Equal(t, tapscript.Type, decoded.Type)
	require.NotNil(t, decoded.ControlBlock)

	// Verify control block round-trips correctly by comparing serialized
	// bytes.
	origBytes, err := tapscript.ControlBlock.ToBytes()
	require.NoError(t, err)
	decodedBytes, err := decoded.ControlBlock.ToBytes()
	require.NoError(t, err)
	require.Equal(t, origBytes, decodedBytes)

	require.Len(t, decoded.Leaves, 2)
	require.Equal(t, leaves[0].Script, decoded.Leaves[0].Script)
	require.Equal(t, leaves[1].Script, decoded.Leaves[1].Script)
	require.Equal(t, tapscript.RevealedScript, decoded.RevealedScript)
	require.Equal(t, tapscript.RootHash, decoded.RootHash)
}

// TestSerializeTapscriptNil tests serialization of nil Tapscript.
func TestSerializeTapscriptNil(t *testing.T) {
	t.Parallel()

	// Serialize nil tapscript.
	data, err := serializeTapscript(nil)
	require.NoError(t, err)
	require.Empty(t, data)

	// Deserialize empty data.
	decoded, err := deserializeTapscript(data)
	require.NoError(t, err)
	require.Nil(t, decoded)
}

// TestSerializeClientRegistration tests round-trip serialization of
// ClientRegistration.
func TestSerializeClientRegistration(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Create a boarding input.
	boardingInput := &rounds.BoardingInput{
		Outpoint: &wire.OutPoint{
			Hash:  chainhash.Hash{0x01},
			Index: 0,
		},
		Tapscript: nil,
		Value:     btcutil.Amount(100000),
		PkScript:  []byte{0x51, 0x20},
		ClientKey: clientKey.PubKey(),
		OperatorKeyDesc: &keychain.KeyDescriptor{
			PubKey: operatorKey.PubKey(),
		},
	}

	// Create leave outputs.
	leaveOutput := &wire.TxOut{
		Value:    50000,
		PkScript: []byte{0x00, 0x14, 0x01, 0x02},
	}

	// Create VTXO descriptor.
	vtxoKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	vtxoDescriptor := &tree.VTXODescriptor{
		Amount:      btcutil.Amount(40000),
		PkScript:    []byte{0x51, 0x20, 0x03, 0x04},
		CoSignerKey: vtxoKey.PubKey(),
	}

	reg := &rounds.ClientRegistration{
		ClientID:       clientconn.ClientID("test-client"),
		BoardingInputs: []*rounds.BoardingInput{boardingInput},
		LeaveOutputs:   []*wire.TxOut{leaveOutput},
		VTXODescriptors: map[rounds.SigningKeyHex]*tree.VTXODescriptor{
			{0x01}: vtxoDescriptor,
		},
		ForfeitInputs: []*rounds.ForfeitInput{},
	}

	// Serialize.
	data, err := SerializeClientRegistration(reg)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Deserialize.
	decoded, err := DeserializeClientRegistration(data)
	require.NoError(t, err)

	// Verify fields match.
	require.Equal(t, reg.ClientID, decoded.ClientID)
	require.Len(t, decoded.BoardingInputs, 1)
	require.Equal(
		t, boardingInput.Outpoint.Hash,
		decoded.BoardingInputs[0].Outpoint.Hash,
	)
	require.Equal(t,
		boardingInput.Value, decoded.BoardingInputs[0].Value,
	)
	require.Len(t, decoded.LeaveOutputs, 1)
	require.Equal(t, leaveOutput.Value, decoded.LeaveOutputs[0].Value)
	require.Len(t, decoded.VTXODescriptors, 1)
	require.Contains(t, decoded.VTXODescriptors, rounds.SigningKeyHex{0x01})
}

// TestSerializeClientRegistrationEmpty tests serialization with empty
// collections.
func TestSerializeClientRegistrationEmpty(t *testing.T) {
	t.Parallel()

	reg := &rounds.ClientRegistration{
		ClientID:       clientconn.ClientID("empty-client"),
		BoardingInputs: []*rounds.BoardingInput{},
		LeaveOutputs:   []*wire.TxOut{},
		VTXODescriptors: make(
			map[rounds.SigningKeyHex]*tree.VTXODescriptor,
		),
		ForfeitInputs: []*rounds.ForfeitInput{},
	}

	data, err := SerializeClientRegistration(reg)
	require.NoError(t, err)

	decoded, err := DeserializeClientRegistration(data)
	require.NoError(t, err)

	require.Equal(t, reg.ClientID, decoded.ClientID)
	require.Len(t, decoded.BoardingInputs, 0)
	require.Len(t, decoded.LeaveOutputs, 0)
	require.Len(t, decoded.VTXODescriptors, 0)
	require.Len(t, decoded.ForfeitInputs, 0)
}

// TestSerializeClientRegistrationWithForfeitInputs tests serialization with
// forfeit inputs.
func TestSerializeClientRegistrationWithForfeitInputs(t *testing.T) {
	t.Parallel()

	roundID := testRoundID("forfeit-test")
	vtxo := createTestVTXO(t, roundID, 0)

	forfeitInput := &rounds.ForfeitInput{
		Outpoint: &wire.OutPoint{
			Hash:  chainhash.Hash{0x02},
			Index: 1,
		},
		VTXO: vtxo,
	}

	reg := &rounds.ClientRegistration{
		ClientID:       clientconn.ClientID("forfeit-client"),
		BoardingInputs: []*rounds.BoardingInput{},
		LeaveOutputs:   []*wire.TxOut{},
		VTXODescriptors: make(
			map[rounds.SigningKeyHex]*tree.VTXODescriptor,
		),
		ForfeitInputs: []*rounds.ForfeitInput{forfeitInput},
	}

	data, err := SerializeClientRegistration(reg)
	require.NoError(t, err)

	decoded, err := DeserializeClientRegistration(data)
	require.NoError(t, err)

	require.Len(t, decoded.ForfeitInputs, 1)
	require.Equal(t,
		forfeitInput.Outpoint.Hash,
		decoded.ForfeitInputs[0].Outpoint.Hash,
	)
	require.Equal(t, vtxo.RoundID, decoded.ForfeitInputs[0].VTXO.RoundID)
	require.Equal(
		t, vtxo.BatchOutputIndex,
		decoded.ForfeitInputs[0].VTXO.BatchOutputIndex,
	)
}

// TestDeserializeClientRegistrationUnsupportedVersion ensures unsupported
// versions are rejected.
func TestDeserializeClientRegistrationUnsupportedVersion(t *testing.T) {
	t.Parallel()

	reg := &rounds.ClientRegistration{
		ClientID:       clientconn.ClientID("versioned-client"),
		BoardingInputs: []*rounds.BoardingInput{},
		LeaveOutputs:   []*wire.TxOut{},
		VTXODescriptors: make(
			map[rounds.SigningKeyHex]*tree.VTXODescriptor,
		),
		ForfeitInputs: []*rounds.ForfeitInput{},
	}

	data, err := SerializeClientRegistration(reg)
	require.NoError(t, err)

	var (
		regVersion          uint16
		clientIDBytes       []byte
		boardingInputsBytes []byte
		leaveOutputsBytes   []byte
		vtxoDescsBytes      []byte
		forfeitInputsBytes  []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(clientRegVersionType, &regVersion),
		tlv.MakeDynamicRecord(clientRegClientIDType, &clientIDBytes,
			nil, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegBoardingInputsType,
			&boardingInputsBytes, nil, tlv.EVarBytes,
			tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegLeaveOutputsType,
			&leaveOutputsBytes, nil, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegVTXODescriptorsType,
			&vtxoDescsBytes, nil, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegForfeitInputsType,
			&forfeitInputsBytes, nil, tlv.EVarBytes, tlv.DVarBytes),
	)
	require.NoError(t, err)

	err = tlvStream.Decode(bytes.NewReader(data))
	require.NoError(t, err)

	regVersion = 2

	var buf bytes.Buffer
	tlvStream, err = tlv.NewStream(
		tlv.MakePrimitiveRecord(clientRegVersionType, &regVersion),
		tlv.MakeDynamicRecord(clientRegClientIDType, &clientIDBytes,
			nil, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegBoardingInputsType,
			&boardingInputsBytes, nil, tlv.EVarBytes,
			tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegLeaveOutputsType,
			&leaveOutputsBytes, nil, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegVTXODescriptorsType,
			&vtxoDescsBytes, nil, tlv.EVarBytes, tlv.DVarBytes),
		tlv.MakeDynamicRecord(clientRegForfeitInputsType,
			&forfeitInputsBytes, nil, tlv.EVarBytes, tlv.DVarBytes),
	)
	require.NoError(t, err)

	err = tlvStream.Encode(&buf)
	require.NoError(t, err)

	_, err = DeserializeClientRegistration(buf.Bytes())
	require.Error(t, err)
}

// TestCodecErrors tests error cases for codec functions.
func TestCodecErrors(t *testing.T) {
	t.Parallel()

	t.Run("nil VTXODescriptor", func(t *testing.T) {
		_, err := SerializeVTXODescriptor(nil)
		require.Error(t, err)
	})

	t.Run("nil ClientRegistration", func(t *testing.T) {
		_, err := SerializeClientRegistration(nil)
		require.Error(t, err)
	})

	t.Run("invalid VTXODescriptor data", func(t *testing.T) {
		_, err := DeserializeVTXODescriptor([]byte{0xff, 0xff})
		require.Error(t, err)
	})

	t.Run("invalid ClientRegistration data", func(t *testing.T) {
		_, err := DeserializeClientRegistration([]byte{0xff, 0xff})
		require.Error(t, err)
	})
}
