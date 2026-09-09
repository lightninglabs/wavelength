package lnruntime

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	clientdb "github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	lndfunding "github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// statusPollFundingBackend models a finalized local lnd reservation and rejects
// any recursive attempt to open the same durable channel action.
type statusPollFundingBackend struct {
	pendingID lndfunding.PendingChanID
	capacity  btcutil.Amount
	output    *wire.TxOut
	psbt      []byte

	openCalls atomic.Int32
	finalized atomic.Bool
	active    atomic.Bool
	cancelled atomic.Bool
}

// newStatusPollFundingBackend creates one deterministic external-funding PSBT.
func newStatusPollFundingBackend(t *testing.T, pendingID [32]byte,
	capacity btcutil.Amount) *statusPollFundingBackend {

	t.Helper()

	pkScript := append([]byte{0x00, 0x20}, bytes.Repeat([]byte{1}, 32)...)
	packet, err := psbt.New(nil, []*wire.TxOut{{
		Value:    int64(capacity),
		PkScript: pkScript,
	}}, 2, 0, nil)
	require.NoError(t, err)
	var encoded bytes.Buffer
	require.NoError(t, packet.Serialize(&encoded))

	return &statusPollFundingBackend{
		pendingID: pendingID,
		capacity:  capacity,
		output: &wire.TxOut{
			Value:    int64(capacity),
			PkScript: pkScript,
		},
		psbt: encoded.Bytes(),
	}
}

// OpenChannel returns the negotiated PSBT once and rejects recursive dispatch.
func (b *statusPollFundingBackend) OpenChannel(request FundingOpenRequest) (
	*FundingFlow, error) {

	if b.openCalls.Add(1) != 1 {
		return nil, fmt.Errorf("channel funding opened more than once")
	}
	if request.PendingChannelID != b.pendingID {
		return nil, fmt.Errorf("unexpected pending channel ID")
	}
	if request.Capacity != b.capacity {
		return nil, fmt.Errorf("unexpected channel capacity")
	}

	updates := make(chan *lnrpc.OpenStatusUpdate, 1)
	updates <- &lnrpc.OpenStatusUpdate{
		PendingChanId: request.PendingChannelID[:],
		Update: &lnrpc.OpenStatusUpdate_PsbtFund{
			PsbtFund: &lnrpc.ReadyForPsbtFunding{
				FundingAmount: int64(request.Capacity),
				Psbt:          bytes.Clone(b.psbt),
			},
		},
	}

	return &FundingFlow{
		PendingChannelID: request.PendingChannelID,
		Updates:          updates,
		Errors:           make(chan error),
	}, nil
}

// ExpectedFundingOutput returns the exact output negotiated with lnd.
func (b *statusPollFundingBackend) ExpectedFundingOutput(
	pendingID lndfunding.PendingChanID) (*wire.TxOut, error) {

	if pendingID != b.pendingID {
		return nil, fmt.Errorf("unexpected pending channel ID")
	}

	return &wire.TxOut{
		Value:    b.output.Value,
		PkScript: bytes.Clone(b.output.PkScript),
	}, nil
}

// RegisterBacking accepts the immutable backing registered before finalization.
func (*statusPollFundingBackend) RegisterBacking(VirtualFunding) error {
	return nil
}

// FinalizeBacking marks the local lnd reservation durable.
func (b *statusPollFundingBackend) FinalizeBacking(
	pendingID lndfunding.PendingChanID, _ *psbt.Packet,
	_ VirtualFunding) error {

	if pendingID != b.pendingID {
		return fmt.Errorf("unexpected pending channel ID")
	}
	b.finalized.Store(true)

	return nil
}

// FundingFinalized reports the local durable reservation state.
func (b *statusPollFundingBackend) FundingFinalized(context.Context,
	arkchannel.Terms, arkchannel.Backing) (bool, error) {

	return b.finalized.Load(), nil
}

// ChannelActive reports whether the virtual funding confirmation was injected.
func (b *statusPollFundingBackend) ChannelActive(context.Context,
	arkchannel.Terms, arkchannel.Backing) (bool, error) {

	return b.active.Load(), nil
}

// CancelBacking accepts the unused cancellation edge.
func (b *statusPollFundingBackend) CancelBacking(lndfunding.PendingChanID,
	*wire.OutPoint) error {

	b.cancelled.Store(true)

	return nil
}

// statusPollCounterparty delays remote finalization and records recovery-order
// evidence at the application transport boundary.
type statusPollCounterparty struct {
	party       arkchannel.Party
	key         *btcec.PrivateKey
	readyAfter  int32
	fundingPoll atomic.Int32

	mu               sync.Mutex
	localService     *arkchannel.Service
	peerRecoverySet  bool
	recoveryExports  int
	recoveryInstalls int
	cancelBackend    *statusPollFundingBackend
	abortAfterCancel bool
}

// SignBacking signs the exact remote channel-policy path.
func (p *statusPollCounterparty) SignBacking(_ context.Context, _ arkchannel.ID,
	terms arkchannel.Terms, source arkchannel.VTXOBinding,
	packet *psbt.Packet) (input.Signature, error) {

	packetCopy, err := cloneFundingPSBT(packet)
	if err != nil {
		return nil, err
	}
	template, err := arkchannel.NewBackingTemplate(
		packetCopy, terms, source,
	)
	if err != nil {
		return nil, err
	}
	desc, err := template.SignDescriptor(
		terms, p.party, keychain.KeyDescriptor{
			PubKey: p.key.PubKey(),
		},
	)
	if err != nil {
		return nil, err
	}

	return input.NewMockSigner(
		[]*btcec.PrivateKey{p.key}, nil,
	).SignOutputRaw(template.Packet().UnsignedTx, desc)
}

// InstallBacking verifies the fully signed backing before acknowledging it.
func (*statusPollCounterparty) InstallBacking(_ context.Context,
	_ arkchannel.ID, terms arkchannel.Terms, source arkchannel.VTXOBinding,
	backing arkchannel.Backing) error {

	return backing.Validate(terms, source)
}

// FundingFinalized withholds the remote barrier across several status polls.
func (p *statusPollCounterparty) FundingFinalized(context.Context,
	arkchannel.Terms, arkchannel.Backing) (bool, error) {

	return p.fundingPoll.Add(1) >= p.readyAfter, nil
}

// ChannelActive reports the remote link active after recovery coordination.
func (*statusPollCounterparty) ChannelActive(context.Context, arkchannel.Terms,
	arkchannel.Backing) (bool, error) {

	return true, nil
}

// ApplyChannelEvent acknowledges peer evidence only after checking that local
// recovery evidence was durable before the peer notification.
func (p *statusPollCounterparty) ApplyChannelEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := event.(*arkchannel.RecoveryPackageInstalled); ok {
		if p.localService != nil {
			record, err := p.localService.GetChannel(ctx, id)
			if err != nil {
				return arkchannel.Record{}, err
			}
			if !record.Snapshot.ClientRecoveryReady {
				return arkchannel.Record{}, fmt.Errorf(
					"local recovery is not durable")
			}
		}
		p.peerRecoverySet = true
	}
	if _, ok := event.(*arkchannel.OORAborted); ok &&
		p.cancelBackend != nil {

		p.abortAfterCancel = p.cancelBackend.cancelled.Load()
	}

	return arkchannel.Record{}, nil
}

// ExportRecoveryPackage returns the remote funder's finalized test package.
func (p *statusPollCounterparty) ExportRecoveryPackage(context.Context,
	arkchannel.ID) (arkchannel.RecoveryPackage, error) {

	p.mu.Lock()
	defer p.mu.Unlock()
	p.recoveryExports++

	return arkchannel.RecoveryPackage{}, nil
}

// cancellationSink accepts terminal native funding callbacks for ordering
// tests without introducing another channel state machine.
type cancellationSink struct{}

// ApplyLocalEvent accepts local funding cancellation evidence.
func (*cancellationSink) ApplyLocalEvent(context.Context, arkchannel.ID,
	arkchannel.Event) (arkchannel.Record, error) {

	return arkchannel.Record{}, nil
}

// ApplyPeerEvent accepts authenticated peer evidence.
func (*cancellationSink) ApplyPeerEvent(context.Context, arkchannel.ID,
	arkchannel.Event) (arkchannel.Record, error) {

	return arkchannel.Record{}, nil
}

// RecordLocalEvent accepts local evidence without action dispatch.
func (*cancellationSink) RecordLocalEvent(context.Context, arkchannel.ID,
	arkchannel.Event) (arkchannel.Record, error) {

	return arkchannel.Record{}, nil
}

// ResumeChannelAction accepts action replay requests.
func (*cancellationSink) ResumeChannelAction(context.Context, arkchannel.ID) (
	arkchannel.Record, error) {

	return arkchannel.Record{}, nil
}

// InstallRecoveryPackage accepts remote recovery only after the peer observed
// the local durable barrier.
func (p *statusPollCounterparty) InstallRecoveryPackage(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding,
	arkchannel.RecoveryPackage) error {

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.peerRecoverySet {
		return fmt.Errorf("remote recovery installed before peer " +
			"barrier")
	}
	p.recoveryInstalls++

	return nil
}

// recoveryInstallCount returns the number of remote package installations.
func (p *statusPollCounterparty) recoveryInstallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.recoveryInstalls
}

// recoveryExportCount returns the number of fetched remote packages.
func (p *statusPollCounterparty) recoveryExportCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.recoveryExports
}

// abortObservedAfterCancel reports the order seen at the peer boundary.
func (p *statusPollCounterparty) abortObservedAfterCancel() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.abortAfterCancel
}

// statusPollRecoveryManager detects recursive preparation after local evidence
// has already been persisted.
type statusPollRecoveryManager struct {
	exports  atomic.Int32
	installs atomic.Int32
}

// ExportRecoveryPackage returns the endpoint-neutral test package.
func (m *statusPollRecoveryManager) ExportRecoveryPackage(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding) (
	arkchannel.RecoveryPackage, error) {

	m.exports.Add(1)

	return arkchannel.RecoveryPackage{}, nil
}

// InstallRecoveryPackage rejects a recursive invocation in one service action.
func (m *statusPollRecoveryManager) InstallRecoveryPackage(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding,
	arkchannel.RecoveryPackage) error {

	if m.installs.Add(1) != 1 {
		return fmt.Errorf("channel recovery prepared more than once")
	}

	return nil
}

// statusPollOORController records OOR finalization through the bound service.
type statusPollOORController struct {
	sink arkchannel.ChannelEventSink
}

// BindChannelEventSink binds the durable channel service.
func (c *statusPollOORController) BindChannelEventSink(
	sink arkchannel.ChannelEventSink) error {

	c.sink = sink

	return nil
}

// ValidatePreparedOOR validates the exact source binding.
func (*statusPollOORController) ValidatePreparedOOR(_ context.Context,
	terms arkchannel.Terms, source arkchannel.VTXOBinding) error {

	return source.Validate(terms)
}

// CommitPreparedOOR records completion after the durable commit action starts.
func (c *statusPollOORController) CommitPreparedOOR(ctx context.Context,
	id arkchannel.ID, _ arkchannel.Terms,
	source arkchannel.VTXOBinding) error {

	_, err := c.sink.ApplyLocalEvent(ctx, id, &arkchannel.OORFinalized{
		SessionID: source.OORSessionID,
	})

	return err
}

// AbortPreparedOOR accepts the unused abort edge.
func (*statusPollOORController) AbortPreparedOOR(context.Context, arkchannel.ID,
	arkchannel.Terms, arkchannel.VTXOBinding, string) error {

	return nil
}

// statusPollNativeEdges supplies unrelated native executor boundaries while
// recording virtual funding activation.
type statusPollNativeEdges struct {
	backend     *statusPollFundingBackend
	activations atomic.Int32
}

// ConfirmBacking marks the fake native channel active.
func (e *statusPollNativeEdges) ConfirmBacking(chainhash.Hash) error {
	e.activations.Add(1)
	e.backend.active.Store(true)

	return nil
}

// HandoffChannel accepts the unused on-chain handoff edge.
func (*statusPollNativeEdges) HandoffChannel(wire.OutPoint) error {
	return nil
}

// EnsureForceCloseChannel accepts the unused force-close edge.
func (*statusPollNativeEdges) EnsureForceCloseChannel(wire.OutPoint) error {
	return nil
}

// NegotiateCooperativeClose accepts the unused cooperative-close edge.
func (*statusPollNativeEdges) NegotiateCooperativeClose(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding,
	arkchannel.Backing, arkchannel.CooperativeCloseRequest) error {

	return nil
}

// PublishCooperativeClose accepts the unused cooperative publication edge.
func (*statusPollNativeEdges) PublishCooperativeClose(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.VTXOBinding,
	arkchannel.CooperativeClose) error {

	return nil
}

// FinalizeCooperativeClose accepts the unused cooperative finalization edge.
func (*statusPollNativeEdges) FinalizeCooperativeClose(context.Context,
	arkchannel.ID, arkchannel.Terms, arkchannel.Backing,
	arkchannel.VTXOBinding, arkchannel.CooperativeCloseRequest,
	arkchannel.CooperativeClose) error {

	return nil
}

// statusPollBootstrapExecutor prepares durable state without running actions.
type statusPollBootstrapExecutor struct{}

// ValidatePreparedOOR validates state setup through the production service API.
func (*statusPollBootstrapExecutor) ValidatePreparedOOR(_ context.Context,
	terms arkchannel.Terms, source arkchannel.VTXOBinding) error {

	return source.Validate(terms)
}

// Execute leaves state setup actions pending for the restarted native executor.
func (*statusPollBootstrapExecutor) Execute(context.Context, arkchannel.ID,
	arkchannel.Action) error {

	return nil
}

// RecordLocalEvent preserves compatibility for native funding component tests
// whose lightweight sink intentionally mirrors state without an executor.
func (s *fundingNegotiationSink) RecordLocalEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	return s.Apply(ctx, id, event)
}

// ResumeChannelAction returns the lightweight component test's current record.
func (s *fundingNegotiationSink) ResumeChannelAction(context.Context,
	arkchannel.ID) (arkchannel.Record, error) {

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.record, nil
}

// RecordLocalEvent persists a native lnd fact without recursively dispatching
// the service action, then mirrors it in the component-test sink.
func (s *fundingWireTestSink) RecordLocalEvent(ctx context.Context,
	id arkchannel.ID, event arkchannel.Event) (arkchannel.Record, error) {

	if _, err := s.service.RecordLocalEvent(ctx, id, event); err != nil {
		return arkchannel.Record{}, err
	}

	return s.mirror.Apply(ctx, id, event)
}

// ResumeChannelAction delegates replay to the real service used by the wire
// component test.
func (s *fundingWireTestSink) ResumeChannelAction(ctx context.Context,
	id arkchannel.ID) (arkchannel.Record, error) {

	return s.service.ResumeChannelAction(ctx, id)
}

// TestFundingStatusPollingDoesNotReopenChannel proves repeated successful local
// status observations cannot recursively dispatch NegotiateFunding while the
// remote lnd finalization barrier remains false.
func TestFundingStatusPollingDoesNotReopenChannel(t *testing.T) {
	t.Parallel()

	terms, source, clientKey, hubKey := statusPollChannel(t)
	coordinator := statusPollCoordinator(t)
	backend := newStatusPollFundingBackend(
		t, terms.PendingChannelID, terms.Capacity,
	)
	remote := &statusPollCounterparty{
		party:      arkchannel.PartyHub,
		key:        hubKey,
		readyAfter: 4,
	}
	recovery := &statusPollRecoveryManager{}
	service, edges := statusPollService(
		t, coordinator, terms, clientKey, backend, remote, recovery,
	)
	remote.localService = service

	record, err := service.PromoteVTXO(t.Context(), terms, source)
	require.NoError(t, err)
	require.Equal(t, int32(4), remote.fundingPoll.Load())
	require.Equal(t, int32(1), backend.openCalls.Load())
	require.True(t, record.Snapshot.ClientFinalized)
	require.True(t, record.Snapshot.HubFinalized)
	require.Equal(t, arkchannel.PhaseActive, record.Snapshot.Phase)
	require.Equal(t, int32(1), edges.activations.Load())
}

// TestCancelChannelNotifiesAfterLocalCleanup proves an authenticated OOR abort
// reaches the peer only after this endpoint removed its native lnd reservation.
func TestCancelChannelNotifiesAfterLocalCleanup(t *testing.T) {
	t.Parallel()

	terms, source, clientKey, _ := statusPollChannel(t)
	backend := newStatusPollFundingBackend(
		t, terms.PendingChannelID, terms.Capacity,
	)
	endpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyClient, backend,
		input.NewMockSigner(
			[]*btcec.PrivateKey{clientKey}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: clientKey.PubKey(),
		},
	)
	require.NoError(t, err)
	remote := &statusPollCounterparty{
		party: arkchannel.PartyHub, key: statusPollKey(t),
		cancelBackend: backend,
	}
	negotiator, err := NewChannelNegotiator(
		endpoint, remote, &Peer{}, &statusPollRecoveryManager{},
	)
	require.NoError(t, err)
	require.NoError(
		t,
		negotiator.BindChannelEventSink(
			&cancellationSink{},
		),
	)

	require.NoError(
		t,
		negotiator.CancelChannel(
			t.Context(), terms.ID, terms, source, nil,
			"funding rejected",
		),
	)
	require.True(t, backend.cancelled.Load())
	require.True(t, remote.abortObservedAfterCancel())
}

// TestRecoveryResumeDoesNotRecursivelyPrepare proves a restart from only local
// recovery readiness completes the peer barrier without re-entering the same
// PrepareRecovery action.
func TestRecoveryResumeDoesNotRecursivelyPrepare(t *testing.T) {
	t.Parallel()

	terms, source, clientKey, hubKey := statusPollChannel(t)
	coordinator := statusPollCoordinator(t)
	bootstrap, err := arkchannel.NewService(
		arkchannel.PartyClient, coordinator,
		&statusPollBootstrapExecutor{},
	)
	require.NoError(t, err)
	_, err = bootstrap.RegisterPromotion(t.Context(), terms)
	require.NoError(t, err)
	_, err = bootstrap.RecordPreparedOOR(t.Context(), terms.ID, source)
	require.NoError(t, err)
	backing := statusPollBacking(t, terms, source, clientKey, hubKey)
	_, err = bootstrap.RecordLocalEvent(
		t.Context(), terms.ID, &arkchannel.BackingSigned{
			Backing: backing,
		},
	)
	require.NoError(t, err)
	_, err = bootstrap.RecordLocalEvent(
		t.Context(), terms.ID, &arkchannel.FundingFinalized{
			Party: arkchannel.PartyClient,
		},
	)
	require.NoError(t, err)
	_, err = bootstrap.RecordPeerEvent(
		t.Context(), terms.ID, &arkchannel.FundingFinalized{
			Party: arkchannel.PartyHub,
		},
	)
	require.NoError(t, err)
	_, err = bootstrap.RecordLocalEvent(
		t.Context(), terms.ID, &arkchannel.OORFinalized{
			SessionID: source.OORSessionID,
		},
	)
	require.NoError(t, err)
	beforeRestart, err := bootstrap.RecordLocalEvent(
		t.Context(), terms.ID, &arkchannel.RecoveryPackageInstalled{
			Party: arkchannel.PartyClient,
		},
	)
	require.NoError(t, err)
	require.True(t, beforeRestart.Snapshot.ClientRecoveryReady)
	require.False(t, beforeRestart.Snapshot.HubRecoveryReady)
	require.Equal(
		t, arkchannel.PhaseBackingReady, beforeRestart.Snapshot.Phase,
	)

	backend := newStatusPollFundingBackend(
		t, terms.PendingChannelID, terms.Capacity,
	)
	remote := &statusPollCounterparty{
		party:      arkchannel.PartyHub,
		key:        hubKey,
		readyAfter: 1,
	}
	recovery := &statusPollRecoveryManager{}
	service, edges := statusPollService(
		t, coordinator, terms, clientKey, backend, remote, recovery,
	)
	remote.localService = service

	record, err := service.ResumeChannelAction(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, int32(1), recovery.exports.Load())
	require.Equal(t, int32(1), recovery.installs.Load())
	require.Equal(t, 1, remote.recoveryInstallCount())
	require.True(t, record.Snapshot.RecoveryReady())
	require.Equal(t, arkchannel.PhaseActivating, record.Snapshot.Phase)
	require.Equal(t, int32(1), edges.activations.Load())
}

// TestReceiveClientResumesRecoveryFromRemoteFunder proves the non-initiating
// client executes its durable recovery action after a hub-funded OOR commits.
func TestReceiveClientResumesRecoveryFromRemoteFunder(t *testing.T) {
	t.Parallel()

	terms, _, clientKey, hubKey := statusPollChannel(t)
	terms.Kind = arkchannel.KindReceiveIntent
	terms.Funder = arkchannel.PartyHub
	terms.PaymentHash = [32]byte{9, 4, 2}
	terms.ID = arkchannel.ReceiveIntentID(terms.PaymentHash)
	terms.PendingChannelID = arkchannel.ReceiveIntentPendingID(
		terms.PaymentHash,
	)
	source := *testIntentBinding(
		t, terms, terms.Capacity+arkchannel.DefaultBackingFee, 0,
	)
	backing := statusPollBacking(t, terms, source, clientKey, hubKey)
	coordinator := statusPollCoordinator(t)
	bootstrap, err := arkchannel.NewService(
		arkchannel.PartyClient, coordinator,
		&statusPollBootstrapExecutor{},
	)
	require.NoError(t, err)
	_, err = bootstrap.RegisterReceiveIntent(t.Context(), terms)
	require.NoError(t, err)
	_, err = bootstrap.RecordPeerEvent(
		t.Context(), terms.ID, &arkchannel.BindVTXO{
			Binding: source,
		},
	)
	require.NoError(t, err)
	_, err = bootstrap.RecordLocalEvent(
		t.Context(), terms.ID, &arkchannel.FundingPeerReady{},
	)
	require.NoError(t, err)
	_, err = bootstrap.RecordPeerEvent(
		t.Context(), terms.ID, &arkchannel.BackingSigned{
			Backing: backing,
		},
	)
	require.NoError(t, err)
	_, err = bootstrap.RecordLocalEvent(
		t.Context(), terms.ID, &arkchannel.FundingFinalized{
			Party: arkchannel.PartyClient,
		},
	)
	require.NoError(t, err)
	for _, event := range []arkchannel.Event{
		&arkchannel.FundingFinalized{
			Party: arkchannel.PartyHub,
		},
		&arkchannel.OORFinalized{
			SessionID: source.OORSessionID,
		},
		&arkchannel.RecoveryPackageInstalled{
			Party: arkchannel.PartyHub,
		},
	} {
		_, err = bootstrap.RecordPeerEvent(t.Context(), terms.ID, event)
		require.NoError(t, err)
	}

	backend := newStatusPollFundingBackend(
		t, terms.PendingChannelID, terms.Capacity,
	)
	remote := &statusPollCounterparty{
		party: arkchannel.PartyHub, key: hubKey, readyAfter: 1,
	}
	recovery := &statusPollRecoveryManager{}
	service, edges := statusPollService(
		t, coordinator, terms, clientKey, backend, remote, recovery,
	)
	remote.localService = service

	record, err := service.ResumeChannelAction(t.Context(), terms.ID)
	require.NoError(t, err)
	require.Equal(t, 1, remote.recoveryExportCount())
	require.Zero(t, recovery.exports.Load())
	require.Equal(t, int32(1), recovery.installs.Load())
	require.True(t, record.Snapshot.RecoveryReady())
	require.Equal(t, arkchannel.PhaseActivating, record.Snapshot.Phase)
	require.Equal(t, int32(1), edges.activations.Load())
}

// statusPollService composes the production service and native action executor.
func statusPollService(t *testing.T, coordinator *arkchannel.Coordinator,
	terms arkchannel.Terms, localKey *btcec.PrivateKey,
	backend *statusPollFundingBackend, remote *statusPollCounterparty,
	recovery *statusPollRecoveryManager) (*arkchannel.Service,
	*statusPollNativeEdges) {

	t.Helper()

	endpoint, err := NewNativeFundingEndpoint(
		arkchannel.PartyClient, backend,
		input.NewMockSigner(
			[]*btcec.PrivateKey{localKey}, nil,
		),
		keychain.KeyDescriptor{
			PubKey: localKey.PubKey(),
		},
	)
	require.NoError(t, err)
	negotiator, err := NewChannelNegotiator(
		endpoint, remote, &Peer{}, recovery,
	)
	require.NoError(t, err)
	oor := &statusPollOORController{}
	edges := &statusPollNativeEdges{backend: backend}
	executor, err := arkchannel.NewNativeExecutor(
		arkchannel.PartyClient, edges, negotiator, oor, nil, nil, edges,
		edges, edges,
	)
	require.NoError(t, err)
	service, err := arkchannel.NewService(
		arkchannel.PartyClient, coordinator, executor,
	)
	require.NoError(t, err)

	return service, edges
}

// statusPollCoordinator creates a persistent coordinator for restart tests.
func statusPollCoordinator(t *testing.T) *arkchannel.Coordinator {
	t.Helper()

	rawStore := clientdb.NewTestDB(t)
	store := clientdb.NewStore(
		rawStore.DB, rawStore.Queries, rawStore.Backend(), btclog.Disabled,
	).NewArkChannelStore(clock.NewDefaultClock())
	coordinator, err := arkchannel.NewCoordinator(store)
	require.NoError(t, err)

	return coordinator
}

// statusPollChannel creates valid promotion terms and their exact OOR binding.
func statusPollChannel(t *testing.T) (arkchannel.Terms, arkchannel.VTXOBinding,
	*btcec.PrivateKey, *btcec.PrivateKey) {

	t.Helper()

	clientKey := statusPollKey(t)
	hubKey := statusPollKey(t)
	terms := arkchannel.Terms{
		ID: arkchannel.ID{
			7,
			1,
			4,
		},
		Kind:   arkchannel.KindPromotion,
		Funder: arkchannel.PartyClient,
		PendingChannelID: [32]byte{
			8,
			2,
			5,
		},
		ReservedSCID: lnwire.ShortChannelID{
			BlockHeight: 16_000_100,
			TxIndex:     4,
		}.ToUint64(),
		Capacity:      200_000,
		ClientNodeKey: statusPollCompressedKey(clientKey),
		HubNodeKey:    statusPollCompressedKey(hubKey),
		VTXO: arkchannel.VTXOTerms{
			ClientArkKey: statusPollCompressedKey(
				statusPollKey(t),
			),
			HubArkKey: statusPollCompressedKey(
				statusPollKey(t),
			),
			ArkOperatorKey: statusPollCompressedKey(
				statusPollKey(t),
			),
			ClientChannelKey: statusPollCompressedKey(clientKey),
			HubChannelKey:    statusPollCompressedKey(hubKey),
			FunderKey: statusPollCompressedKey(
				statusPollKey(t),
			),
			ChannelDelay: 144,
			FunderDelay:  576,
			MinExitDelay: 144,
		},
	}
	source := testIntentBinding(
		t, terms, terms.Capacity+1_000, 0,
	)

	return terms, *source, clientKey, hubKey
}

// statusPollBacking creates the signed backing stored before recovery resumes.
func statusPollBacking(t *testing.T, terms arkchannel.Terms,
	source arkchannel.VTXOBinding,
	clientKey, hubKey *btcec.PrivateKey) arkchannel.Backing {

	t.Helper()

	backend := newStatusPollFundingBackend(
		t, terms.PendingChannelID, terms.Capacity,
	)
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(backend.psbt), false,
	)
	require.NoError(t, err)
	template, err := arkchannel.NewBackingTemplate(packet, terms, source)
	require.NoError(t, err)
	sign := func(party arkchannel.Party,
		key *btcec.PrivateKey) input.Signature {

		desc, err := template.SignDescriptor(
			terms, party, keychain.KeyDescriptor{
				PubKey: key.PubKey(),
			},
		)
		require.NoError(t, err)
		signature, err := input.NewMockSigner(
			[]*btcec.PrivateKey{key}, nil,
		).SignOutputRaw(template.Packet().UnsignedTx, desc)
		require.NoError(t, err)

		return signature
	}
	backing, err := template.Complete(
		terms, source, sign(arkchannel.PartyClient, clientKey),
		sign(arkchannel.PartyHub, hubKey),
	)
	require.NoError(t, err)

	return backing
}

// statusPollKey creates one channel or policy key.
func statusPollKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return key
}

// statusPollCompressedKey serializes one key for durable channel terms.
func statusPollCompressedKey(key *btcec.PrivateKey) [33]byte {
	var serialized [33]byte
	copy(serialized[:], key.PubKey().SerializeCompressed())

	return serialized
}

var (
	_ NativeFundingBackend       = (*statusPollFundingBackend)(nil)
	_ RecoveryCounterparty       = (*statusPollCounterparty)(nil)
	_ RecoveryExportCounterparty = (*statusPollCounterparty)(nil)
	_ ChannelRecoveryManager     = (*statusPollRecoveryManager)(nil)
)
