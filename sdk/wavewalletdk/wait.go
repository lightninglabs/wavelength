package wavewalletdk

// closedWaitChan gives nil clients the same non-blocking Wait behavior as
// handles without an owned runtime.
func closedWaitChan() <-chan error {
	ch := make(chan error)
	close(ch)

	return ch
}
