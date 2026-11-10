package round

import "github.com/btcsuite/btcd/wire/v2"

// releaseBoardingEffect delays the wallet's process-local in-flight release
// until the failed round's checkpoint has committed.
type releaseBoardingEffect struct {
	outpoints []wire.OutPoint
}

// clientOutMsgSealed permits capture alongside protocol-emitted effects.
func (*releaseBoardingEffect) clientOutMsgSealed() {}
