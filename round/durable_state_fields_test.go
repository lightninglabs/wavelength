package round

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// requireDurableFixtureFields prevents nested protocol additions from silently
// passing round-trip tests with a zero value on both sides of the codec.
func requireDurableFixtureFields(t testing.TB, fixtures ...any) {
	t.Helper()
	for _, fixture := range fixtures {
		value := reflect.ValueOf(fixture).Elem()
		for i := 0; i < value.NumField(); i++ {
			require.False(
				t, value.Field(i).IsZero(),
				"missing fixture for %s.%s", value.Type(),
				value.Type().Field(i).Name,
			)
		}
	}
}

// durableStateFixtures fills every declared state field with nonzero data.
// A new field must receive a fixture and survive the real codec; this catches
// omissions when the protocol state evolves independently of its checkpoint.
func durableStateFixtures(t *testing.T) []ClientState {
	t.Helper()
	private, key := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{7}, 32))
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(
			validPSBTBytes(t),
		),
		false,
	)
	require.NoError(t, err)
	id := testRoundID("checkpoint-field-coverage")
	point := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	hash := chainhash.Hash{3}
	clock := time.Unix(1700000000, 0).UTC()
	wait := ScheduledBatchWait{
		Opens:       clock,
		Cutoff:      clock.Add(time.Minute),
		PlannedJoin: clock.Add(time.Second),
	}
	path := &tree.Tree{BatchOutpoint: point, BatchOutput: wire.NewTxOut(
		500, []byte{0x51},
	),
		SweepTapscriptRoot: bytes.Repeat([]byte{1},
			32),
		SweepKey:   key,
		SweepDelay: 144,
		Root: &tree.Node{Input: point,
			Amount: 450,
			Outputs: []*wire.TxOut{
				wire.NewTxOut(450, []byte{0x51}),
			},
			CoSigners: []*btcec.PublicKey{
				key,
			}, FinalKey: key, Children: map[uint32]*tree.Node{}}}
	intents := Intents{Leaves: []*types.LeaveRequest{{Output: wire.NewTxOut(
		37, []byte{0x51},
	),
		IsChange:             true,
		DestinationOwnWallet: true}},
		QuotedLeaveAmounts: []int64{
			31,
		}}
	quote := &ClientQuote{QuoteID: [32]byte{
		1,
	}, SealPass: 2, OperatorFeeSat: 3,
		QuoteExpiresAt: clock.Unix(),
		RejectReason:   roundpb.QuoteReason_QUOTE_OK,
		VTXOQuotes: []VTXOQuoteEntry{
			{
				PkScript: []byte{
					0x51,
				},
				AmountSat:    29,
				RecipientKey: key.SerializeCompressed(),
			},
		},
		LeaveQuotes: []LeaveQuoteEntry{
			{
				PkScript: []byte{
					0x51,
				},
				AmountSat: 31,
			},
		}}
	_, sweepKey := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{13}, 32))
	result := &ClientVTXO{Outpoint: point,
		Amount: 43,
		PolicyTemplate: []byte{
			1,
		}, PkScript: []byte{
			0x51,
		},
		TaprootAssetRoot:   &hash,
		TaprootAssetRef:    "asset",
		TaprootAssetAmount: 47,
		TaprootAssetSealedPackage: []byte{
			5,
			6,
		}, Expiry: 144, OperatorKey: key,
		OwnerKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: 7,
				Index:  11,
			},
			PubKey: key,
		},
		Ancestry: []types.Ancestry{{
			TreePath:       path,
			CommitmentTxID: hash,
			InputIndices: []uint32{
				2,
				3,
			},
			TreeDepth:            2,
			CommitmentHeight:     97,
			CommitmentSweepDelay: 288,
			CommitmentSweepKey:   sweepKey,
		}},
		Origin:         types.VTXOOriginRoundBoarding,
		CommitmentTxID: hash,
		BatchExpiry:    101,
		CreatedHeight:  99,
		RoundID:        fn.Some(id)}

	// Cover nested result fields as well as the top-level state union. Keep
	// ancestry policy distinct from the tree cache so neither can mask loss
	// of the other during restoration.
	requireDurableFixtureFields(t, result, &result.Ancestry[0])

	_, operator := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{11}, 32))
	info, err := arkscript.NewVTXOSpendInfoFromPolicy(key, operator, 144, 0)
	require.NoError(t, err)
	auth := &arkscript.SpendPath{
		SpendInfo:        info,
		RequiredSequence: 1,
		RequiredLockTime: 2,
		Conditions: [][]byte{
			{
				3,
			},
		},
	}
	forfeit := &arkscript.SpendPath{
		SpendInfo:        info,
		RequiredSequence: 4,
		RequiredLockTime: 5,
		Conditions: [][]byte{
			{
				6,
			},
		},
	}
	intents.Boarding = []BoardingIntent{durableBoardingFixture(t)}
	intents.VTXOs = []types.VTXORequest{
		{
			Amount: 11,
			PolicyTemplate: []byte{
				1,
			},
			PkScript: []byte{
				0x51,
			},
		},
	}
	intents.Forfeits = []types.ForfeitRequest{
		{
			VTXOOutpoint: &point,
			Amount:       53,
			AuthSpend:    auth,
			ForfeitSpend: forfeit,
		},
	}
	var nonce tree.Musig2PubNonce
	copy(nonce[:33], key.SerializeCompressed())
	copy(nonce[33:], operator.SerializeCompressed())
	signature, err := schnorr.Sign(private, bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	signedForfeit := &ForfeitSignatureResponse{VTXOOutpoint: point,
		RoundID:   id.String(),
		ForfeitTx: packet.UnsignedTx,
		Signature: signature,
		SpendPath: forfeit,
		ParticipantVTXOSigs: []*types.ForfeitParticipantSig{
			{
				PubKey:    key,
				Signature: signature,
			},
		}}
	collected := map[wire.OutPoint]*ForfeitSignatureResponse{
		point: signedForfeit,
	}
	fields := map[string]any{
		"BatchWait": wait,
		"Boarding":  intents.Boarding,
		"VTXOs":     intents.VTXOs,
		"Leaves":    intents.Leaves, "Forfeits": intents.Forfeits,
		"RoundID":         id,
		"AdmittedRoundID": id,
		"Intents":         intents,
		"Quote":           quote,
		"CommitmentTx":    packet, "VTXOTreePaths": map[int]*tree.Tree{
			2: path,
		},
		"TxID": hash, "AssetLeafPackages": map[wire.OutPoint][]byte{
			point: {
				7,
			},
		},
		"TreeCosignKey":        key,
		"ConnectorOperatorKey": key,
		"SweepKey":             key,
		"SweepDelay":           uint32(144),
		"FlowVersion":          roundpb.FlowVersion(1),
		"ForfeitKey":           key,
		"ClientTrees": map[SignerKey]*tree.Tree{
			NewSignerKey(key): path,
		},
		"BoardingInputIndices": map[wire.OutPoint]int{
			point: 3,
		},
		"ForfeitMappings": map[wire.OutPoint]*ConnectorLeafInfo{
			point: {
				ConnectorOutpoint: point,
				ConnectorPkScript: []byte{
					0x51,
				},
				VTXOAmount: 61,
			},
		},
		"ExpectedForfeits": map[wire.OutPoint]*ConnectorLeafInfo{
			point: {
				ConnectorOutpoint: point,
				ConnectorPkScript: []byte{
					0x51,
				},
				VTXOAmount: 61,
			},
		},
		"CollectedForfeits": collected,
		"Musig2Sessions": map[SignerKey]*tree.SignerSession{
			NewSignerKey(key): {},
		},
		"AggNonces": map[tree.TxID]tree.Musig2PubNonce{
			hash: nonce,
		},
		"InputSigs": []*types.BoardingInputSignature{
			{
				InputIndex: 2,
				Outpoint:   point,
			},
		},
		"ForfeitedVTXOs": []wire.OutPoint{
			point,
		},
		"PendingFailure": &BoardingFailed{
			RoundID:     fn.Some(id),
			Reason:      "pending",
			Recoverable: true,
		},
		"ReconcileProbes": uint32(7),
		"BlockHeight":     int32(101),
		"BlockHash":       hash,
		"Confirmations":   int32(3),
		"Reason":          "failure",
		"Error":           fmt.Errorf("cause"),
		"Recoverable":     true,
		"FailureCode":     RoundFailureCode(1),
		"Outpoint":        point,
		"SweepTxID":       hash,
	}
	states := []ClientState{&Idle{},
		&PendingRoundAssembly{},
		&IntentSentState{},
		&QuoteReceivedState{},
		&RoundJoinedState{},
		&CommitmentTxReceivedState{},
		&CommitmentTxValidatedState{},
		&ForfeitSignaturesCollectingState{},
		&NoncesSentState{},
		&NoncesAggregatedState{},
		&PartialSigsSentState{}, &InputSigSentState{}, &ConfirmedState{
			VTXOs: []*ClientVTXO{
				result,
			},
		},
		&ClientFailedState{}, &RecoveryInitiatedState{}}
	for _, state := range states {
		value := reflect.ValueOf(state).Elem()
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if _, confirmed := state.(*ConfirmedState); confirmed &&
				field.Name == "VTXOs" {

				continue
			}
			fixture, ok := fields[field.Name]
			require.Truef(
				t, ok, "missing fixture for %T.%s", state,
				field.Name,
			)
			if fixture != nil {
				value.Field(i).Set(reflect.ValueOf(fixture))
			}
		}
	}

	return states
}

// TestDurableClientStateEveryField verifies the actual state union, including
// local ownership, selected schedule, asset results and process-only signers.
func TestDurableClientStateEveryField(t *testing.T) {
	for _, state := range durableStateFixtures(t) {
		t.Run(fmt.Sprintf("%T", state), func(t *testing.T) {
			raw, err := encodeClientState(state)
			require.NoError(t, err)
			restored, lost, err := decodeClientState(
				raw, &chaincfg.RegressionNetParams,
			)
			require.NoError(t, err)
			value := reflect.ValueOf(state).Elem()
			sessions := value.FieldByName("Musig2Sessions")
			require.Equal(t, sessions.IsValid(), lost)
			if sessions.IsValid() {
				sessions.SetZero()
			}
			require.Equal(t, state, restored)
			again, err := encodeClientState(restored)
			require.NoError(t, err)
			require.Equal(t, raw, again)
		})
	}
}

// TestDurableForfeitEmptyMap preserves the allocation required by the next
// forfeit response, whose transition clones and writes the collected map.
func TestDurableForfeitEmptyMap(t *testing.T) {
	original := &ForfeitSignaturesCollectingState{
		CollectedForfeits: make(
			map[wire.OutPoint]*ForfeitSignatureResponse,
		),
	}
	raw, err := encodeClientState(original)
	require.NoError(t, err)
	state, _, err := decodeClientState(raw, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	restored, ok := state.(*ForfeitSignaturesCollectingState)
	require.True(t, ok)
	require.NotNil(t, restored.CollectedForfeits)
	next := maps.Clone(restored.CollectedForfeits)
	next[wire.OutPoint{}] = &ForfeitSignatureResponse{}
	require.Empty(t, restored.CollectedForfeits)
}
