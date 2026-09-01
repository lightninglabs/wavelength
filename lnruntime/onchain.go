package lnruntime

import (
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/contractcourt"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

type (
	sweepScriptGenerator       = func() fn.Result[lnwallet.AddrWithKey]
	optionalOutgoingResolution = fn.Option[lnwallet.OutgoingHtlcResolution]
	optionalIncomingResolution = fn.Option[lnwallet.IncomingHtlcResolution]
)

// OnchainConfig contains the application-owned admission and observation
// boundaries around lnd's standard channel resolution subsystems.
type OnchainConfig struct {
	Mempool chainntnfs.MempoolWatcher

	ShouldWatchChannel      func(wire.OutPoint) (bool, error)
	BeforeCommitmentPublish func(wire.OutPoint) error
	RecordFullyResolved     func(wire.OutPoint) error
}

// OnchainRuntime composes lnd's chain arbitrator, breach handling, nursery,
// sweeper, transaction publisher, and block dispatcher around the same channel
// database used by the off-chain runtime.
type OnchainRuntime struct {
	chainArbitrator  *contractcourt.ChainArbitrator
	breachArbitrator *contractcourt.BreachArbitrator
	nursery          *contractcourt.UtxoNursery
	sweeper          *sweep.UtxoSweeper
	txPublisher      *sweep.TxPublisher
	dispatcher       *chainio.BlockbeatDispatcher

	chain lnwallet.BlockChainIO
	quit  chan struct{}

	mu      sync.Mutex
	started bool
	stopped bool
}

// newOnchainRuntime wires lnd's existing on-chain components to the modular
// runtime. The funding runtime supplies the LightningWallet used by the normal
// sweeper, avoiding a second wallet or a parallel output-resolution engine.
func newOnchainRuntime(runtime *Runtime,
	cfg OnchainConfig) (*OnchainRuntime, error) {

	if runtime == nil || runtime.funding == nil ||
		runtime.funding.wallet == nil {
		return nil, fmt.Errorf("on-chain lifecycle requires lnd " +
			"funding wallet")
	}
	if cfg.ShouldWatchChannel == nil {
		return nil, fmt.Errorf("on-chain channel admission is required")
	}
	if cfg.BeforeCommitmentPublish == nil {
		return nil, fmt.Errorf("commitment publication barrier is " +
			"required")
	}
	if cfg.RecordFullyResolved == nil {
		return nil, fmt.Errorf("channel resolution recorder is " +
			"required")
	}

	stateDB := runtime.cfg.DB.ChannelStateDB()
	chainHash := runtime.funding.netParams.GenesisHash
	nurseryStore, err := contractcourt.NewNurseryStore(
		chainHash, runtime.cfg.DB,
	)
	if err != nil {
		return nil, fmt.Errorf("create lnd nursery store: %w", err)
	}
	sweeperStore, err := sweep.NewSweeperStore(
		runtime.cfg.DB.Backend, chainHash,
	)
	if err != nil {
		return nil, fmt.Errorf("create lnd sweeper store: %w", err)
	}

	lightningWallet := runtime.funding.wallet
	noAuxSweeper := fn.None[sweep.AuxSweeper]()
	aggregator := sweep.NewBudgetAggregator(
		runtime.cfg.FeeEstimator, sweep.DefaultMaxInputsPerTx,
		noAuxSweeper,
	)
	txPublisher := sweep.NewTxPublisher(sweep.TxPublisherConfig{
		Signer: runtime.cfg.Signer, Wallet: lightningWallet,
		Estimator:  runtime.cfg.FeeEstimator,
		Notifier:   runtime.cfg.Notifier,
		AuxSweeper: noAuxSweeper,
	})
	genSweepScript := newSweepScriptGenerator(
		runtime.funding.wallet.WalletController,
		runtime.funding.netParams,
	)
	utxoSweeper := sweep.New(&sweep.UtxoSweeperConfig{
		FeeEstimator:   runtime.cfg.FeeEstimator,
		GenSweepScript: genSweepScript,
		Signer:         runtime.cfg.Signer,
		Wallet:         lightningWallet,
		Mempool:        cfg.Mempool,
		Notifier:       runtime.cfg.Notifier,
		Store:          sweeperStore,
		MaxInputsPerTx: sweep.DefaultMaxInputsPerTx,
		MaxFeeRate:     sweep.DefaultMaxFeeRate,
		Aggregator:     aggregator,
		Publisher:      txPublisher,
		NoDeadlineConfTarget: uint32(
			sweep.DefaultDeadlineDelta,
		),
	})
	budget := contractcourt.DefaultBudgetConfig()
	nursery := contractcourt.NewUtxoNursery(&contractcourt.NurseryConfig{
		ChainIO: runtime.cfg.Chain, ConfDepth: 1,
		FetchClosedChannels: stateDB.FetchClosedChannels,
		FetchClosedChannel:  stateDB.FetchClosedChannel,
		Notifier:            runtime.cfg.Notifier,
		PublishTransaction:  lightningWallet.PublishTransaction,
		Store:               nurseryStore,
		SweepInput:          utxoSweeper.SweepInput,
		Budget:              budget,
	})

	contractBreaches := make(chan *contractcourt.ContractBreachEvent, 1)
	breachArbitrator := contractcourt.NewBreachArbitrator(
		&contractcourt.BreachConfig{
			CloseLink: func(channelPoint *wire.OutPoint,
				_ contractcourt.ChannelCloseType) {

				runtime.RemoveLink(*channelPoint)
			},
			DB: stateDB, Estimator: runtime.cfg.FeeEstimator,
			GenSweepScript: genSweepScript,
			Notifier:       runtime.cfg.Notifier,
			PublishTransaction: lightningWallet.
				PublishTransaction,
			ContractBreaches: contractBreaches,
			Signer:           runtime.cfg.Signer,
			Store: contractcourt.NewRetributionStore(
				runtime.cfg.DB.Backend,
			),
			AuxSweeper: noAuxSweeper,
		},
	)

	onchain := &OnchainRuntime{
		breachArbitrator: breachArbitrator, nursery: nursery,
		sweeper: utxoSweeper, txPublisher: txPublisher,
		dispatcher: chainio.NewBlockbeatDispatcher(
			runtime.cfg.Notifier,
		),
		chain: runtime.cfg.Chain, quit: make(chan struct{}),
	}
	onchain.chainArbitrator = newChainArbitrator(
		runtime, cfg, onchain, lightningWallet, contractBreaches,
		budget, genSweepScript,
	)
	onchain.dispatcher.RegisterQueue([]chainio.Consumer{
		onchain.chainArbitrator, utxoSweeper, txPublisher,
	})

	return onchain, nil
}

// newChainArbitrator composes lnd's contract owner around the already-created
// sweeper, nursery, and breach arbitrator.
func newChainArbitrator(runtime *Runtime, cfg OnchainConfig,
	onchain *OnchainRuntime, lightningWallet *lnwallet.LightningWallet,
	contractBreaches chan<- *contractcourt.ContractBreachEvent,
	budget *contractcourt.BudgetConfig,
	genSweepScript sweepScriptGenerator) *contractcourt.ChainArbitrator {

	stateDB := runtime.cfg.DB.ChannelStateDB()
	noAuxLeafStore := fn.None[lnwallet.AuxLeafStore]()
	noAuxSigner := fn.None[lnwallet.AuxSigner]()
	noAuxResolver := fn.None[lnwallet.AuxContractResolver]()
	noAuxCloser := fn.None[contractcourt.AuxChanCloser]()
	noCloseConfs := fn.None[uint32]()
	noCustomHtlcChecker := fn.None[contractcourt.CustomHtlcChecker]()
	processResolution := runtime.switcher.ProcessContractResolution

	return contractcourt.NewChainArbitrator(
		contractcourt.ChainArbitratorConfig{
			ChainHash: *runtime.funding.netParams.GenesisHash,
			IncomingBroadcastDelta: lncfg.
				DefaultIncomingBroadcastDelta,
			OutgoingBroadcastDelta: lncfg.
				DefaultOutgoingBroadcastDelta,
			CustomHtlcChecker: noCustomHtlcChecker,
			NewSweepAddr: func() ([]byte, error) {
				addr, err := genSweepScript().Unpack()
				if err != nil {
					return nil, err
				}

				return addr.DeliveryAddress, nil
			},
			PublishTx: lightningWallet.PublishTransaction,
			BeforeCommitmentPublish: cfg.
				BeforeCommitmentPublish,
			DeliverResolutionMsg: func(
				messages ...contractcourt.ResolutionMsg) error {

				for _, message := range messages {
					err := processResolution(message)
					if err != nil {
						return err
					}
				}

				return nil
			},
			MarkLinkInactive: func(
				channelPoint wire.OutPoint) error {

				runtime.RemoveLink(channelPoint)

				return nil
			},
			ContractBreach: func(channelPoint wire.OutPoint,
				retribution *lnwallet.BreachRetribution) error {

				return onchain.handOffBreach(
					contractBreaches, channelPoint,
					retribution,
				)
			},
			IsOurAddress: lightningWallet.IsOurAddress,
			IncubateOutputs: func(channelPoint wire.OutPoint,
				outgoing optionalOutgoingResolution,
				incoming optionalIncomingResolution,
				broadcastHeight uint32,
				deadline fn.Option[int32],
				opts ...contractcourt.IncubateOption) error {

				return onchain.nursery.IncubateOutputs(
					channelPoint, outgoing, incoming,
					broadcastHeight, deadline, opts...,
				)
			},
			PreimageDB:   runtime.cfg.WitnessBeacon,
			Notifier:     runtime.cfg.Notifier,
			Mempool:      cfg.Mempool,
			Signer:       runtime.cfg.Signer,
			FeeEstimator: runtime.cfg.FeeEstimator,
			ChainIO:      runtime.cfg.Chain,
			DisableChannel: func(wire.OutPoint) error {
				return nil
			},
			Sweeper:             onchain.sweeper,
			Registry:            runtime.invoices,
			NotifyClosedChannel: func(wire.OutPoint) {},
			NotifyEarlyClosedChannel: func(
				*channeldb.ChannelCloseSummary) {
			},
			BeforeFullyResolvedChannel: cfg.RecordFullyResolved,
			OnionProcessor:             runtime.onionProcessor,
			IsForwardedHTLC: runtime.switcher.
				IsForwardedHTLC,
			Clock: runtime.cfg.Clock,
			SubscribeBreachComplete: onchain.breachArbitrator.
				SubscribeBreachComplete,
			PutFinalHtlcOutcome: stateDB.PutOnchainFinalHtlcOutcome,
			HtlcNotifier:        runtime.htlcNotifier,
			Budget:              *budget,
			QueryIncomingCircuit: func(
				key models.CircuitKey) *models.CircuitKey {

				circuit := runtime.switcher.CircuitLookup().
					LookupOpenCircuit(key)
				if circuit == nil {
					return nil
				}

				return &circuit.Incoming
			},
			AuxLeafStore:      noAuxLeafStore,
			AuxSigner:         noAuxSigner,
			AuxResolver:       noAuxResolver,
			AuxCloser:         noAuxCloser,
			ChannelCloseConfs: noCloseConfs,
			ShouldWatchChannel: func(
				channel *chanstate.OpenChannel) (bool, error) {

				return cfg.ShouldWatchChannel(
					channel.FundingOutpoint,
				)
			},
		}, runtime.cfg.DB,
	)
}

// newSweepScriptGenerator derives one wallet-owned taproot output for lnd's
// sweeper and includes the internal key metadata used by auxiliary sweepers.
func newSweepScriptGenerator(wallet lnwallet.WalletController,
	netParams *chaincfg.Params) sweepScriptGenerator {

	return func() fn.Result[lnwallet.AddrWithKey] {
		addr, err := wallet.NewAddress(
			lnwallet.TaprootPubkey, false,
			lnwallet.DefaultAccountName,
		)
		if err != nil {
			return fn.Err[lnwallet.AddrWithKey](err)
		}
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return fn.Err[lnwallet.AddrWithKey](err)
		}
		internalKey, err := lnwallet.InternalKeyForAddr(
			wallet, netParams, pkScript,
		)
		if err != nil {
			return fn.Err[lnwallet.AddrWithKey](err)
		}

		return fn.Ok(lnwallet.AddrWithKey{
			DeliveryAddress: pkScript, InternalKey: internalKey,
		})
	}
}

// handOffBreach waits until the breach arbitrator has durably accepted one
// retribution before allowing the channel arbitrator to advance.
func (o *OnchainRuntime) handOffBreach(
	breaches chan<- *contractcourt.ContractBreachEvent,
	channelPoint wire.OutPoint,
	retribution *lnwallet.BreachRetribution) error {

	result := make(chan error, 1)
	event := &contractcourt.ContractBreachEvent{
		ChanPoint: channelPoint, BreachRetribution: retribution,
		ProcessACK: func(err error) {
			result <- err
		},
	}
	select {
	case breaches <- event:
	case <-o.quit:
		return fmt.Errorf("on-chain runtime stopped")
	}
	select {
	case err := <-result:
		return err

	case <-o.quit:
		return fmt.Errorf("on-chain runtime stopped")
	}
}

// Start starts lnd's on-chain components and block dispatcher in dependency
// order after the HTLC switch is available for resolver messages.
func (o *OnchainRuntime) Start() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started {
		return nil
	}
	if o.stopped {
		return fmt.Errorf("on-chain runtime already stopped")
	}

	blockHash, height, err := o.chain.GetBestBlock()
	if err != nil {
		return fmt.Errorf("read on-chain runtime tip: %w", err)
	}
	beat := chainio.NewBeat(chainntnfs.BlockEpoch{
		Hash: blockHash, Height: height,
	})
	if err := o.txPublisher.Start(beat); err != nil {
		return fmt.Errorf("start lnd transaction publisher: %w", err)
	}
	if err := o.sweeper.Start(beat); err != nil {
		_ = o.txPublisher.Stop()

		return fmt.Errorf("start lnd sweeper: %w", err)
	}
	if err := o.nursery.Start(); err != nil {
		_ = o.sweeper.Stop()
		_ = o.txPublisher.Stop()

		return fmt.Errorf("start lnd nursery: %w", err)
	}
	if err := o.breachArbitrator.Start(); err != nil {
		_ = o.nursery.Stop()
		_ = o.sweeper.Stop()
		_ = o.txPublisher.Stop()

		return fmt.Errorf("start lnd breach arbitrator: %w", err)
	}
	if err := o.chainArbitrator.Start(beat); err != nil {
		_ = o.breachArbitrator.Stop()
		_ = o.nursery.Stop()
		_ = o.sweeper.Stop()
		_ = o.txPublisher.Stop()

		return fmt.Errorf("start lnd chain arbitrator: %w", err)
	}
	if err := o.dispatcher.Start(); err != nil {
		_ = o.chainArbitrator.Stop()
		_ = o.breachArbitrator.Stop()
		_ = o.nursery.Stop()
		_ = o.sweeper.Stop()
		_ = o.txPublisher.Stop()

		return fmt.Errorf("start lnd block dispatcher: %w", err)
	}

	o.started = true

	return nil
}

// Stop stops block delivery before shutting down lnd's resolution components
// in reverse dependency order.
func (o *OnchainRuntime) Stop() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopped {
		return nil
	}
	o.stopped = true
	if !o.started {
		return nil
	}

	o.dispatcher.Stop()
	close(o.quit)

	return errors.Join(
		o.chainArbitrator.Stop(), o.breachArbitrator.Stop(),
		o.nursery.Stop(), o.sweeper.Stop(), o.txPublisher.Stop(),
	)
}

// WatchChannel admits one materializing channel to lnd's standard chain
// watcher and contract arbitrator. Repeated calls are idempotent.
func (o *OnchainRuntime) WatchChannel(channel *chanstate.OpenChannel) error {
	if channel == nil {
		return fmt.Errorf("open channel state is required")
	}

	return o.chainArbitrator.WatchNewChannel(channel)
}

// ForgetChannel retires the watcher and resolver state after an
// application-owned cooperative close is confirmed and archived in lnd.
func (o *OnchainRuntime) ForgetChannel(channelPoint wire.OutPoint) error {
	return o.chainArbitrator.ResolveContract(channelPoint)
}

// LinkConfig returns the real chain event and contract callbacks for a channel
// whose funding outpoint may still be unpublished.
func (o *OnchainRuntime) LinkConfig(channelPoint wire.OutPoint) (
	*contractcourt.ChainEventSubscription,
	func(*contractcourt.ContractSignals) error,
	func(*contractcourt.ContractUpdate) error, error) {

	events, err := o.chainArbitrator.SubscribeChannelEvents(channelPoint)
	if err != nil {
		return nil, nil, nil, err
	}

	return events, func(signals *contractcourt.ContractSignals) error {
			return o.chainArbitrator.UpdateContractSignals(
				channelPoint, signals,
			)
		}, func(update *contractcourt.ContractUpdate) error {
			return o.chainArbitrator.NotifyContractUpdate(
				channelPoint, update,
			)
		}, nil
}

// ForceClose asks lnd's channel arbitrator to publish the latest commitment
// and retain ownership of every resulting contract until fully resolved.
func (o *OnchainRuntime) ForceClose(channelPoint wire.OutPoint) (*wire.MsgTx,
	error) {

	return o.chainArbitrator.ForceCloseContract(channelPoint)
}

// ResumeForceClose retries a commitment publication that stopped at the Ark
// backing barrier and is idempotent after lnd has progressed beyond it.
func (o *OnchainRuntime) ResumeForceClose(channelPoint wire.OutPoint) (
	*wire.MsgTx, error) {

	return o.chainArbitrator.ResumeForceCloseContract(channelPoint)
}
