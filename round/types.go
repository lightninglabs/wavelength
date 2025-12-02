package round

import (
	"github.com/lightninglabs/darepo-client/wallet"
)

// BoardingAddress is a type alias for wallet.BoardingAddress. The wallet
// package owns the canonical definition of boarding addresses including the
// tapscript, keys, and exit delay. The round package uses this type directly
// without extension.
type BoardingAddress = wallet.BoardingAddress
