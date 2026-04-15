package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// testVTXORow builds a minimal VTXORow with the given policy and script.
func testVTXORow(t *testing.T, policyTemplate []byte,
	pkScript []byte) VTXORow {

	t.Helper()

	return VTXORow{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 0,
		},
		PkScript:       append([]byte(nil), pkScript...),
		PolicyTemplate: append([]byte(nil), policyTemplate...),
		Status:         storeVTXOStatusLive,
	}
}

// TestAuthorizePolicySignerByRowsStandardVTXO ensures that only the non-
// operator owner key is authorized for standard VTXO queries.
func TestAuthorizePolicySignerByRowsStandardVTXO(t *testing.T) {
	t.Parallel()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	template, err := arkscript.StandardVTXOTemplate(
		ownerPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	policyBytes, err := template.Encode()
	require.NoError(t, err)

	pkScript, err := template.PkScript()
	require.NoError(t, err)

	rows := []VTXORow{testVTXORow(t, policyBytes, pkScript)}
	scopedSignerKeys := map[string]*btcec.PublicKey{
		hex.EncodeToString(pkScript): ownerPriv.PubKey(),
	}

	err = authorizePolicySignerByRows(
		scopedSignerKeys, rows, operatorPriv.PubKey(),
	)
	require.NoError(t, err)

	scopedSignerKeys[hex.EncodeToString(pkScript)] = operatorPriv.PubKey()
	err = authorizePolicySignerByRows(
		scopedSignerKeys, rows, operatorPriv.PubKey(),
	)
	require.ErrorContains(t, err, "not authorized")
}

// TestAuthorizePolicySignerByRowsVHTLC ensures that sender and receiver can
// query a vHTLC, while the operator cannot.
func TestAuthorizePolicySignerByRowsVHTLC(t *testing.T) {
	t.Parallel()

	senderPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	receiverPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	preimageHash := sha256.Sum256([]byte("policy-auth-vhtlc"))
	paymentHash := lntypes.Hash(preimageHash)

	policy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               senderPriv.PubKey(),
		Receiver:                             receiverPriv.PubKey(),
		Server:                               operatorPriv.PubKey(),
		PreimageHash:                         paymentHash,
		RefundLocktime:                       1_000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                144,
		UnilateralRefundWithoutReceiverDelay: 144,
	})
	require.NoError(t, err)

	policyBytes, err := policy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := policy.PkScript()
	require.NoError(t, err)

	rows := []VTXORow{testVTXORow(t, policyBytes, pkScript)}

	err = authorizePolicySignerByRows(
		map[string]*btcec.PublicKey{
			hex.EncodeToString(pkScript): senderPriv.PubKey(),
		},
		rows, operatorPriv.PubKey(),
	)
	require.NoError(t, err)

	err = authorizePolicySignerByRows(
		map[string]*btcec.PublicKey{
			hex.EncodeToString(pkScript): receiverPriv.PubKey(),
		},
		rows, operatorPriv.PubKey(),
	)
	require.NoError(t, err)

	err = authorizePolicySignerByRows(
		map[string]*btcec.PublicKey{
			hex.EncodeToString(pkScript): operatorPriv.PubKey(),
		},
		rows, operatorPriv.PubKey(),
	)
	require.ErrorContains(t, err, "not authorized")
}

// TestParticipantKeysFromRowExcludesNonSettlementPair asserts that a
// non-operator key appearing only in a leaf without an operator-backed
// sibling is NOT authorized. This closes the over-authorization surface
// where an OOR sender could poison a recipient's stored policy
// template with an arbitrary watcher key and then prove control over
// that key to query the recipient's VTXO metadata.
//
// The test constructs a policy whose leaves are:
//   - collab:    Multisig{owner, operator}   (has settlement pair)
//   - owner exit: CSV{Multisig{owner}}       (owner: has op-paired sibling)
//   - stalker:   CSV{Multisig{stalker}}      (no operator-backed sibling)
//
// Before the settlement-pair filter, `template.ParticipantKeys()`
// admitted `stalker` because it merely enumerated every non-operator
// key across every leaf.
func TestParticipantKeysFromRowExcludesNonSettlementPair(t *testing.T) {
	t.Parallel()

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	stalkerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ownerKey := ownerPriv.PubKey()
	operatorKey := operatorPriv.PubKey()
	stalkerKey := stalkerPriv.PubKey()

	template := &arkscript.PolicyTemplate{
		Leaves: []arkscript.LeafTemplate{
			{Node: &arkscript.Multisig{Keys: []*btcec.PublicKey{
				ownerKey, operatorKey,
			}}},
			{Node: &arkscript.CSV{
				Lock: 144,
				Inner: &arkscript.Multisig{
					Keys: []*btcec.PublicKey{ownerKey},
				},
			}},
			{Node: &arkscript.CSV{
				Lock: 144,
				Inner: &arkscript.Multisig{
					Keys: []*btcec.PublicKey{stalkerKey},
				},
			}},
		},
	}

	policyBytes, err := template.Encode()
	require.NoError(t, err)

	pkScript, err := template.PkScript()
	require.NoError(t, err)

	rows := []VTXORow{testVTXORow(t, policyBytes, pkScript)}

	// Owner is a full settlement participant -> authorized.
	err = authorizePolicySignerByRows(
		map[string]*btcec.PublicKey{
			hex.EncodeToString(pkScript): ownerKey,
		},
		rows, operatorKey,
	)
	require.NoError(t, err)

	// Stalker appears in the template's participant set but has no
	// operator-backed sibling -> must NOT authorize. The old code
	// (ParticipantKeys-only) accepted this.
	err = authorizePolicySignerByRows(
		map[string]*btcec.PublicKey{
			hex.EncodeToString(pkScript): stalkerKey,
		},
		rows, operatorKey,
	)
	require.ErrorContains(t, err, "not authorized")
}

// TestAuthorizePolicySignerByRowsNoRows permits empty query results after the
// registration authorizer has already accepted the request.
func TestAuthorizePolicySignerByRowsNoRows(t *testing.T) {
	t.Parallel()

	signerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pkScript, err := txscript.PayToTaprootScript(signerPriv.PubKey())
	require.NoError(t, err)

	err = authorizePolicySignerByRows(
		map[string]*btcec.PublicKey{
			hex.EncodeToString(pkScript): signerPriv.PubKey(),
		},
		nil, nil,
	)
	require.NoError(t, err)
}
