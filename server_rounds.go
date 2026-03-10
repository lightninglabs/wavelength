package darepo

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo-client/rpc/roundpb"
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/batch"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightninglabs/darepo/lndbackend"
	"github.com/lightninglabs/darepo/rounds"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/protobuf/proto"
)

// setupRoundsSubsystem initializes the timeout actor, batch watcher,
// and rounds actor. The rounds actor drives the round FSM lifecycle:
// registration, signing, broadcast, and confirmation. The batch
// watcher monitors confirmed batch transactions for tree-level events.
//
// This must be called after the indexer subsystem (step 5) so that the
// shared bridge and DB stores are available. The resulting actor
// references are stored on the Server for use by admin RPC handlers
// and dispatcher wiring.
func (s *Server) setupRoundsSubsystem(ctx context.Context) error {
	chainParams, err := networkToChainParams(s.cfg.Network)
	if err != nil {
		return fmt.Errorf("resolve chain params: %w", err)
	}

	// Register the shared timeout actor that provides wall-clock
	// timer scheduling for round phase deadlines.
	timeoutActor := timeout.NewActor()
	s.timeoutRef = actor.RegisterWithSystem(
		s.actorSystem, "timeout",
		actor.NewServiceKey[timeout.Msg, timeout.Resp](
			"timeout",
		),
		timeoutActor,
	)

	// Build DB-backed stores for rounds and VTXOs using the
	// shared db.Store to avoid redundant wrappers.
	roundsLog := subLogger(s.cfg.Loggers, rounds.Subsystem)

	roundStore := s.db.NewRoundStore()
	vtxoStore := s.db.NewVTXOStore()

	// Create the LND-backed wallet controller for PSBT funding and
	// signing.
	walletCtrl := lndbackend.NewLndWalletController(
		s.lnd.WalletKit, s.lnd.Signer,
		fn.Some(roundsLog),
	)

	// Use a static floor fee estimator. A future config phase can
	// wire the real LND fee estimator.
	feeEstimator := chainfee.NewStaticEstimator(
		chainfee.FeePerKwFloor, 0,
	)

	// Create and spawn the batch watcher actor for monitoring
	// confirmed batches on-chain. Use ServiceKey.Spawn so we
	// can set SelfRef on the config before the actor processes
	// any messages.
	bwLog := subLogger(s.cfg.Loggers, batchwatcher.Subsystem)
	batchWatcherCfg := &batchwatcher.ActorConfig{
		Log:         fn.Some(bwLog),
		ChainSource: s.chainSourceRef,
	}
	batchWatcher := batchwatcher.NewActor(batchWatcherCfg)
	bwKey := batchwatcher.NewServiceKey()
	s.batchWatcherRef = bwKey.Spawn(
		s.actorSystem,
		batchwatcher.BatchWatcherServiceKeyName,
		batchWatcher,
	)

	// Set SelfRef before the actor processes any messages,
	// needed for callback mapping in the batch watcher.
	batchWatcherCfg.SelfRef = s.batchWatcherRef

	// Derive operator and sweep keys from LND for the batch
	// terms. Both use the multi-sig key family so they are
	// backed by real on-chain keys.
	operatorKeyDesc, err := s.lnd.WalletKit.DeriveNextKey(
		ctx, int32(keychain.KeyFamilyMultiSig),
	)
	if err != nil {
		return fmt.Errorf("derive operator key: %w", err)
	}

	sweepKeyDesc, err := s.lnd.WalletKit.DeriveNextKey(
		ctx, int32(keychain.KeyFamilyMultiSig),
	)
	if err != nil {
		return fmt.Errorf("derive sweep key: %w", err)
	}

	// Build a taproot connector address from the operator key.
	outputKey := txscript.ComputeTaprootOutputKey(
		operatorKeyDesc.PubKey, nil,
	)
	connectorAddr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(outputKey), chainParams,
	)
	if err != nil {
		return fmt.Errorf("create connector address: %w",
			err)
	}

	// Start with config-derived terms and overlay the LND-derived
	// key fields.
	rc := s.cfg.Rounds
	terms := roundsTermsFromConfig(rc)
	terms.OperatorKey = *operatorKeyDesc
	terms.SweepKey = *sweepKeyDesc
	terms.ConnectorAddress = connectorAddr

	roundsCfg := &rounds.ActorConfig{
		ChainParams:        chainParams,
		Log:                fn.Some(roundsLog),
		Terms:              terms,
		ClientsConn:        s.clientBridge,
		ChainSourceActor:   s.chainSourceRef,
		TimeoutActor:       s.timeoutRef,
		RoundStore:         roundStore,
		VTXOStore:          vtxoStore,
		VTXOLocker:         s.vtxoLocker,
		WalletController:   walletCtrl,
		FeeEstimator:       feeEstimator,
		WalletAccount:      "",
		ConfTarget:         rc.ConfTarget,
		MinConfs:           rc.MinConfs,
		ConfirmationTarget: rc.ConfirmationTarget,
		BatchWatcher:       fn.Some(s.batchWatcherRef),
	}

	// Create and spawn the rounds actor.
	s.roundsActor = rounds.NewActor(roundsCfg)
	roundsKey := actor.NewServiceKey[
		rounds.ActorMsg, rounds.ActorResp,
	]("rounds-actor")
	s.roundsRef = roundsKey.Spawn(
		s.actorSystem, "rounds-actor", s.roundsActor,
	)

	// Set SelfRef on config after spawning (needed for timeout
	// callback mapping). ActorRef embeds TellOnlyRef, so we can
	// assign directly.
	roundsCfg.SelfRef = s.roundsRef

	// Start the rounds actor: loads pending rounds from storage
	// and creates a new live round accepting registrations.
	if err := s.roundsActor.Start(ctx); err != nil {
		return fmt.Errorf("start rounds actor: %w", err)
	}

	// Register fire-and-forget dispatch routes for all round
	// RPCs on the shared event router. Each route deserializes
	// the envelope body, extracts the client ID from the
	// envelope sender, converts the proto to a domain actor
	// message, and Tell's the rounds actor.
	s.registerRoundRoutes(roundsKey)

	s.log.InfoS(ctx, "Rounds subsystem initialized",
		slog.Uint64("conf_target",
			uint64(rc.ConfTarget)))

	return nil
}

// stopRoundsSubsystem releases rounds-related resources. The actor
// system's Shutdown handles actor lifecycle; this method handles any
// additional cleanup.
func (s *Server) stopRoundsSubsystem(ctx context.Context) {
	if s.roundsActor != nil {
		s.log.InfoS(ctx, "Rounds subsystem stopped")
	}
}

// roundsTermsFromConfig maps a RoundsConfig into a batch.Terms
// struct. Key-dependent fields (OperatorKey, SweepKey,
// ConnectorAddress) are left at their zero values.
func roundsTermsFromConfig(rc *RoundsConfig) *batch.Terms {
	return &batch.Terms{
		SweepDelay:                    rc.SweepDelay,
		MaxVTXOsPerTree:               rc.MaxVTXOsPerTree,
		TreeRadix:                     rc.TreeRadix,
		MaxConnectorsPerTree:          rc.MaxConnectorsPerTree,
		BoardingExitDelay:             rc.BoardingExitDelay,
		VTXOExitDelay:                 rc.VTXOExitDelay,
		RegistrationTimeout:           rc.RegistrationTimeout,
		FundPsbtLockDuration:          rc.FundPsbtLockDuration,
		BoardingExitDelaySafetyMargin: rc.BoardingExitDelaySafetyMargin,
		MinBoardingConfirmations:      rc.MinBoardingConfirmations,
		SignatureCollectionTimeout:    rc.SignatureCollectionTimeout,
	}
}

// registerRoundRoutes adds fire-and-forget dispatch routes for all
// five round RPCs to the server's shared EventRouter. Each route
// deserializes the envelope body into the expected proto type,
// extracts the client ID from the envelope sender, converts the
// proto to a domain actor message, and Tell's the rounds actor.
//
// This replaces the previous RoundOperator + ServeMux + Edge
// response pattern with the simpler AddEnvelopeRoute model. Since
// all round RPCs are fire-and-forget (the real response arrives
// asynchronously via the outbox event path), no response envelope
// needs to be built.
func (s *Server) registerRoundRoutes( //nolint:funlen
	roundsKey actor.ServiceKey[rounds.ActorMsg, rounds.ActorResp]) {

	svc := roundpb.ServiceName

	// JoinRound: client wants to join a round. This is the
	// only route that doesn't produce a RoundMsg wrapper,
	// since JoinRoundRequest is a top-level actor message.
	clientconn.AddEnvelopeRoute(
		s.eventRouter,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "JoinRound",
			NewEvent: func() proto.Message {
				return &roundpb.JoinRoundRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.JoinRoundRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T",
						p,
					)
				}

				domainReq, err :=
					rounds.JoinRoundRequestFromProto(
						req,
					)
				if err != nil {
					return nil, fmt.Errorf(
						"parse join request: %w",
						err,
					)
				}

				return &rounds.JoinRoundRequest{
					ClientID: clientconn.ClientID(
						env.Sender,
					),
					Request: domainReq,
				}, nil
			},
		},
	)

	// SubmitNonces: client submits MuSig2 public nonces for a
	// round's VTXO tree transactions.
	clientconn.AddEnvelopeRoute(
		s.eventRouter,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitNonces",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitNoncesRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitNoncesRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T",
						p,
					)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf(
						"parse round_id: %w", err,
					)
				}

				nonces, err := rounds.NoncesFromProto(
					req.GetNonces(),
				)
				if err != nil {
					return nil, fmt.Errorf(
						"parse nonces: %w", err,
					)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientVTXONoncesEvent{ //nolint:ll
						ClientID: cID,
						Nonces:   nonces,
					},
				}, nil
			},
		},
	)

	// SubmitPartialSigs: client submits MuSig2 partial
	// signatures for a round's VTXO tree transactions.
	clientconn.AddEnvelopeRoute(
		s.eventRouter,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitPartialSigs",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitPartialSigRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitPartialSigRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T",
						p,
					)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf(
						"parse round_id: %w", err,
					)
				}

				sigs, err := rounds.PartialSigsFromProto(
					req.GetSignatures(),
				)
				if err != nil {
					return nil, fmt.Errorf(
						"parse signatures: %w",
						err,
					)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientVTXOPartialSigsEvent{ //nolint:ll
						ClientID:   cID,
						Signatures: sigs,
					},
				}, nil
			},
		},
	)

	// SubmitForfeitSigs: client submits boarding input
	// signatures (Schnorr) for on-chain inputs in a round.
	clientconn.AddEnvelopeRoute(
		s.eventRouter,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitForfeitSigs",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitForfeitSigRequest{}
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitForfeitSigRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T",
						p,
					)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf(
						"parse round_id: %w", err,
					)
				}

				boardingSigs, err :=
					rounds.BoardingInputSigsFromProto(
						req.GetSignatures(),
					)
				if err != nil {
					return nil, fmt.Errorf(
						"parse boarding sigs: %w",
						err,
					)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientInputSignaturesEvent{ //nolint:ll
						ClientID:   cID,
						Signatures: boardingSigs,
					},
				}, nil
			},
		},
	)

	// SubmitVTXOForfeitSigs: client submits VTXO forfeit
	// transaction signatures for cooperative spend paths.
	clientconn.AddEnvelopeRoute(
		s.eventRouter,
		clientconn.EnvelopeRouteConfig[
			rounds.ActorMsg, rounds.ActorResp,
		]{
			Service: svc,
			Method:  "SubmitVTXOForfeitSigs",
			NewEvent: func() proto.Message {
				return &roundpb.SubmitVTXOForfeitSigsRequest{} //nolint:ll
			},
			Key: roundsKey,
			Adapt: func(env *mailboxpb.Envelope,
				p proto.Message) (
				rounds.ActorMsg, error) {

				req, ok := p.(*roundpb.SubmitVTXOForfeitSigsRequest) //nolint:ll
				if !ok {
					return nil, fmt.Errorf(
						"unexpected type %T",
						p,
					)
				}

				roundID, err := rounds.ParseRoundID(
					req.GetRoundId(),
				)
				if err != nil {
					return nil, fmt.Errorf(
						"parse round_id: %w", err,
					)
				}

				forfeitTxs, err :=
					rounds.ForfeitTxSigsFromProto(
						req.GetForfeitTxs(),
					)
				if err != nil {
					return nil, fmt.Errorf(
						"parse forfeit sigs: %w",
						err,
					)
				}

				cID := clientconn.ClientID(env.Sender)

				return &rounds.RoundMsg{
					RoundID: roundID,
					Event: &rounds.ClientInputSignaturesEvent{ //nolint:ll
						ClientID:   cID,
						ForfeitTxs: forfeitTxs,
					},
				}, nil
			},
		},
	)
}

// networkToChainParams maps a network name to btcd chain parameters.
func networkToChainParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, nil

	case "testnet":
		return &chaincfg.TestNet3Params, nil

	case "regtest":
		return &chaincfg.RegressionNetParams, nil

	case "simnet":
		return &chaincfg.SimNetParams, nil

	case "signet":
		return &chaincfg.SigNetParams, nil

	default:
		return nil, fmt.Errorf("unknown network %q", network)
	}
}
