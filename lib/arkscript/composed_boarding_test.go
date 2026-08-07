package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/stretchr/testify/require"
)

// composedBoardingFixture builds a boarding policy template plus the
// commitment leaf hash a boarded asset output would disclose.
func composedBoardingFixture(t *testing.T) ([]byte, [32]byte, *btcec.PublicKey,
	*btcec.PublicKey, uint32) {

	t.Helper()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	const exitDelay = 512
	template, err := EncodeStandardVTXOTemplate(
		ownerPriv.PubKey(), operatorPriv.PubKey(), exitDelay,
	)
	require.NoError(t, err)

	leafHash := [32]byte(chainhash.HashH([]byte("asset-commitment-leaf")))

	return template, leafHash, ownerPriv.PubKey(), operatorPriv.PubKey(),
		exitDelay
}

// TestComposedBoardingAddressMatchesScript proves the address a boarder
// derives pays to the same script the operator recomputes from the
// disclosure alone. A mismatch here is what makes a boarding output
// unspendable by the round.
func TestComposedBoardingAddressMatchesScript(t *testing.T) {
	t.Parallel()

	template, leafHash, owner, operator, exitDelay :=
		composedBoardingFixture(t)

	want, _, _, err := ComposedBoardingScript(template, leafHash)
	require.NoError(t, err)

	address, tapscript, err := ComposedBoardingAddress(
		template, leafHash, owner, operator, exitDelay,
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	got, err := txscript.PayToAddrScript(address)
	require.NoError(t, err)
	require.Equal(t, want, got, "composed address script")
	require.NotNil(t, tapscript.ControlBlock)
}

// TestComposedBoardingSpendPathsCommitToTheOutput proves both spend paths
// of a composed boarding output rebuild that output's key: the
// collaborative leaf the round's commitment transaction spends, and the
// timeout leaf join-round authorization proves ownership over. Either one
// deriving a different key is rejected as an invalid witness.
func TestComposedBoardingSpendPathsCommitToTheOutput(t *testing.T) {
	t.Parallel()

	template, leafHash, owner, operator, exitDelay :=
		composedBoardingFixture(t)

	pkScript, _, _, err := ComposedBoardingScript(template, leafHash)
	require.NoError(t, err)
	wantKey := pkScript[2:]

	_, tapscript, err := ComposedBoardingAddress(
		template, leafHash, owner, operator, exitDelay,
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	collabKey := txscript.ComputeTaprootOutputKey(
		tapscript.ControlBlock.InternalKey,
		tapscript.ControlBlock.RootHash(tapscript.Leaves[0].Script),
	)
	require.Equal(
		t, wantKey, schnorr.SerializePubKey(collabKey),
		"collaborative leaf must rebuild the composed output key",
	)

	authSpend, err := ComposedBoardingAuthSpend(
		leafHash, owner, operator, exitDelay,
	)
	require.NoError(t, err)

	authBlock, err := txscript.ParseControlBlock(authSpend.ControlBlock)
	require.NoError(t, err)
	authKey := txscript.ComputeTaprootOutputKey(
		authBlock.InternalKey,
		authBlock.RootHash(authSpend.WitnessScript),
	)
	require.Equal(
		t, wantKey, schnorr.SerializePubKey(authKey),
		"timeout leaf must rebuild the composed output key",
	)

	// Parity must follow the composed key, not the policy-only key: the
	// script engine reconstructs the output key from the control block's
	// parity bit and rejects the spend when it is wrong.
	require.Equal(
		t, collabKey.SerializeCompressed()[0] == 0x03,
		authBlock.OutputKeyYIsOdd,
	)
}
