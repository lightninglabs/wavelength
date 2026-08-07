package tapassets

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

// ComposedBoardingScript recomputes a boarded asset output's script from
// hashes alone. See arkscript.ComposedBoardingScript; the owner deriving
// the address and the operator validating the disclosure must agree
// exactly, so both reach the same derivation through this one path.
func ComposedBoardingScript(policyTemplate []byte,
	commitmentLeafHash [32]byte) ([]byte, *btcec.PublicKey, [32]byte,
	error) {

	return arkscript.ComposedBoardingScript(
		policyTemplate, commitmentLeafHash,
	)
}
