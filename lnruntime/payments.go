// Package lnruntime composes lnd's Lightning subsystems without starting an
// lnd daemon.
package lnruntime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// ErrPathfindingDisabled is returned when a caller tries to obtain a
	// route from the client runtime. Public routing belongs to the channel
	// operator.
	ErrPathfindingDisabled = errors.New("client pathfinding is disabled")

	// ErrOneHopRouteRequired is returned when a payment route does not
	// point directly at the channel operator.
	ErrOneHopRouteRequired = errors.New("operator route must contain one " +
		"hop")
)

// LinkLookup finds an active lnd channel link by its short channel ID.
type LinkLookup func(lnwire.ShortChannelID) (htlcswitch.ChannelLink, error)

// FixedRoutePaymentsConfig contains the native lnd dependencies needed to run
// durable payments over the single client-to-operator channel leg.
type FixedRoutePaymentsConfig struct {
	DB          *channeldb.DB
	Chain       lnwallet.BlockChainIO
	Payer       routing.PaymentAttemptDispatcher
	GetLink     LinkLookup
	SelfNode    route.Vertex
	Clock       clock.Clock
	ClosedSCIDs map[lnwire.ShortChannelID]struct{}

	// ApplyChannelUpdate lets an embedding runtime retain private policy
	// updates learned from failures. A nil callback safely ignores them.
	ApplyChannelUpdate func(*lnwire.ChannelUpdate1) bool

	KeepFailedPaymentAttempts bool
}

// FixedRoutePayments owns lnd's normal payment control tower and payment
// lifecycle while intentionally omitting graph construction and pathfinding.
type FixedRoutePayments struct {
	router            *routing.ChannelRouter
	control           routing.ControlTower
	missionController *routing.MissionController

	mu      sync.Mutex
	started bool
	stopped bool
}

// NewFixedRoutePayments composes lnd's existing payment components around the
// supplied channel switch or another PaymentAttemptDispatcher.
func NewFixedRoutePayments(cfg FixedRoutePaymentsConfig) (*FixedRoutePayments,
	error) {

	if cfg.DB == nil {
		return nil, fmt.Errorf("channel database is required")
	}
	if cfg.Chain == nil {
		return nil, fmt.Errorf("chain backend is required")
	}
	if cfg.Payer == nil {
		return nil, fmt.Errorf("payment dispatcher is required")
	}
	if cfg.GetLink == nil {
		return nil, fmt.Errorf("channel link lookup is required")
	}

	paymentDB, err := paymentsdb.NewKVStore(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("open lnd payment store: %w", err)
	}
	control := routing.NewControlTower(paymentDB)

	sequencer, err := htlcswitch.NewPersistentSequencer(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("open lnd payment sequencer: %w", err)
	}

	estimator, err := routing.NewAprioriEstimator(
		routing.DefaultAprioriConfig(),
	)
	if err != nil {
		return nil, fmt.Errorf("create lnd payment estimator: %w", err)
	}

	noConfigUpdate := fn.None[func(*routing.MissionControlConfig)]()
	missionController, err := routing.NewMissionController(
		cfg.DB, cfg.SelfNode, &routing.MissionControlConfig{
			Estimator:       estimator,
			OnConfigUpdate:  noConfigUpdate,
			MaxMcHistory:    routing.DefaultMaxMcHistory,
			McFlushInterval: routing.DefaultMcFlushInterval,
			MinFailureRelaxInterval: routing.
				DefaultMinFailureRelaxInterval,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create lnd mission control: %w", err)
	}

	missionControl, err := missionController.GetNamespacedStore(
		routing.DefaultMissionControlNamespace,
	)
	if err != nil {
		return nil, fmt.Errorf("open lnd mission control: %w", err)
	}

	runtimeClock := cfg.Clock
	if runtimeClock == nil {
		runtimeClock = clock.NewDefaultClock()
	}
	applyChannelUpdate := cfg.ApplyChannelUpdate
	if applyChannelUpdate == nil {
		applyChannelUpdate = func(*lnwire.ChannelUpdate1) bool {
			return false
		}
	}
	closedSCIDs := cfg.ClosedSCIDs
	if closedSCIDs == nil {
		closedSCIDs = make(map[lnwire.ShortChannelID]struct{})
	}

	noTrafficShaper := fn.None[htlcswitch.AuxTrafficShaper]()
	router, err := routing.New(routing.Config{
		SelfNode:       cfg.SelfNode,
		Chain:          cfg.Chain,
		Payer:          cfg.Payer,
		Control:        control,
		MissionControl: missionControl,
		SessionSource:  fixedRouteSessionSource{},
		GetLink: func(scid lnwire.ShortChannelID) (
			htlcswitch.ChannelLink, error) {

			return cfg.GetLink(scid)
		},
		NextPaymentID:             sequencer.NextID,
		Clock:                     runtimeClock,
		ApplyChannelUpdate:        applyChannelUpdate,
		ClosedSCIDs:               closedSCIDs,
		TrafficShaper:             noTrafficShaper,
		KeepFailedPaymentAttempts: cfg.KeepFailedPaymentAttempts,
	})
	if err != nil {
		return nil, fmt.Errorf("create lnd payment lifecycle: %w", err)
	}

	return &FixedRoutePayments{
		router:            router,
		control:           control,
		missionController: missionController,
	}, nil
}

// Start reloads in-flight attempts through lnd's normal payment lifecycle.
func (p *FixedRoutePayments) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.started {
		return nil
	}
	if p.stopped {
		return fmt.Errorf("fixed-route payments already stopped")
	}

	p.missionController.RunStoreTickers()
	if err := p.router.Start(); err != nil {
		p.missionController.StopStoreTickers()

		return fmt.Errorf("start lnd payment lifecycle: %w", err)
	}

	p.started = true

	return nil
}

// Stop shuts down payment result collectors before stopping mission-control
// persistence.
func (p *FixedRoutePayments) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return nil
	}
	p.stopped = true

	var stopErr error
	if p.started {
		stopErr = p.router.Stop()
	}
	p.missionController.StopStoreTickers()

	return stopErr
}

// SendToOperator executes one payment attempt over the private operator leg
// and persists its result in lnd's control tower.
func (p *FixedRoutePayments) SendToOperator(ctx context.Context,
	paymentHash lntypes.Hash, paymentRoute *route.Route,
	firstHopRecords lnwire.CustomRecords) (*paymentsdb.HTLCAttempt, error) {

	if paymentRoute == nil || len(paymentRoute.Hops) != 1 {
		return nil, ErrOneHopRouteRequired
	}

	return p.router.SendToRoute(
		ctx, paymentHash, paymentRoute, firstHopRecords,
	)
}

// ControlTower exposes lnd's read and subscription interface for payment
// accounting without exposing the graph-capable channel router.
func (p *FixedRoutePayments) ControlTower() routing.ControlTower {
	return p.control
}

// fixedRouteSessionSource only supplies the empty session lnd needs when it
// resumes already-dispatched attempts after a restart.
type fixedRouteSessionSource struct{}

// NewPaymentSession rejects graph-based payment requests.
func (fixedRouteSessionSource) NewPaymentSession(*routing.LightningPayment,
	fn.Option[tlv.Blob], fn.Option[htlcswitch.AuxTrafficShaper]) (
	routing.PaymentSession, error) {

	return nil, ErrPathfindingDisabled
}

// NewPaymentSessionEmpty returns a session that cannot create another shard.
func (fixedRouteSessionSource) NewPaymentSessionEmpty() routing.PaymentSession {
	return emptyPaymentSession{}
}

// emptyPaymentSession supports result collection for attempts lnd already
// persisted, but cannot select another route.
type emptyPaymentSession struct{}

// RequestRoute rejects any attempt to create a new route.
func (emptyPaymentSession) RequestRoute(lnwire.MilliSatoshi,
	lnwire.MilliSatoshi, uint32, uint32, lnwire.CustomRecords) (
	*route.Route, error) {

	return nil, ErrPathfindingDisabled
}

// UpdateAdditionalEdge ignores policy updates because no graph is present.
func (emptyPaymentSession) UpdateAdditionalEdge(*lnwire.ChannelUpdate1,
	*btcec.PublicKey, *models.CachedEdgePolicy) bool {

	return false
}

// GetAdditionalEdgePolicy reports that no graph policy is available.
func (emptyPaymentSession) GetAdditionalEdgePolicy(*btcec.PublicKey,
	uint64) *models.CachedEdgePolicy {

	return nil
}

var _ routing.PaymentSessionSource = fixedRouteSessionSource{}
var _ routing.PaymentSession = emptyPaymentSession{}
