package darepod

import (
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/stretchr/testify/require"
)

// TestFilterBoardingScriptsDropsBoardingUTXOs verifies that any UTXO
// whose pkScript matches a known boarding script is removed from the
// view. This is the load-bearing case: lwwallet's btcwallet credit
// pipeline can briefly continue to credit a spent boarding output, so
// the preflight needs the filter to avoid counting it as a fee input.
func TestFilterBoardingScriptsDropsBoardingUTXOs(t *testing.T) {
	t.Parallel()

	boardingScript := []byte{0x51, 0x20, 0xaa}
	walletScript := []byte{0x51, 0x20, 0xbb}

	utxos := []*wallet.Utxo{
		{
			Outpoint: wire.OutPoint{
				Index: 0,
			},
			PkScript: boardingScript,
		},
		{
			Outpoint: wire.OutPoint{
				Index: 1,
			},
			PkScript: walletScript,
		},
	}

	boardingSet := map[string]struct{}{
		string(boardingScript): {},
	}

	filtered := filterBoardingScripts(utxos, boardingSet)
	require.Len(t, filtered, 1)
	require.Equal(t, uint32(1), filtered[0].Outpoint.Index)
}

// TestFilterBoardingScriptsEmptySetIsNoOp verifies that an empty
// boarding-script set returns the input slice untouched. Avoids
// unnecessary allocation on the common no-boarding-addresses path.
func TestFilterBoardingScriptsEmptySetIsNoOp(t *testing.T) {
	t.Parallel()

	utxos := []*wallet.Utxo{
		{
			Outpoint: wire.OutPoint{
				Index: 0,
			},
			PkScript: []byte{
				0x51,
			},
		},
	}

	filtered := filterBoardingScripts(utxos, nil)
	require.Equal(t, utxos, filtered)

	filtered = filterBoardingScripts(utxos, map[string]struct{}{})
	require.Equal(t, utxos, filtered)
}

// TestFilterBoardingScriptsAllBoardingReturnsEmpty verifies the edge
// case where every UTXO is a boarding output — the result is an
// empty, non-nil slice so callers can iterate without nil checks.
func TestFilterBoardingScriptsAllBoardingReturnsEmpty(t *testing.T) {
	t.Parallel()

	boardingScript := []byte{0x51, 0x20, 0xcc}
	utxos := []*wallet.Utxo{
		{
			PkScript: boardingScript,
		},
		{
			PkScript: boardingScript,
		},
	}

	filtered := filterBoardingScripts(utxos, map[string]struct{}{
		string(boardingScript): {},
	})
	require.Empty(t, filtered)
	require.NotNil(t, filtered)
}

// TestFilterBoardingScriptsSkipsNilEntries verifies that a nil UTXO
// in the input is dropped rather than panicking the filter. The
// backing-wallet conversion currently never emits nils, but the
// defensive skip keeps the helper robust if a backend ever does.
func TestFilterBoardingScriptsSkipsNilEntries(t *testing.T) {
	t.Parallel()

	utxos := []*wallet.Utxo{
		nil,
		{
			PkScript: []byte{
				0x51,
				0x20,
				0xdd,
			},
		},
	}

	filtered := filterBoardingScripts(utxos, map[string]struct{}{
		string([]byte{0xff}): {},
	})
	require.Len(t, filtered, 1)
	require.Equal(t, []byte{0x51, 0x20, 0xdd}, filtered[0].PkScript)
}
