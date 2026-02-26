package sdk

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	clienttree "github.com/lightninglabs/darepo-client/lib/tree"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	clientoor "github.com/lightninglabs/darepo-client/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// IncomingTransfer is a wallet-targeted incoming transfer payload fetched from
// the operator side (for now via a polling adapter owned by the caller).
type IncomingTransfer struct {
	SessionID clientoor.SessionID
	ArkPSBT   *psbt.Packet
}

// VTXOStateStore is the VTXO persistence contract needed by SDK transfer
// flows.
type VTXOStateStore interface {
	ListLiveVTXOs(ctx context.Context) ([]*vtxo.Descriptor, error)
	GetVTXO(ctx context.Context, outpoint wire.OutPoint) (
		*vtxo.Descriptor, error)
	SaveVTXO(ctx context.Context, desc *vtxo.Descriptor) error
}

// Config wires the embedded SDK client to a running local runtime.
//
// The runtime ownership model is explicit: the caller owns actor startup and
// connector lifecycle; the SDK owns only its local OOR actor.
type Config struct {
	// RequestRoundOutputs forwards output registration to the local round
	// runtime.
	RequestRoundOutputs func(ctx context.Context,
		amounts []btcutil.Amount) error

	// TriggerRoundJoin asks the local round runtime to emit a join request.
	TriggerRoundJoin func(ctx context.Context) error

	// LastCompletedRoundID returns the latest local confirmed round ID.
	LastCompletedRoundID func(ctx context.Context) (string, error)

	// RPCClient is the mailbox unary RPC client bound to the operator.
	RPCClient mailboxrpc.RPCClient

	// Signer signs checkpoint inputs for outgoing OOR finalize.
	Signer input.Signer

	// SpendMarker marks local outgoing inputs spent after finalize.
	SpendMarker clientoor.InputSpendMarker

	// DeliveryStore backs the SDK-owned local OOR actor.
	DeliveryStore actor.DeliveryStore

	// VTXOStore is the local persisted VTXO state.
	VTXOStore VTXOStateStore

	// OperatorKey is the active operator key used in VTXO script
	// derivations.
	OperatorKey *btcec.PublicKey

	// DeriveReceiveKey derives a fresh receive key for incoming OOR
	// outputs.
	DeriveReceiveKey func(ctx context.Context) (keychain.KeyDescriptor,
		error)

	// ReceiveAddressHRP is the bech32m human-readable prefix used for
	// SDK receive addresses.
	ReceiveAddressHRP string

	// ReceiveExitDelay is the unilateral CSV delay encoded in newly
	// generated receive addresses.
	ReceiveExitDelay uint32

	// FetchIncomingTransfers resolves incoming transfer payloads for one
	// recipient script.
	FetchIncomingTransfers func(ctx context.Context,
		recipientPkScript []byte) ([]IncomingTransfer, error)
}

// Client is a high-level embedded SDK façade over a running local runtime.
type Client struct {
	cfg Config

	oorActor *clientoor.OORClientActor

	receiveMu      sync.Mutex
	receiveTargets map[string]*receiveTarget
}

// receiveTarget keeps recipient-owned metadata for one generated
// receive address.
type receiveTarget struct {
	address string

	keyDesc keychain.KeyDescriptor

	pkScript  []byte
	exitDelay uint32

	processed map[clientoor.SessionID]struct{}
}

// New creates a new SDK client with runtime dependency injection.
func New(cfg Config) (*Client, error) {
	if cfg.VTXOStore == nil {
		return nil, fmt.Errorf("vtxo store is required")
	}
	if cfg.LastCompletedRoundID == nil {
		return nil, fmt.Errorf(
			"last completed round id callback is required",
		)
	}
	if cfg.TriggerRoundJoin == nil {
		return nil, fmt.Errorf("round join callback is required")
	}
	if cfg.RequestRoundOutputs == nil {
		return nil, fmt.Errorf(
			"round output registration callback is required",
		)
	}
	if cfg.RPCClient == nil {
		return nil, fmt.Errorf("rpc client is required")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("signer is required")
	}
	if cfg.SpendMarker == nil {
		return nil, fmt.Errorf("spend marker is required")
	}
	if cfg.DeliveryStore == nil {
		return nil, fmt.Errorf("delivery store is required")
	}
	if cfg.OperatorKey == nil {
		return nil, fmt.Errorf("operator key is required")
	}
	if cfg.DeriveReceiveKey == nil {
		return nil, fmt.Errorf("receive key deriver is required")
	}
	if cfg.ReceiveAddressHRP == "" {
		return nil, fmt.Errorf("receive address hrp is required")
	}
	if cfg.ReceiveExitDelay == 0 {
		return nil, fmt.Errorf("receive exit delay is required")
	}
	if cfg.FetchIncomingTransfers == nil {
		return nil, fmt.Errorf("incoming transfer fetcher is required")
	}

	return &Client{
		cfg:            cfg,
		receiveTargets: make(map[string]*receiveTarget),
	}, nil
}

const (
	receiveAddressVersionV1 = uint8(1)

	receiveAddressPayloadLen = 1 + 32 + 32 + 4
)

type decodedReceiveAddress struct {
	hrp string

	operatorKey  *btcec.PublicKey
	recipientKey *btcec.PublicKey

	exitDelay uint32
}

// Stop releases SDK-owned resources.
func (c *Client) Stop() {
	if c.oorActor != nil {
		c.oorActor.Stop()
		c.oorActor = nil
	}
}

// RequestRoundOutputs registers desired output amounts for the next round.
func (c *Client) RequestRoundOutputs(ctx context.Context,
	amounts []btcutil.Amount) error {

	return c.cfg.RequestRoundOutputs(ctx, amounts)
}

// JoinRound asks the local round runtime to emit a join request.
func (c *Client) JoinRound(ctx context.Context) error {
	return c.cfg.TriggerRoundJoin(ctx)
}

// CompletedRoundID returns the latest locally confirmed round ID.
func (c *Client) CompletedRoundID(ctx context.Context) (string, error) {
	return c.cfg.LastCompletedRoundID(ctx)
}

// LiveVTXOs returns all local live (non-terminal) VTXOs.
func (c *Client) LiveVTXOs(ctx context.Context) ([]*vtxo.Descriptor, error) {
	return c.cfg.VTXOStore.ListLiveVTXOs(ctx)
}

// LiveBalance returns the sum of local live VTXO amounts.
func (c *Client) LiveBalance(ctx context.Context) (btcutil.Amount, error) {
	live, err := c.LiveVTXOs(ctx)
	if err != nil {
		return 0, err
	}

	var total btcutil.Amount
	for i := range live {
		total += live[i].Amount
	}

	return total, nil
}

// NewReceiveAddress derives a fresh recipient address for incoming OOR
// transfers and tracks it for SyncIncoming.
func (c *Client) NewReceiveAddress(ctx context.Context) (string, error) {
	recipientKey, err := c.cfg.DeriveReceiveKey(ctx)
	if err != nil {
		return "", fmt.Errorf("derive receive key: %w", err)
	}
	if recipientKey.PubKey == nil {
		return "", fmt.Errorf("receive key is missing pubkey")
	}

	address, err := encodeReceiveAddress(
		c.cfg.ReceiveAddressHRP, c.cfg.OperatorKey,
		recipientKey.PubKey, c.cfg.ReceiveExitDelay,
	)
	if err != nil {
		return "", err
	}

	recipientPkScript, err := recipientVTXOPkScript(
		recipientKey.PubKey, c.cfg.OperatorKey, c.cfg.ReceiveExitDelay,
	)
	if err != nil {
		return "", err
	}

	scriptKey := hex.EncodeToString(recipientPkScript)

	c.receiveMu.Lock()
	defer c.receiveMu.Unlock()

	target := c.receiveTargets[scriptKey]
	if target == nil {
		target = &receiveTarget{
			processed: make(map[clientoor.SessionID]struct{}),
		}
		c.receiveTargets[scriptKey] = target
	}

	target.address = address
	target.keyDesc = recipientKey
	target.pkScript = append([]byte(nil), recipientPkScript...)
	target.exitDelay = c.cfg.ReceiveExitDelay

	return address, nil
}

// SyncIncoming fetches and materializes all unprocessed incoming OOR transfers
// addressed to locally generated receive addresses.
func (c *Client) SyncIncoming(ctx context.Context) (int, error) {
	targets := c.receiveTargetSnapshots()
	if len(targets) == 0 {
		return 0, nil
	}

	roundID, err := c.cfg.LastCompletedRoundID(ctx)
	if err != nil {
		return 0, fmt.Errorf("recipient completed round id: %w", err)
	}

	processed := 0

	for i := range targets {
		target := targets[i]

		incoming, err := c.cfg.FetchIncomingTransfers(
			ctx, target.pkScript,
		)
		if err != nil {
			return processed, fmt.Errorf("list incoming transfers "+
				"for address %s: %w", target.address, err)
		}

		for j := range incoming {
			transfer := incoming[j]
			if transfer.ArkPSBT == nil ||
				transfer.ArkPSBT.UnsignedTx == nil {

				return processed, fmt.Errorf(
					"incoming transfer missing ark "+
						"psbt for address %s",
					target.address,
				)
			}

			sessionID := transfer.SessionID
			if sessionID == (clientoor.SessionID{}) {
				sessionID = clientoor.SessionID(
					transfer.ArkPSBT.UnsignedTx.TxHash(),
				)
			}

			if c.isTransferProcessed(target.scriptKey, sessionID) {
				continue
			}

			err = c.materializeIncomingTransfer(
				ctx, target, roundID, sessionID,
				transfer.ArkPSBT,
			)
			if err != nil {
				return processed, err
			}

			c.markTransferProcessed(target.scriptKey, sessionID)
			processed++
		}
	}

	return processed, nil
}

// SendOORPayment performs a single-input outgoing OOR transfer to a recipient
// address. Recipient materialization is handled by the recipient runtime via
// SyncIncoming.
func (c *Client) SendOORPayment(ctx context.Context, recipientAddress string,
	amount btcutil.Amount) error {

	decodedAddress, err := decodeReceiveAddress(recipientAddress)
	if err != nil {
		return err
	}
	if decodedAddress.hrp != c.cfg.ReceiveAddressHRP {
		return fmt.Errorf("recipient address hrp mismatch: "+
			"expected=%s got=%s", c.cfg.ReceiveAddressHRP,
			decodedAddress.hrp)
	}
	expectedOperator := schnorr.SerializePubKey(c.cfg.OperatorKey)
	gotOperator := schnorr.SerializePubKey(decodedAddress.operatorKey)
	if !bytes.Equal(expectedOperator, gotOperator) {
		return fmt.Errorf("recipient address operator key mismatch")
	}
	if amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}

	if err := c.ensureOORActor(); err != nil {
		return err
	}

	senderLiveVTXOs, err := c.cfg.VTXOStore.ListLiveVTXOs(ctx)
	if err != nil {
		return fmt.Errorf("list sender live vtxos: %w", err)
	}
	if len(senderLiveVTXOs) == 0 {
		return fmt.Errorf("sender has no live vtxos")
	}

	input, err := selectSingleInput(senderLiveVTXOs, amount)
	if err != nil {
		return err
	}

	recipientPkScript, err := recipientVTXOPkScript(
		decodedAddress.recipientKey, decodedAddress.operatorKey,
		decodedAddress.exitDelay,
	)
	if err != nil {
		return err
	}

	startResp := c.oorActor.Receive(
		ctx, &clientoor.StartTransferRequest{
			Policy: scripts.CheckpointPolicy{
				OperatorKey: c.cfg.OperatorKey,
				CSVDelay:    input.RelativeExpiry,
			},
			Inputs: []clientoor.TransferInput{{
				VTXO:            input,
				OwnerLeafScript: []byte{txscript.OP_1},
			}},
			Recipients: []oortx.RecipientOutput{{
				PkScript: recipientPkScript,
				Value:    amount,
			}},
		},
	)
	if startResp.IsErr() {
		return fmt.Errorf("start oor transfer: %w", startResp.Err())
	}

	senderInput, err := c.cfg.VTXOStore.GetVTXO(ctx, input.Outpoint)
	if err != nil {
		return fmt.Errorf("load sender input after oor: %w", err)
	}
	if senderInput.Status != vtxo.VTXOStatusSpent {
		return fmt.Errorf("sender input not marked spent after oor")
	}

	return nil
}

func (c *Client) ensureOORActor() error {
	if c.oorActor != nil {
		return nil
	}

	c.oorActor = clientoor.NewOORClientActor(clientoor.ClientActorCfg{
		OutboxHandler: &clientoor.MailboxOutboxHandler{
			RPCClient:   c.cfg.RPCClient,
			Signer:      c.cfg.Signer,
			SpendMarker: c.cfg.SpendMarker,
		},
		DeliveryStore: c.cfg.DeliveryStore,
	})

	return nil
}

type receiveTargetSnapshot struct {
	scriptKey string
	address   string

	keyDesc keychain.KeyDescriptor

	pkScript  []byte
	exitDelay uint32
}

func (c *Client) receiveTargetSnapshots() []receiveTargetSnapshot {
	c.receiveMu.Lock()
	defer c.receiveMu.Unlock()

	targets := make([]receiveTargetSnapshot, 0, len(c.receiveTargets))
	for scriptKey, target := range c.receiveTargets {
		targets = append(targets, receiveTargetSnapshot{
			scriptKey: scriptKey,
			address:   target.address,
			keyDesc:   target.keyDesc,
			pkScript:  append([]byte(nil), target.pkScript...),
			exitDelay: target.exitDelay,
		})
	}

	return targets
}

func (c *Client) isTransferProcessed(scriptKey string,
	sessionID clientoor.SessionID) bool {

	c.receiveMu.Lock()
	defer c.receiveMu.Unlock()

	target := c.receiveTargets[scriptKey]
	if target == nil {
		return false
	}

	_, ok := target.processed[sessionID]

	return ok
}

func (c *Client) markTransferProcessed(scriptKey string,
	sessionID clientoor.SessionID) {

	c.receiveMu.Lock()
	defer c.receiveMu.Unlock()

	target := c.receiveTargets[scriptKey]
	if target == nil {
		return
	}

	if target.processed == nil {
		target.processed = make(map[clientoor.SessionID]struct{})
	}

	target.processed[sessionID] = struct{}{}
}

func (c *Client) materializeIncomingTransfer(
	ctx context.Context, target receiveTargetSnapshot,
	roundID string, sessionID clientoor.SessionID,
	arkPSBT *psbt.Packet) error {

	receiveSession, receiveOutbox, err := clientoor.DriveIncomingTransfer(
		ctx, sessionID, arkPSBT,
	)
	if err != nil {
		return fmt.Errorf("drive incoming transfer for address %s: %w",
			target.address, err)
	}

	incomingHandler := &incomingReceiveOutboxHandler{
		recipientKey: target.keyDesc,
		operatorKey:  c.cfg.OperatorKey,
		exitDelay:    target.exitDelay,
		roundID:      roundID,
		vtxoStore:    c.cfg.VTXOStore,
	}

	if err := driveOutboxToFSM(
		ctx, receiveSession.ID, receiveSession.FSM,
		incomingHandler, receiveOutbox,
	); err != nil {
		return fmt.Errorf("process incoming outbox for address %s: %w",
			target.address, err)
	}

	if len(incomingHandler.materialized) == 0 {
		return fmt.Errorf("incoming materialization produced no "+
			"vtxos for address %s", target.address)
	}

	return nil
}

func selectSingleInput(vtxos []*vtxo.Descriptor,
	amount btcutil.Amount) (*vtxo.Descriptor, error) {

	for i := range vtxos {
		desc := vtxos[i]
		if desc.Amount != amount {
			continue
		}

		if desc.ClientKey.PubKey == nil {
			return nil, fmt.Errorf("input client key is missing")
		}
		if desc.OperatorKey == nil {
			return nil, fmt.Errorf("input operator key is missing")
		}

		return desc, nil
	}

	return nil, fmt.Errorf(
		"no single live vtxo matches requested amount=%d", amount,
	)
}

func recipientVTXOPkScript(ownerKey, operatorKey *btcec.PublicKey,
	exitDelay uint32) ([]byte, error) {

	tapKey, err := scripts.VTXOTapKey(ownerKey, operatorKey, exitDelay)
	if err != nil {
		return nil, fmt.Errorf("derive recipient tap key: %w", err)
	}

	pkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return nil, fmt.Errorf("derive recipient pkScript: %w", err)
	}

	return pkScript, nil
}

func encodeReceiveAddress(hrp string, operatorKey,
	recipientKey *btcec.PublicKey, exitDelay uint32) (string, error) {

	if hrp == "" {
		return "", fmt.Errorf("receive address hrp is required")
	}
	if operatorKey == nil {
		return "", fmt.Errorf("operator key is required")
	}
	if recipientKey == nil {
		return "", fmt.Errorf("recipient key is required")
	}
	if exitDelay == 0 {
		return "", fmt.Errorf(
			"receive address exit delay must be positive",
		)
	}

	payload := make([]byte, 0, receiveAddressPayloadLen)
	payload = append(payload, receiveAddressVersionV1)
	payload = append(payload, schnorr.SerializePubKey(operatorKey)...)
	payload = append(payload, schnorr.SerializePubKey(recipientKey)...)

	delayBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(delayBytes, exitDelay)
	payload = append(payload, delayBytes...)

	addrData, err := bech32.ConvertBits(payload, 8, 5, true)
	if err != nil {
		return "", err
	}

	address, err := bech32.EncodeM(hrp, addrData)
	if err != nil {
		return "", err
	}

	return address, nil
}

func decodeReceiveAddress(address string) (*decodedReceiveAddress, error) {
	if address == "" {
		return nil, fmt.Errorf("recipient address is required")
	}

	hrp, addrData, err := bech32.DecodeNoLimit(address)
	if err != nil {
		return nil, fmt.Errorf("decode recipient address: %w", err)
	}

	payload, err := bech32.ConvertBits(addrData, 5, 8, false)
	if err != nil {
		return nil, fmt.Errorf(
			"decode recipient address payload: %w", err,
		)
	}

	if len(payload) != receiveAddressPayloadLen {
		return nil, fmt.Errorf("invalid recipient address payload "+
			"length: expected=%d got=%d", receiveAddressPayloadLen,
			len(payload))
	}

	version := payload[0]
	if version != receiveAddressVersionV1 {
		return nil, fmt.Errorf("unsupported recipient address "+
			"version: %d", version)
	}

	operatorKey, err := schnorr.ParsePubKey(payload[1:33])
	if err != nil {
		return nil, fmt.Errorf("parse recipient address operator "+
			"key: %w", err)
	}

	recipientKey, err := schnorr.ParsePubKey(payload[33:65])
	if err != nil {
		return nil, fmt.Errorf("parse recipient address recipient "+
			"key: %w", err)
	}

	exitDelay := binary.BigEndian.Uint32(payload[65:])
	if exitDelay == 0 {
		return nil, fmt.Errorf("recipient address exit delay must be " +
			"positive")
	}

	return &decodedReceiveAddress{
		hrp:          hrp,
		operatorKey:  operatorKey,
		recipientKey: recipientKey,
		exitDelay:    exitDelay,
	}, nil
}

type incomingReceiveOutboxHandler struct {
	recipientKey keychain.KeyDescriptor
	operatorKey  *btcec.PublicKey
	exitDelay    uint32
	roundID      string
	vtxoStore    VTXOStateStore

	materialized []*vtxo.Descriptor
}

func (h *incomingReceiveOutboxHandler) Handle(ctx context.Context,
	_ clientoor.SessionID,
	outbox clientoor.OutboxEvent) ([]clientoor.Event, error) {

	switch msg := outbox.(type) {
	case *clientoor.IncomingTransferNotification:
		return nil, nil

	case *clientoor.MaterializeIncomingVTXOsRequest:
		if msg.ArkPSBT == nil || msg.ArkPSBT.UnsignedTx == nil {
			return nil, fmt.Errorf("ark psbt must be provided")
		}
		if h.vtxoStore == nil {
			return nil, fmt.Errorf("vtxo store is required")
		}
		if h.roundID == "" {
			return nil, fmt.Errorf("round id is required")
		}

		arkTxid := msg.ArkPSBT.UnsignedTx.TxHash()

		for i := range msg.Recipients {
			recipient := msg.Recipients[i]

			desc := &vtxo.Descriptor{
				Outpoint: wire.OutPoint{
					Hash:  arkTxid,
					Index: recipient.OutputIndex,
				},
				Amount:         recipient.Value,
				PkScript:       recipient.PkScript,
				ClientKey:      h.recipientKey,
				OperatorKey:    h.operatorKey,
				RelativeExpiry: h.exitDelay,
				RoundID:        h.roundID,
				CommitmentTxID: arkTxid,
				TreePath:       &clienttree.Tree{},
				Status:         vtxo.VTXOStatusLive,
			}

			err := h.vtxoStore.SaveVTXO(ctx, desc)
			if err != nil {
				existing, getErr := h.vtxoStore.GetVTXO(
					ctx, desc.Outpoint,
				)
				if getErr != nil || existing == nil {
					return nil, err
				}

				if existing.Amount != desc.Amount {
					return nil, err
				}

				desc = existing
			}

			h.materialized = append(h.materialized, desc)
		}

		if len(h.materialized) == 0 {
			return nil, fmt.Errorf(
				"no incoming recipients materialized",
			)
		}

		return []clientoor.Event{
			&clientoor.IncomingHandledEvent{},
		}, nil

	case *clientoor.SendIncomingAckRequest:
		return []clientoor.Event{
			&clientoor.IncomingAckSentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

func driveOutboxToFSM(ctx context.Context, sessionID clientoor.SessionID,
	fsm *clientoor.StateMachine, handler clientoor.OutboxHandler,
	outbox []clientoor.OutboxEvent) error {

	for i := range outbox {
		followUps, err := handler.Handle(ctx, sessionID, outbox[i])
		if err != nil {
			return err
		}

		for j := range followUps {
			result := fsm.AskEvent(ctx, followUps[j]).Await(ctx)
			if result.IsErr() {
				return result.Err()
			}

			next := result.UnwrapOr(nil)
			if err := driveOutboxToFSM(
				ctx, sessionID, fsm, handler, next,
			); err != nil {
				return err
			}
		}
	}

	return nil
}
