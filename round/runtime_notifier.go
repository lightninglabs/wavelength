package round

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
)

// registerWalletNotifier requests persisted boarding backlog only after the
// durable owner's restart turn has committed and its mailbox can accept input.
func (a *RoundClientActor) registerWalletNotifier(ctx context.Context,
	cfg *RoundClientConfig) error {

	ref := actor.NewMapInputRef(cfg.SelfRef,
		func(
			event wallet.BoardingUtxoConfirmedEvent,
		) actormsg.RoundReceivable {

			return &WalletBoardingConfirmed{
				Intent: event.BoardingIntent,
			}
		},
	)
	_, err := a.askWallet(ctx, &wallet.RegisterConfirmationNotifierRequest{
		NotifierID:  fmt.Sprintf("round-actor-%s", cfg.Name),
		NotifyActor: ref, BacklogHeight: fn.None[int32](),
		MinConf: fn.Some(cfg.OperatorTerms.MinConfirmations),
	})

	return err
}
