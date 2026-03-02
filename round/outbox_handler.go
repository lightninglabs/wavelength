package round

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/types"
)

// OutboxRequest is a sealed marker interface for outbox messages that
// represent I/O side-effect requests. These are emitted by FSM
// transitions and handled by the OutboxHandler, which performs the
// actual I/O and returns follow-up ClientEvents to feed back into the
// FSM. Non-request outbox messages (server messages, notifications)
// continue through the existing processOutbox path.
type OutboxRequest interface {
	ClientOutMsg

	// outboxRequestSealed prevents external implementations,
	// ensuring only this package can define request types.
	outboxRequestSealed()
}

// OutboxHandler processes OutboxRequest messages emitted by the FSM.
// Each request represents an I/O side effect (persistence, signing,
// key derivation) that was previously performed inline during state
// transitions. The handler performs the I/O and returns follow-up
// ClientEvent(s) that the event pump feeds back into the FSM.
type OutboxHandler interface {
	// Handle processes an OutboxRequest and returns zero or more
	// follow-up events. The caller feeds these events back into the
	// FSM via the askAndDrive event pump. An error return indicates
	// a fatal handler failure.
	Handle(ctx context.Context,
		msg OutboxRequest) ([]ClientEvent, error)
}

// InProcessOutboxHandler performs I/O side effects in the same process
// using the provided stores and wallet. This is the default handler
// wired by the actor; tests may substitute a mock.
type InProcessOutboxHandler struct {
	// RoundStore provides round checkpoint persistence.
	RoundStore RoundStore

	// VTXOStore provides VTXO persistence and lookup.
	VTXOStore VTXOStore

	// Wallet provides signing and key derivation.
	Wallet ClientWallet

	// QueryBestHeight returns the current chain tip height.
	QueryBestHeight func(context.Context) (uint32, error)

	// OperatorTerms contains the operator's parameters.
	OperatorTerms *types.OperatorTerms

	// ChainParams are the Bitcoin network parameters.
	ChainParams *chaincfg.Params

	// MaxOperatorFee is the maximum acceptable operator fee.
	MaxOperatorFee btcutil.Amount

	// DisableJoinRequestAuth skips BIP-322 join authorization in
	// tests.
	DisableJoinRequestAuth bool

	// Log is the logger for handler operations.
	Log btclog.Logger
}

// SaveVTXOsReq requests persistence of newly created VTXOs after a
// round's commitment transaction confirms. The handler calls
// VTXOStore.SaveVTXOs and returns SaveVTXOsSucceeded or
// SaveVTXOsFailed.
type SaveVTXOsReq struct {
	// VTXOs are the client VTXOs to persist.
	VTXOs []*ClientVTXO
}

func (r *SaveVTXOsReq) clientOutMsgSealed()  {}
func (r *SaveVTXOsReq) outboxRequestSealed() {}

// CommitRoundStateReq requests atomic persistence of round data and
// FSM state at the "point of no return". The handler calls
// RoundStore.CommitState and returns CommitRoundStateSucceeded or
// CommitRoundStateFailed.
type CommitRoundStateReq struct {
	// Round is the round data to persist.
	Round *Round

	// State is the FSM state to persist alongside the round.
	State ClientState
}

func (r *CommitRoundStateReq) clientOutMsgSealed()  {}
func (r *CommitRoundStateReq) outboxRequestSealed() {}

// BuildRegistrationReq requests construction of the JoinRoundRequest
// including forfeit amount computation, amount validation, key
// derivation, and BIP-322 authorization. The handler performs all
// I/O (VTXOStore lookups, Wallet key derivation, Wallet signing,
// QueryBestHeight) and returns BuildRegistrationSucceeded or
// BuildRegistrationFailed.
type BuildRegistrationReq struct {
	// Boarding are the confirmed boarding intents.
	Boarding []BoardingIntent

	// VTXOs are the VTXO output requests.
	VTXOs []types.VTXORequest

	// Forfeits are the forfeit input requests.
	Forfeits []types.ForfeitRequest

	// Leaves are the on-chain exit output requests.
	Leaves []*types.LeaveRequest
}

func (r *BuildRegistrationReq) clientOutMsgSealed()  {}
func (r *BuildRegistrationReq) outboxRequestSealed() {}

// SignBoardingInputsReq requests signing of all boarding inputs in the
// commitment transaction. The handler calls signBoardingInputs with
// the Wallet and returns SignBoardingInputsSucceeded or
// SignBoardingInputsFailed.
type SignBoardingInputsReq struct {
	// CommitmentTx is the PSBT containing the boarding inputs.
	CommitmentTx *psbt.Packet

	// Intents are the round intents containing boarding info.
	Intents Intents

	// BoardingInputIndices maps each boarding outpoint to its
	// position in the commitment tx inputs.
	BoardingInputIndices map[wire.OutPoint]int
}

func (r *SignBoardingInputsReq) clientOutMsgSealed()  {}
func (r *SignBoardingInputsReq) outboxRequestSealed() {}

// Compile-time assertion that InProcessOutboxHandler implements
// OutboxHandler.
var _ OutboxHandler = (*InProcessOutboxHandler)(nil)

// Handle dispatches an OutboxRequest to the appropriate handler
// method. Unrecognized request types return an error.
func (h *InProcessOutboxHandler) Handle(ctx context.Context,
	msg OutboxRequest) ([]ClientEvent, error) {

	switch req := msg.(type) {
	case *SaveVTXOsReq:
		return h.handleSaveVTXOs(ctx, req)

	case *CommitRoundStateReq:
		return h.handleCommitRoundState(ctx, req)

	case *SignBoardingInputsReq:
		return h.handleSignBoardingInputs(ctx, req)

	default:
		return nil, fmt.Errorf("unhandled outbox request "+
			"type: %T", msg)
	}
}

// handleSaveVTXOs persists VTXOs via VTXOStore and returns the
// appropriate follow-up event.
func (h *InProcessOutboxHandler) handleSaveVTXOs(ctx context.Context,
	req *SaveVTXOsReq) ([]ClientEvent, error) {

	if err := h.VTXOStore.SaveVTXOs(ctx, req.VTXOs); err != nil {
		return []ClientEvent{
			&SaveVTXOsFailed{Error: err},
		}, nil
	}

	return []ClientEvent{&SaveVTXOsSucceeded{}}, nil
}

// handleCommitRoundState persists round data and FSM state atomically
// via RoundStore.CommitState.
func (h *InProcessOutboxHandler) handleCommitRoundState(
	ctx context.Context,
	req *CommitRoundStateReq) ([]ClientEvent, error) {

	if err := h.RoundStore.CommitState(
		ctx, req.Round, req.State,
	); err != nil {
		return []ClientEvent{
			&CommitRoundStateFailed{Error: err},
		}, nil
	}

	return []ClientEvent{&CommitRoundStateSucceeded{}}, nil
}

// handleSignBoardingInputs signs all boarding inputs in the
// commitment transaction using the wallet and returns the
// appropriate follow-up event.
func (h *InProcessOutboxHandler) handleSignBoardingInputs(
	_ context.Context,
	req *SignBoardingInputsReq) ([]ClientEvent, error) {

	sigs, err := signBoardingInputs(
		h.Wallet, req.CommitmentTx, req.Intents,
		req.BoardingInputIndices,
	)
	if err != nil {
		return []ClientEvent{
			&SignBoardingInputsFailed{Error: err},
		}, nil
	}

	return []ClientEvent{
		&SignBoardingInputsSucceeded{InputSigs: sigs},
	}, nil
}

// signBoardingInputs signs all boarding inputs for a commitment
// transaction. It builds the PrevOutputFetcher, sigHashes, and
// generates Schnorr signatures for each boarding intent's input.
func signBoardingInputs(wallet ClientWallet,
	commitmentTx *psbt.Packet, intents Intents,
	boardingInputIndices map[wire.OutPoint]int,
) ([]*types.BoardingInputSignature, error) {

	tx := commitmentTx.UnsignedTx

	// Build a PrevOutputFetcher from ALL PSBT inputs. Taproot
	// sighash (BIP341) requires prevout info for all inputs.
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	for i, pIn := range commitmentTx.Inputs {
		if pIn.WitnessUtxo == nil {
			return nil, fmt.Errorf("PSBT input %d "+
				"missing WitnessUtxo", i)
		}
		outpoint := tx.TxIn[i].PreviousOutPoint
		prevOuts[outpoint] = pIn.WitnessUtxo
	}
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

	// Build structured boarding input signatures for each
	// intent.
	var boardingInputSigs []*types.BoardingInputSignature
	for _, boardingIntent := range intents.Boarding {
		outpoint := boardingIntent.Request.Outpoint
		inputIdx, found := boardingInputIndices[*outpoint]
		if !found {
			return nil, fmt.Errorf("no input index "+
				"found for boarding outpoint %s",
				outpoint)
		}

		spendInfo, err := scripts.NewVTXOSpendInfo(
			boardingIntent.Address.Tapscript,
			scripts.VTXOCollabPathLeaf,
		)
		if err != nil {
			return nil, err
		}

		chainInfo := boardingIntent.ChainInfo
		addr := boardingIntent.Address.Address
		amt := chainInfo.Amount

		// Use PayToAddrScript to get the full pkScript
		// with OP_1 OP_PUSHBYTES_32 prefix for P2TR
		// addresses. ScriptAddress() only returns the
		// 32-byte witness program.
		pkScript, err := txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, fmt.Errorf(
				"pay to addr script: %w", err,
			)
		}

		output := &wire.TxOut{
			Value:    int64(amt),
			PkScript: pkScript,
		}

		signature, err := scripts.SignVTXOCollabInput(
			wallet, tx, inputIdx, spendInfo,
			&boardingIntent.Address.KeyDesc, output,
			sigHashes, prevOutFetcher,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to sign "+
				"boarding input %d: %w",
				inputIdx, err)
		}

		schnorrSig, ok := signature.(*schnorr.Signature)
		if !ok {
			return nil, fmt.Errorf("signature is not " +
				"a schnorr signature")
		}

		inputSig := &types.BoardingInputSignature{
			InputIndex:      inputIdx,
			Outpoint:        *outpoint,
			ClientSignature: schnorrSig,
		}
		boardingInputSigs = append(
			boardingInputSigs, inputSig,
		)
	}

	return boardingInputSigs, nil
}
