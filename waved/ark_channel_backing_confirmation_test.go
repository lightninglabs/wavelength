package waved

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/stretchr/testify/require"
)

// backingConfirmationBackend exposes only the local confirmation boundary.
type backingConfirmationBackend struct {
	chainsource.ChainBackend
	registered chan struct{}
	confirmed  chan *chainsource.TxConfirmation
}

// RegisterConf retains a buffered confirmation watch until its caller stops.
func (b *backingConfirmationBackend) RegisterConf(context.Context,
	*chainhash.Hash, []byte, uint32, uint32, bool) (
	*chainsource.ConfRegistration, error) {

	close(b.registered)

	return &chainsource.ConfRegistration{
		Confirmed: b.confirmed, Cancel: func() {},
	}, nil
}

// TestChannelBackingRequiresExactConfirmation rejects absent or unrelated
// confirmation evidence and cannot mistake mempool publication for a winner.
func TestChannelBackingRequiresExactConfirmation(t *testing.T) {
	t.Parallel()

	for _, valid := range []bool{true, false} {
		name := "unrelated transaction"
		if valid {
			name = "exact backing"
		}
		t.Run(name, func(t *testing.T) {
			tx := wire.NewMsgTx(2)
			tx.AddTxIn(&wire.TxIn{})
			tx.AddTxOut(
				&wire.TxOut{
					Value:    1_000,
					PkScript: []byte{0x51},
				},
			)
			var raw bytes.Buffer
			require.NoError(t, tx.Serialize(&raw))
			backing := arkchannel.Backing{
				ChannelPoint: wire.OutPoint{
					Hash: tx.TxHash(),
				},
				Transaction: raw.Bytes(),
			}
			backend := &backingConfirmationBackend{
				registered: make(chan struct{}),
				confirmed: make(
					chan *chainsource.TxConfirmation, 1,
				),
			}
			watcher := newArkChannelSourceWatcher(backend, nil)
			defer watcher.Stop()
			archive := &arkChannelRecoveryArchive{watcher: watcher}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				result <- archive.WaitForChannelBacking(
					ctx, arkchannel.ID{1}, backing,
				)
			}()
			<-backend.registered
			select {
			case err := <-result:
				t.Fatalf("premature backing return: %v", err)

			default:
			}
			if !valid {
				tx = tx.Copy()
				tx.TxOut[0].Value++
			}
			backend.confirmed <- &chainsource.TxConfirmation{
				Tx: tx, BlockHeight: 500,
			}
			select {
			case err := <-result:
				if valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}

			case <-time.After(5 * time.Second):
				t.Fatal("local confirmation was not consumed")
			}
		})
	}
}
