package scripts

import (
	"encoding/hex"

	"github.com/btcsuite/btcd/btcec/v2"
)

const (
	// ARKNUMSHex is the hex encoded version of the ARK NUMs key.
	ARKNUMSHex = "02372f225b3caee8213096de3229ee4335306b0" +
		"7c3c169438461b5d4749884ec65"

	// ARKNUMSSeedPhrase is the seed phrase used to generate
	// the ARK NUMS key.
	ARKNUMSSeedPhrase = "Ark Protocol NUMS"
)

var (
	// ARKNUMSKey is a NUMS key (nothing up my sleeves number) that has
	// no known private key. This was generated using the following script:
	// https://github.com/lightninglabs/lightning-node-connect/tree/
	// master/mailbox/numsgen, with the seed phrase "Ark Protocol NUMS".
	ARKNUMSKey = mustParsePubKey(ARKNUMSHex)
)

// mustParsePubKey parses a hex encoded public key string into a public key and
// panic if parsing fails.
func mustParsePubKey(pubStr string) btcec.PublicKey {
	pubBytes, err := hex.DecodeString(pubStr)
	if err != nil {
		panic(err)
	}

	pub, err := btcec.ParsePubKey(pubBytes)
	if err != nil {
		panic(err)
	}

	return *pub
}
