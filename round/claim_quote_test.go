package round

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestEvaluateClaimOnlyQuote validates multiple claim outputs without relying
// on the ordinary single-output implicit-change rule.
func TestEvaluateClaimOnlyQuote(t *testing.T) {
	t.Parallel()

	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	policy, err := arkscript.EncodeStandardVTXOTemplate(
		owner.PubKey(), operator.PubKey(), 144,
	)
	require.NoError(t, err)

	intents := Intents{}
	quote := &ClientQuote{}
	for i := 0; i < 2; i++ {
		signingKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		expected := types.VTXORequest{
			Amount:         btcutil.Amount(50_000 + i),
			FixedAmount:    true,
			PolicyTemplate: bytes.Clone(policy),
			OwnerKey: keychain.KeyDescriptor{
				PubKey: owner.PubKey(),
			},
			SigningKey: keychain.KeyDescriptor{
				PubKey: signingKey.PubKey(),
			},
			Origin: types.VTXOOriginRoundRefresh,
		}
		pkScript, err := expected.EffectivePkScript()
		require.NoError(t, err)
		source := wire.OutPoint{
			Hash: chainhash.Hash{
				byte(i + 1),
			},
			Index: uint32(i),
		}
		claim := VTXOClaimIntent{
			Input: types.VTXOClaimInput{
				SourceOutpoint:    source,
				ParticipantPubKey: owner.PubKey(),
				ReplacementSigningKey: keychain.KeyDescriptor{
					PubKey: signingKey.PubKey(),
				},
			},
			ExpectedOutput: expected,
		}
		intents.Claims = append(intents.Claims, claim)
		intents.VTXOs = append(intents.VTXOs, expected)
		quote.ClaimQuotes = append(
			quote.ClaimQuotes, VTXOClaimQuoteEntry{
				SourceOutpoint: source,
				PkScript:       pkScript,
				PolicyTemplate: bytes.Clone(
					expected.PolicyTemplate,
				),
				AmountSat: int64(expected.Amount),
				ReplacementSigningKey: signingKey.PubKey().
					SerializeCompressed(),
			},
		)
	}

	event := evaluateQuote(
		t.Context(), &ClientEnvironment{}, RoundID{}, intents, quote,
	)
	require.IsType(t, &QuoteAccepted{}, event)

	tests := []struct {
		name   string
		mutate func(*ClientQuote)
		want   string
	}{
		{
			name: "source", want: "source outpoint echo mismatch",
			mutate: func(q *ClientQuote) {
				q.ClaimQuotes[0].SourceOutpoint.Index++
			},
		},
		{
			name: "script", want: "pkScript echo mismatch",
			mutate: func(q *ClientQuote) {
				q.ClaimQuotes[0].PkScript[0] ^= 1
			},
		},
		{
			name: "policy", want: "policy template echo mismatch",
			mutate: func(q *ClientQuote) {
				q.ClaimQuotes[0].PolicyTemplate[0] ^= 1
			},
		},
		{
			name: "amount", want: "amount",
			mutate: func(q *ClientQuote) {
				q.ClaimQuotes[0].AmountSat++
			},
		},
		{
			name: "replacement key",
			want: "replacement signing key echo mismatch",
			mutate: func(q *ClientQuote) {
				q.ClaimQuotes[0].ReplacementSigningKey[0] ^= 1
			},
		},
		{
			name: "fee", want: "must have zero operator fee",
			mutate: func(q *ClientQuote) {
				q.OperatorFeeSat = 1
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			cloned := *quote
			cloned.ClaimQuotes = append(
				[]VTXOClaimQuoteEntry(nil),
				quote.ClaimQuotes...,
			)
			for i := range cloned.ClaimQuotes {
				dst := &cloned.ClaimQuotes[i]
				src := quote.ClaimQuotes[i]
				replacementKey := src.ReplacementSigningKey
				dst.PkScript = bytes.Clone(
					src.PkScript,
				)
				dst.PolicyTemplate =
					bytes.Clone(
						src.PolicyTemplate,
					)
				dst.ReplacementSigningKey =
					bytes.Clone(
						replacementKey,
					)
			}
			test.mutate(&cloned)

			event := evaluateQuote(
				t.Context(), &ClientEnvironment{}, RoundID{},
				intents, &cloned,
			)
			rejected, ok := event.(*QuoteRejected)
			require.True(t, ok)
			require.Contains(t, rejected.Reason, test.want)
		})
	}
}
