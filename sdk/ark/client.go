package ark

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc"
)

const (
	// defaultCloseTimeout bounds how long Close waits for an embedded
	// daemon to shut down after cancellation.
	defaultCloseTimeout = 5 * time.Second
)

// Client is the consumer-facing Ark SDK facade. It hides whether the caller
// is talking to a remote daemon or an embedded in-process daemon runtime.
// Client is safe for concurrent use.
type Client struct {
	daemon waverpc.DaemonServiceClient
	waitCh <-chan error

	closeFn   func(context.Context) error
	closeOnce sync.Once
	closeErr  error
}

// WalletState mirrors the daemon's wallet lifecycle enum so SDK
// consumers can render wallet setup progress. WalletStateSyncing and
// WalletStateReady mean seed material is loaded; only WalletStateReady
// means the wallet is fully usable.
type WalletState int32

const (
	// WalletStateUnspecified is the proto3 zero value. The daemon never
	// emits this; reserved so a missing field deserializes to a safe
	// non-ready state.
	WalletStateUnspecified WalletState = 0

	// WalletStateNone indicates no wallet has been created yet.
	WalletStateNone WalletState = 1

	// WalletStateLocked indicates a wallet database exists but its
	// password has not been provided.
	WalletStateLocked WalletState = 2

	// WalletStateReady indicates the wallet is initialized, unlocked,
	// and signing is available.
	WalletStateReady WalletState = 3

	// WalletStateSyncing indicates the wallet is unlocked and the
	// backing chain source is catching up before wallet RPCs are safe.
	WalletStateSyncing WalletState = 4
)

// WalletReady reports whether the daemon wallet is fully unlocked and
// ready to sign. Convenience predicate over Info.WalletState.
func (i *Info) WalletReady() bool {
	if i == nil {
		return false
	}

	return i.WalletState == WalletStateReady
}

// Info describes the current status of a running waved instance.
type Info struct {
	// Version is the daemon's semantic version string.
	Version string

	// Commit is the build's git commit identifier.
	Commit string

	// Network is the active bitcoin network.
	Network string

	// LndIdentityPubKey is the connected lnd node identity public key
	// when the daemon is running in lnd-backed mode.
	LndIdentityPubKey string

	// BlockHeight is the best block height known to the daemon.
	BlockHeight uint32

	// ServerConnected reports whether the daemon's mailbox transport to the
	// Ark operator currently has mailbox ingress running.
	ServerConnected bool

	// LndAlias is the connected lnd node alias when applicable.
	LndAlias string

	// WalletType is the active wallet backend name.
	WalletType string

	// WalletState reports the daemon wallet lifecycle state
	// (Unspecified/None/Locked/Ready/Syncing). WalletStateSyncing
	// and WalletStateReady mean seed material is loaded; only
	// WalletStateReady means the wallet is fully usable.
	WalletState WalletState

	// IdentityPubKey is the daemon identity public key derived from the
	// active wallet backend.
	IdentityPubKey string

	// ServerInfo contains cached operator terms when the daemon has
	// already connected to the Ark server and fetched them. It remains
	// nil until the daemon reaches the operator-bootstrap stage, and can
	// be refreshed by daemon paths that fetch the live operator key before
	// building new policy scripts.
	ServerInfo *ServerInfo
}

// ServerInfo describes operator terms learned from the remote Ark server and
// cached by the daemon.
type ServerInfo struct {
	// OperatorPubKey is the compressed SEC-encoded public key of the Ark
	// operator.
	OperatorPubKey []byte

	// BoardingExitDelay is the minimum CSV delay required for boarding
	// outputs.
	BoardingExitDelay uint32

	// VTXOExitDelay is the minimum CSV delay required for VTXO outputs.
	VTXOExitDelay uint32

	// DustLimit is the minimum output value accepted by the operator.
	DustLimit uint64

	// MinVTXOAmountSat is the operator-advertised minimum VTXO output
	// amount in satoshis.
	MinVTXOAmountSat uint64

	// MinBoardingAmount is the smallest boarding amount accepted by the
	// operator.
	MinBoardingAmount uint64

	// MaxVTXOAmount is the largest amount accepted per VTXO by the
	// operator, applied to boarding requests, round outputs and OOR
	// recipient outputs alike. A value of zero means no cap.
	MaxVTXOAmount uint64

	// FeeRate is the operator's target package feerate in sat/vbyte.
	FeeRate uint64

	// MinOperatorFee is the minimum operator fee in satoshis.
	MinOperatorFee uint64

	// MinConfirmations is the minimum confirmations required on boarding
	// inputs.
	MinConfirmations uint32

	// MaxUserBalance is the maximum total balance in satoshis a single
	// user should hold in the system. A value of zero means no cap.
	MaxUserBalance uint64
}

// Seed contains the mnemonic and enciphered seed bytes returned by GenSeed.
type Seed struct {
	// Mnemonic is the generated 24-word aezeed mnemonic.
	Mnemonic []string

	// EncipheredSeed is the raw enciphered seed bytes returned by the
	// daemon.
	EncipheredSeed []byte
}

// WalletInitResult contains the daemon identity derived after wallet creation
// or unlock completes.
type WalletInitResult struct {
	// IdentityPubKey is the daemon identity public key derived after the
	// wallet was created or unlocked.
	IdentityPubKey string
}

// VTXOExpiryInfo is the SDK-owned typed view of a VTXO expiry
// classification.
type VTXOExpiryInfo struct {
	// Status is the daemon's current expiry posture for the VTXO.
	Status waverpc.VTXOExpiryStatus

	// CurrentHeight is the chain height used for classification.
	CurrentHeight int32

	// BatchExpiry is the VTXO batch expiry height.
	BatchExpiry int32

	// BlocksRemaining is BatchExpiry - CurrentHeight.
	BlocksRemaining int32

	// RefreshThresholdBlocks is the blocks-before-expiry threshold at
	// which cooperative refresh should begin.
	RefreshThresholdBlocks int32

	// CriticalThresholdBlocks is the blocks-before-expiry threshold at
	// which recovery/unilateral-exit handling should begin.
	CriticalThresholdBlocks int32

	// RelativeExpiry is the CSV delay used in the threshold calculation.
	RelativeExpiry uint32

	// MaxTreeDepth is the worst-case VTXO tree depth used in the threshold
	// calculation.
	MaxTreeDepth uint32

	// ChainDepth is the number of OOR checkpoint hops between this VTXO
	// and the most recent on-chain commitment.
	ChainDepth uint32
}

// VTXOInfo is the SDK-owned typed view of a daemon VTXO record.
type VTXOInfo struct {
	// Outpoint is the VTXO's outpoint in "txid:index" format.
	Outpoint string

	// AmountSat is the VTXO value in satoshis.
	AmountSat int64

	// Status is the daemon's current lifecycle state for the VTXO.
	Status waverpc.VTXOStatus

	// BatchExpiry is the absolute block height of the batch-wide expiry.
	BatchExpiry int32

	// RoundID is the identifier of the round that created the VTXO.
	RoundID string

	// CreatedHeight is the block height at which the VTXO was created.
	CreatedHeight int32

	// RelativeExpiry is the CSV delay, in blocks, for unilateral exit.
	RelativeExpiry uint32

	// PkScript is the raw taproot output script decoded from the daemon's
	// hex-encoded protobuf field.
	PkScript []byte

	// CommitmentTxID is the on-chain commitment txid anchoring the VTXO's
	// tree.
	CommitmentTxID string

	// ChainDepth is the number of OOR checkpoint hops between this VTXO
	// and the most recent on-chain commitment.
	ChainDepth uint32

	// FinalCheckpointPSBTs are the finalized checkpoint PSBTs that spent
	// this VTXO through an OOR package when known.
	FinalCheckpointPSBTs [][]byte

	// SpentByTxID is the Ark or OOR txid that spent this VTXO when known.
	SpentByTxID string

	// ExpiryInfo is the daemon's current expiry posture for this VTXO when
	// available.
	ExpiryInfo *VTXOExpiryInfo
}

// ReceiveInfo is the SDK-owned typed view of a wallet-owned receive
// destination allocated by the daemon.
type ReceiveInfo struct {
	// PkScript is the raw taproot output script for the receive
	// destination.
	PkScript []byte

	// PubKeyXOnly is the 32-byte x-only owner pubkey controlling the
	// receive destination.
	PubKeyXOnly []byte

	// KeyFamily is the wallet key family used for the receive key.
	KeyFamily uint32

	// KeyIndex is the wallet key index used for the receive key.
	KeyIndex uint32

	// Label is the human-readable registration label stored with the
	// indexer.
	Label string
}

// IndexedOORSessionInfo is the SDK-owned typed view of an indexed OOR session
// and its finalized checkpoints.
type IndexedOORSessionInfo struct {
	// ArkPSBT is the serialized Ark PSBT for the indexed OOR session.
	ArkPSBT []byte

	// CheckpointPSBTs are the serialized finalized checkpoint PSBTs for
	// the indexed OOR session.
	CheckpointPSBTs [][]byte
}

// CustomOORInput describes one caller-specified OOR input with an explicit
// policy template and spend path.
type CustomOORInput struct {
	// Outpoint identifies the VTXO to spend in "txid:vout" format.
	Outpoint string

	// VTXOPolicyTemplate is the serialized arkscript policy template for
	// the spent VTXO.
	VTXOPolicyTemplate []byte

	// SpendPath is the serialized arkscript spend path selected for this
	// checkpoint spend.
	SpendPath []byte

	// AmountSat is the VTXO value in satoshis.
	AmountSat int64

	// PkScript is the raw taproot output script of the spent VTXO.
	PkScript []byte

	// ExternalSignatures are tapscript signatures produced by additional
	// parties required by the selected spend path.
	ExternalSignatures []TaprootScriptSignature
}

// TaprootScriptSignature carries one externally produced tapscript signature
// for a custom OOR input.
type TaprootScriptSignature struct {
	// PubKey is the compressed public key that produced the signature.
	PubKey []byte

	// WitnessScript is the tapscript leaf this signature commits to.
	WitnessScript []byte

	// Signature is the raw Schnorr signature, optionally followed by a
	// one-byte sighash type when the sighash is not SIGHASH_DEFAULT.
	Signature []byte

	// SigHash is the tapscript sighash type. Zero means SIGHASH_DEFAULT.
	SigHash uint32
}

// PreparedOORCustomInput carries signing data for one prepared custom input.
type PreparedOORCustomInput struct {
	// Outpoint identifies the custom input.
	Outpoint string

	// CheckpointPSBT is the serialized unsigned checkpoint PSBT to sign.
	CheckpointPSBT []byte

	// WitnessScript is the tapscript leaf external parties must sign.
	WitnessScript []byte

	// SigningPubKeys are the compressed public keys required by the spend
	// path in script order.
	SigningPubKeys [][]byte
}

// PreparedOOR describes the deterministic package returned by PrepareOOR.
type PreparedOOR struct {
	// ArkPSBT is the serialized unsigned Ark PSBT.
	ArkPSBT []byte

	// CheckpointPSBTs are the serialized unsigned checkpoint PSBTs.
	CheckpointPSBTs [][]byte

	// CustomInputs carries signing data for each prepared custom input.
	CustomInputs []PreparedOORCustomInput

	// SessionID is the deterministic OOR session id.
	SessionID string
}

// WrapDaemonClient creates an Ark SDK facade from an already-connected daemon
// gRPC client. The optional closeFn lets callers release the underlying
// transport when the SDK facade owns it; pass nil when another layer manages
// the daemon client's lifetime.
func WrapDaemonClient(daemon waverpc.DaemonServiceClient,
	closeFn func(context.Context) error) *Client {

	return &Client{
		daemon:  daemon,
		waitCh:  closedWaitChan(),
		closeFn: closeFn,
	}
}

// Close releases the client transport and, for embedded clients, shuts down
// the in-process daemon.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}

	c.closeOnce.Do(func() {
		if c.closeFn == nil {
			return
		}

		closeCtx, cancel := context.WithTimeout(
			context.Background(), defaultCloseTimeout,
		)
		defer cancel()

		c.closeErr = c.closeFn(closeCtx)
	})

	return c.closeErr
}

// Wait returns a blocking channel that reports the embedded daemon's terminal
// run error. Remote clients return an already-closed channel because there is
// no in-process runtime to observe.
func (c *Client) Wait() <-chan error {
	if c == nil || c.waitCh == nil {
		return closedWaitChan()
	}

	return c.waitCh
}

// closedWaitChan returns an already-closed wait channel for client modes that
// do not manage an in-process daemon runtime.
func closedWaitChan() <-chan error {
	ch := make(chan error)
	close(ch)

	return ch
}

// GetInfo returns the daemon's current status, wallet state, and any cached
// operator terms.
func (c *Client) GetInfo(ctx context.Context) (*Info, error) {
	resp, err := c.daemon.GetInfo(ctx, &waverpc.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("get daemon info: %w", err)
	}

	info := &Info{
		Version:           resp.Version,
		Commit:            resp.Commit,
		Network:           resp.Network,
		LndIdentityPubKey: resp.LndIdentityPubkey,
		BlockHeight:       resp.BlockHeight,
		ServerConnected:   resp.ServerConnected,
		LndAlias:          resp.LndAlias,
		WalletType:        resp.WalletType,
		WalletState:       WalletState(resp.WalletState),
		IdentityPubKey:    resp.IdentityPubkey,
	}

	if resp.ServerInfo != nil {
		info.ServerInfo = &ServerInfo{
			OperatorPubKey: bytes.Clone(
				resp.ServerInfo.OperatorPubkey,
			),
			BoardingExitDelay: resp.ServerInfo.BoardingExitDelay,
			VTXOExitDelay:     resp.ServerInfo.VtxoExitDelay,
			DustLimit:         resp.ServerInfo.DustLimit,
			MinVTXOAmountSat:  resp.ServerInfo.MinVtxoAmountSat,
			MinBoardingAmount: resp.ServerInfo.MinBoardingAmount,
			MaxVTXOAmount:     resp.ServerInfo.MaxVtxoAmount,
			FeeRate:           resp.ServerInfo.FeeRate,
			MinOperatorFee:    resp.ServerInfo.MinOperatorFee,
			MinConfirmations:  resp.ServerInfo.MinConfirmations,
			MaxUserBalance:    resp.ServerInfo.MaxUserBalance,
		}
	}

	return info, nil
}

// BlockHeight returns the daemon's best known chain height from the current
// GetInfo snapshot.
func (c *Client) BlockHeight(ctx context.Context) (uint32, error) {
	info, err := c.GetInfo(ctx)
	if err != nil {
		return 0, err
	}

	return info.BlockHeight, nil
}

// IdentityPubKey parses and returns the daemon identity public key from the
// current GetInfo snapshot.
func (c *Client) IdentityPubKey(ctx context.Context) (*btcec.PublicKey, error) {
	info, err := c.GetInfo(ctx)
	if err != nil {
		return nil, err
	}

	return parseHexPubKey(info.IdentityPubKey, "identity")
}

// OperatorPubKey parses and returns the operator public key from the daemon's
// cached operator terms snapshot.
func (c *Client) OperatorPubKey(ctx context.Context) (*btcec.PublicKey, error) {
	info, err := c.GetInfo(ctx)
	if err != nil {
		return nil, err
	}

	if info.ServerInfo == nil {
		return nil, fmt.Errorf("operator terms are unavailable")
	}

	key, err := btcec.ParsePubKey(info.ServerInfo.OperatorPubKey)
	if err != nil {
		return nil, fmt.Errorf("parse operator public key: %w", err)
	}

	return key, nil
}

// GenSeed requests a fresh aezeed seed from a self-managed wallet daemon.
func (c *Client) GenSeed(ctx context.Context, seedPassphrase []byte) (*Seed,
	error) {

	resp, err := c.daemon.GenSeed(ctx, &waverpc.GenSeedRequest{
		SeedPassphrase: bytes.Clone(seedPassphrase),
	})
	if err != nil {
		return nil, fmt.Errorf("generate seed: %w", err)
	}

	return &Seed{
		Mnemonic:       append([]string(nil), resp.Mnemonic...),
		EncipheredSeed: bytes.Clone(resp.EncipheredSeed),
	}, nil
}

// InitWallet creates a new self-managed wallet from the supplied mnemonic and
// returns the daemon's derived identity public key.
func (c *Client) InitWallet(ctx context.Context, mnemonic []string,
	seedPassphrase, walletPassword []byte) (*WalletInitResult, error) {

	resp, err := c.daemon.InitWallet(ctx, &waverpc.InitWalletRequest{
		Mnemonic:       append([]string(nil), mnemonic...),
		SeedPassphrase: bytes.Clone(seedPassphrase),
		WalletPassword: bytes.Clone(walletPassword),
	})
	if err != nil {
		return nil, fmt.Errorf("initialize wallet: %w", err)
	}

	return &WalletInitResult{
		IdentityPubKey: resp.IdentityPubkey,
	}, nil
}

// UnlockWallet unlocks an existing self-managed wallet and returns the daemon
// identity public key derived after startup completes.
func (c *Client) UnlockWallet(ctx context.Context, walletPassword []byte) (
	*WalletInitResult, error) {

	resp, err := c.daemon.UnlockWallet(ctx,
		&waverpc.UnlockWalletRequest{
			WalletPassword: bytes.Clone(walletPassword),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("unlock wallet: %w", err)
	}

	return &WalletInitResult{
		IdentityPubKey: resp.IdentityPubkey,
	}, nil
}

// GetBalance returns the daemon's current wallet balances split across the
// boarding and VTXO buckets.
func (c *Client) GetBalance(ctx context.Context) (*waverpc.GetBalanceResponse,
	error) {

	resp, err := c.daemon.GetBalance(ctx,
		&waverpc.GetBalanceRequest{})
	if err != nil {
		return nil, fmt.Errorf("get wallet balance: %w", err)
	}

	return resp, nil
}

// ListVTXOs returns the daemon's known VTXOs using the supplied filter
// request. Passing nil uses the daemon defaults with no extra filters.
func (c *Client) ListVTXOs(ctx context.Context, req *waverpc.ListVTXOsRequest) (
	*waverpc.ListVTXOsResponse, error) {

	if req == nil {
		req = &waverpc.ListVTXOsRequest{}
	}

	resp, err := c.daemon.ListVTXOs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	return resp, nil
}

// NewAddress allocates a fresh boarding address from the daemon wallet.
func (c *Client) NewAddress(ctx context.Context) (*waverpc.NewAddressResponse,
	error) {

	resp, err := c.daemon.NewAddress(ctx, &waverpc.NewAddressRequest{})
	if err != nil {
		return nil, fmt.Errorf("create boarding address: %w", err)
	}

	return resp, nil
}

// NewReceiveScript allocates and registers a fresh receive script with the
// daemon and indexer.
func (c *Client) NewReceiveScript(ctx context.Context, label string) (
	*waverpc.NewReceiveScriptResponse, error) {

	resp, err := c.daemon.NewReceiveScript(ctx,
		&waverpc.NewReceiveScriptRequest{
			Label: label,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create receive script: %w", err)
	}

	return resp, nil
}

// AllocateReceiveScript allocates and decodes a wallet-owned receive
// destination for higher-level callers such as sdk/swaps.
func (c *Client) AllocateReceiveScript(ctx context.Context, label string) (
	*ReceiveInfo, error) {

	resp, err := c.NewReceiveScript(ctx, label)
	if err != nil {
		return nil, err
	}

	return newReceiveInfo(resp)
}

// ReceiveAuthKey asks the daemon wallet for the payment-scoped receive-auth
// public key used by higher-level swap receive flows.
func (c *Client) ReceiveAuthKey(ctx context.Context, paymentHash lntypes.Hash) (
	*btcec.PublicKey, error) {

	resp, err := c.daemon.ReceiveAuthKey(
		ctx, &waverpc.ReceiveAuthKeyRequest{
			PaymentHash: paymentHash[:],
		},
	)
	if err != nil {
		return nil, fmt.Errorf("get receive auth key: %w", err)
	}

	pubKey, err := btcec.ParsePubKey(resp.GetPubkey())
	if err != nil {
		return nil, fmt.Errorf("parse receive auth pubkey: %w", err)
	}

	return pubKey, nil
}

// SignReceiveAuthMessage asks the daemon wallet to sign one message with the
// payment-scoped receive-auth key.
func (c *Client) SignReceiveAuthMessage(ctx context.Context,
	paymentHash lntypes.Hash, message []byte, doubleHash bool) (
	*ecdsa.Signature, error) {

	resp, err := c.daemon.SignReceiveAuthMessage(
		ctx, &waverpc.SignReceiveAuthMessageRequest{
			PaymentHash: paymentHash[:],
			Message:     message,
			DoubleHash:  doubleHash,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("sign receive auth message: %w", err)
	}

	sig, err := ecdsa.ParseDERSignature(resp.GetSignature())
	if err != nil {
		return nil, fmt.Errorf("parse receive auth signature: %w", err)
	}

	return sig, nil
}

// SignReceiveAuthMessageCompact asks the daemon wallet to sign one message
// with the payment-scoped receive-auth key and return a compact signature.
func (c *Client) SignReceiveAuthMessageCompact(ctx context.Context,
	paymentHash lntypes.Hash, message []byte, doubleHash bool) ([]byte,
	error) {

	resp, err := c.daemon.SignReceiveAuthMessageCompact(
		ctx, &waverpc.SignReceiveAuthMessageCompactRequest{
			PaymentHash: paymentHash[:],
			Message:     message,
			DoubleHash:  doubleHash,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("sign receive auth message compact: %w",
			err)
	}

	return append([]byte(nil), resp.GetSignature()...), nil
}

// ReceiveAuthECDH asks the daemon wallet to derive one Sphinx shared secret
// with the payment-scoped receive-auth key.
func (c *Client) ReceiveAuthECDH(ctx context.Context, paymentHash lntypes.Hash,
	pubKey *btcec.PublicKey) ([32]byte, error) {

	if pubKey == nil {
		return [32]byte{}, fmt.Errorf("receive auth ECDH pubkey is " +
			"required")
	}

	resp, err := c.daemon.ReceiveAuthECDH(
		ctx, &waverpc.ReceiveAuthECDHRequest{
			PaymentHash: paymentHash[:],
			Pubkey:      pubKey.SerializeCompressed(),
		},
	)
	if err != nil {
		return [32]byte{}, fmt.Errorf("receive auth ECDH: %w", err)
	}
	if len(resp.GetSharedSecret()) != 32 {
		return [32]byte{}, fmt.Errorf("receive auth shared secret " +
			"must be 32 bytes")
	}

	var sharedSecret [32]byte
	copy(sharedSecret[:], resp.GetSharedSecret())

	return sharedSecret, nil
}

// GetIndexedVTXOByPkScript asks the daemon's indexer client for the first VTXO
// matching the supplied script and status filters. Passing nil uses the
// daemon defaults.
func (c *Client) GetIndexedVTXOByPkScript(ctx context.Context,
	req *waverpc.GetIndexedVTXOByPkScriptRequest) (
	*waverpc.GetIndexedVTXOByPkScriptResponse, error) {

	if req == nil {
		req = &waverpc.GetIndexedVTXOByPkScriptRequest{}
	}

	resp, err := c.daemon.GetIndexedVTXOByPkScript(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get indexed vtxo by script: %w", err)
	}

	return resp, nil
}

// GetVTXOExpiryInfo asks the daemon to classify a VTXO using the
// authoritative wallet/VTXO expiry policy. Passing nil uses an empty request.
func (c *Client) GetVTXOExpiryInfo(ctx context.Context,
	req *waverpc.GetVTXOExpiryInfoRequest) (
	*waverpc.GetVTXOExpiryInfoResponse, error) {

	if req == nil {
		req = &waverpc.GetVTXOExpiryInfoRequest{}
	}

	resp, err := c.daemon.GetVTXOExpiryInfo(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get vtxo expiry info: %w", err)
	}

	return resp, nil
}

// GetIndexedOORSessionByTxid asks the daemon's indexer client for the indexed
// OOR session matching the supplied spent-script proof and session txid.
// Passing nil uses the daemon defaults.
func (c *Client) GetIndexedOORSessionByTxid(ctx context.Context,
	req *waverpc.GetIndexedOORSessionByTxidRequest) (
	*waverpc.GetIndexedOORSessionByTxidResponse, error) {

	if req == nil {
		req = &waverpc.GetIndexedOORSessionByTxidRequest{}
	}

	resp, err := c.daemon.GetIndexedOORSessionByTxid(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get indexed oor session: %w", err)
	}

	return resp, nil
}

// FindLiveVTXOByPkScript returns the first live indexed VTXO matching the
// supplied output script.
func (c *Client) FindLiveVTXOByPkScript(ctx context.Context, pkScript []byte) (
	*VTXOInfo, error) {

	return c.findIndexedVTXOByPkScript(
		ctx, pkScript, waverpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
}

// FindSpentVTXOByPkScript returns the first spent indexed VTXO matching the
// supplied output script.
func (c *Client) FindSpentVTXOByPkScript(ctx context.Context, pkScript []byte) (
	*VTXOInfo, error) {

	return c.findIndexedVTXOByPkScript(
		ctx, pkScript, waverpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
}

// findIndexedVTXOByPkScript queries the daemon's authoritative indexer view
// for one VTXO matching the supplied script and status.
func (c *Client) findIndexedVTXOByPkScript(ctx context.Context, pkScript []byte,
	status waverpc.VTXOStatus) (*VTXOInfo, error) {

	resp, err := c.GetIndexedVTXOByPkScript(ctx,
		&waverpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: append([]byte(nil), pkScript...),
			StatusFilter: []waverpc.VTXOStatus{
				status,
			},
		},
	)
	if err != nil {
		return nil, err
	}

	if resp.GetVtxo() == nil {
		return nil, nil
	}

	return newVTXOInfo(resp.GetVtxo())
}

// GetIndexedOORSession looks up one indexed OOR session by the spent output
// script and deterministic session txid.
func (c *Client) GetIndexedOORSession(ctx context.Context, pkScript []byte,
	sessionTxID string) (*IndexedOORSessionInfo, error) {

	sessionHash, err := parseSessionTxID(sessionTxID)
	if err != nil {
		return nil, fmt.Errorf("parse session txid: %w", err)
	}

	resp, err := c.GetIndexedOORSessionByTxid(ctx,
		&waverpc.GetIndexedOORSessionByTxidRequest{
			PkScript:    append([]byte(nil), pkScript...),
			SessionTxid: sessionHash,
		},
	)
	if err != nil {
		return nil, err
	}

	return newIndexedOORSessionInfo(resp), nil
}

// GetOORSession returns the daemon's local durable status for one OOR
// transfer session.
func (c *Client) GetOORSession(ctx context.Context, sessionID string) (
	*waverpc.OORSessionInfo, error) {

	resp, err := c.daemon.GetOORSession(ctx,
		&waverpc.GetOORSessionRequest{
			SessionId: sessionID,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("get oor session: %w", err)
	}

	return resp.GetSession(), nil
}

// SendVTXO submits an in-round send request through the daemon. Passing nil
// uses an empty request.
func (c *Client) SendVTXO(ctx context.Context, req *waverpc.SendVTXORequest) (
	*waverpc.SendVTXOResponse, error) {

	if req == nil {
		req = &waverpc.SendVTXORequest{}
	}

	resp, err := c.daemon.SendVTXO(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("send vtxo: %w", err)
	}

	return resp, nil
}

// SendOOR submits an out-of-round send request through the daemon. Passing nil
// uses an empty request.
func (c *Client) SendOOR(ctx context.Context, req *waverpc.SendOORRequest) (
	*waverpc.SendOORResponse, error) {

	if req == nil {
		req = &waverpc.SendOORRequest{}
	}

	resp, err := c.daemon.SendOOR(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("send oor transfer: %w", err)
	}

	return resp, nil
}

// OORSendResult contains daemon metadata for an accepted OOR transfer.
type OORSendResult struct {
	// SessionID is the OOR session identifier.
	SessionID string

	// RecipientOutpoint is the Ark tx outpoint created for the requested
	// recipient when the daemon can resolve it locally.
	RecipientOutpoint string
}

// SendOORWithPolicy sends one OOR transfer to a semantic policy-backed
// destination and returns the resulting OOR session id.
func (c *Client) SendOORWithPolicy(ctx context.Context, amountSat int64,
	recipientPolicyTemplate []byte) (string, error) {

	result, err := c.SendOORWithPolicyAndKeyDetails(
		ctx, amountSat, recipientPolicyTemplate, "",
	)
	if err != nil {
		return "", err
	}

	return result.SessionID, nil
}

// SendOORWithPolicyAndKey sends one OOR transfer to a semantic policy-backed
// destination using the supplied idempotency key and returns the resulting OOR
// session id.
func (c *Client) SendOORWithPolicyAndKey(ctx context.Context, amountSat int64,
	recipientPolicyTemplate []byte, idempotencyKey string) (string, error) {

	result, err := c.SendOORWithPolicyAndKeyDetails(
		ctx, amountSat, recipientPolicyTemplate, idempotencyKey,
	)
	if err != nil {
		return "", err
	}

	return result.SessionID, nil
}

// SendOORWithPolicyDetails sends one OOR transfer to a semantic policy-backed
// destination and returns the accepted OOR metadata.
func (c *Client) SendOORWithPolicyDetails(ctx context.Context, amountSat int64,
	recipientPolicyTemplate []byte) (*OORSendResult, error) {

	return c.SendOORWithPolicyAndKeyDetails(
		ctx, amountSat, recipientPolicyTemplate, "",
	)
}

// SendOORWithPolicyAndKeyDetails sends one OOR transfer to a semantic
// policy-backed destination using the supplied idempotency key and returns the
// accepted OOR metadata.
func (c *Client) SendOORWithPolicyAndKeyDetails(ctx context.Context,
	amountSat int64, recipientPolicyTemplate []byte,
	idempotencyKey string) (*OORSendResult, error) {

	resp, err := c.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{
			{
				Destination: &waverpc.Output_PolicyTemplate{
					PolicyTemplate: append(
						[]byte(nil),
						recipientPolicyTemplate...,
					),
				},
				AmountSat: amountSat,
			},
		},
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return nil, err
	}

	recipientOutpoints := resp.GetRecipientOutpoints()
	recipientOutpoint := ""
	if len(recipientOutpoints) > 0 {
		recipientOutpoint = recipientOutpoints[0]
	}

	return &OORSendResult{
		SessionID:         resp.GetSessionId(),
		RecipientOutpoint: recipientOutpoint,
	}, nil
}

// SendOORWithCustomInputs sends one OOR transfer using caller-specified
// inputs and a standard x-only pubkey destination.
func (c *Client) SendOORWithCustomInputs(ctx context.Context,
	recipientPubKey []byte, amountSat int64, inputs []CustomOORInput) (
	string, error) {

	resp, err := c.SendOOR(ctx, &waverpc.SendOORRequest{
		Recipients: []*waverpc.Output{
			{
				Destination: &waverpc.Output_Pubkey{
					Pubkey: append(
						[]byte(nil), recipientPubKey...,
					),
				},
				AmountSat: amountSat,
			},
		},
		CustomInputs: customOORInputsToRPC(inputs),
	})
	if err != nil {
		return "", err
	}

	return resp.GetSessionId(), nil
}

// ArmVHTLCRecovery asks the daemon to persist one dormant vHTLC recovery job.
// Higher-level swap FSMs call this before the cooperative path becomes risky so
// restart recovery has a durable handle to the vHTLC context.
func (c *Client) ArmVHTLCRecovery(ctx context.Context,
	req *waverpc.ArmVHTLCRecoveryRequest) (
	*waverpc.ArmVHTLCRecoveryResponse, error) {

	if req == nil {
		req = &waverpc.ArmVHTLCRecoveryRequest{}
	}

	resp, err := c.daemon.ArmVHTLCRecovery(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("arm vhtlc recovery: %w", err)
	}

	return resp, nil
}

// EscalateVHTLCRecovery asks the daemon to start or resume the on-chain unroll
// path for one previously armed vHTLC recovery job.
func (c *Client) EscalateVHTLCRecovery(ctx context.Context,
	req *waverpc.EscalateVHTLCRecoveryRequest) (
	*waverpc.EscalateVHTLCRecoveryResponse, error) {

	if req == nil {
		req = &waverpc.EscalateVHTLCRecoveryRequest{}
	}

	resp, err := c.daemon.EscalateVHTLCRecovery(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("escalate vhtlc recovery: %w", err)
	}

	return resp, nil
}

// CancelVHTLCRecovery tells the daemon that cooperative settlement made one
// armed vHTLC recovery job unnecessary.
func (c *Client) CancelVHTLCRecovery(ctx context.Context,
	req *waverpc.CancelVHTLCRecoveryRequest) (
	*waverpc.CancelVHTLCRecoveryResponse, error) {

	if req == nil {
		req = &waverpc.CancelVHTLCRecoveryRequest{}
	}

	resp, err := c.daemon.CancelVHTLCRecovery(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("cancel vhtlc recovery: %w", err)
	}

	return resp, nil
}

// GetVHTLCRecoveryStatus returns the daemon's durable recovery status plus the
// current unroll phase when the recovery has been escalated.
func (c *Client) GetVHTLCRecoveryStatus(ctx context.Context,
	req *waverpc.GetVHTLCRecoveryStatusRequest) (
	*waverpc.GetVHTLCRecoveryStatusResponse, error) {

	if req == nil {
		req = &waverpc.GetVHTLCRecoveryStatusRequest{}
	}

	resp, err := c.daemon.GetVHTLCRecoveryStatus(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get vhtlc recovery status: %w", err)
	}

	return resp, nil
}

// PrepareOORWithCustomInputs builds a deterministic custom-input OOR package
// without submitting it.
func (c *Client) PrepareOORWithCustomInputs(ctx context.Context,
	recipientPubKey []byte, amountSat int64, inputs []CustomOORInput) (
	*PreparedOOR, error) {

	rpcInputs := customOORInputsToRPC(inputs)
	resp, err := c.daemon.PrepareOOR(
		ctx, &waverpc.PrepareOORRequest{
			Recipient: &waverpc.Output{
				Destination: &waverpc.Output_Pubkey{
					Pubkey: append(
						[]byte(nil), recipientPubKey...,
					),
				},
				AmountSat: amountSat,
			},
			CustomInputs: rpcInputs,
		},
	)
	if err != nil {
		return nil, err
	}

	preparedInputs := make(
		[]PreparedOORCustomInput, 0,
		len(
			resp.GetCustomInputs(),
		),
	)
	for _, input := range resp.GetCustomInputs() {
		signingPubKeys := make(
			[][]byte, 0,
			len(
				input.GetSigningPubkeys(),
			),
		)
		for _, key := range input.GetSigningPubkeys() {
			signingPubKeys = append(
				signingPubKeys,
				append(
					[]byte(nil), key...,
				),
			)
		}

		preparedInputs = append(preparedInputs, PreparedOORCustomInput{
			Outpoint: input.GetOutpoint(),
			CheckpointPSBT: append(
				[]byte(nil), input.GetCheckpointPsbt()...,
			),
			WitnessScript: append(
				[]byte(nil), input.GetWitnessScript()...,
			),
			SigningPubKeys: signingPubKeys,
		})
	}

	checkpoints := make([][]byte, 0, len(resp.GetCheckpointPsbts()))
	for _, checkpoint := range resp.GetCheckpointPsbts() {
		checkpoints = append(
			checkpoints,
			append(
				[]byte(nil), checkpoint...,
			),
		)
	}

	return &PreparedOOR{
		ArkPSBT:         append([]byte(nil), resp.GetArkPsbt()...),
		CheckpointPSBTs: checkpoints,
		CustomInputs:    preparedInputs,
		SessionID:       resp.GetSessionId(),
	}, nil
}

// SignOORCustomInput asks the daemon identity key to sign one prepared custom
// OOR input.
func (c *Client) SignOORCustomInput(ctx context.Context, input CustomOORInput,
	checkpointPSBT []byte) (*TaprootScriptSignature, error) {

	resp, err := c.daemon.SignOORCustomInput(
		ctx, &waverpc.SignOORCustomInputRequest{
			CustomInput:    customOORInputToRPC(input),
			CheckpointPsbt: append([]byte(nil), checkpointPSBT...),
		},
	)
	if err != nil {
		return nil, err
	}

	sig := resp.GetSignature()
	if sig == nil {
		return nil, fmt.Errorf("daemon returned empty custom input " +
			"signature")
	}

	return &TaprootScriptSignature{
		PubKey:        append([]byte(nil), sig.GetPubkey()...),
		WitnessScript: append([]byte(nil), sig.GetWitnessScript()...),
		Signature:     append([]byte(nil), sig.GetSignature()...),
		SigHash:       sig.GetSighash(),
	}, nil
}

// customOORInputsToRPC converts SDK custom input structs to RPC messages.
func customOORInputsToRPC(inputs []CustomOORInput) []*waverpc.CustomOORInput {
	rpcInputs := make([]*waverpc.CustomOORInput, len(inputs))
	for i := range inputs {
		rpcInputs[i] = customOORInputToRPC(inputs[i])
	}

	return rpcInputs
}

// customOORInputToRPC converts one SDK custom input struct to an RPC message.
func customOORInputToRPC(input CustomOORInput) *waverpc.CustomOORInput {
	externalSigs := make(
		[]*waverpc.TaprootScriptSignature, 0,
		len(input.ExternalSignatures),
	)
	for j := range input.ExternalSignatures {
		sig := input.ExternalSignatures[j]
		externalSigs = append(
			externalSigs, &waverpc.TaprootScriptSignature{
				Pubkey: append([]byte(nil), sig.PubKey...),
				WitnessScript: append(
					[]byte(nil), sig.WitnessScript...,
				),
				Signature: append(
					[]byte(nil), sig.Signature...,
				),
				Sighash: sig.SigHash,
			},
		)
	}

	return &waverpc.CustomOORInput{
		Outpoint: input.Outpoint,
		VtxoPolicyTemplate: append(
			[]byte(nil), input.VTXOPolicyTemplate...,
		),
		SpendPath:          append([]byte(nil), input.SpendPath...),
		AmountSat:          input.AmountSat,
		PkScript:           append([]byte(nil), input.PkScript...),
		ExternalSignatures: externalSigs,
	}
}

// ListLiveVTXOs returns the daemon's live spendable VTXOs as typed SDK-owned
// models.
func (c *Client) ListLiveVTXOs(ctx context.Context) ([]VTXOInfo, error) {
	return c.listVTXOsByStatus(
		ctx, waverpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
}

// ListSpentVTXOs returns the daemon's locally tracked spent VTXOs as typed
// SDK-owned models.
func (c *Client) ListSpentVTXOs(ctx context.Context) ([]VTXOInfo, error) {
	return c.listVTXOsByStatus(
		ctx, waverpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
}

// listVTXOsByStatus loads one daemon VTXO status bucket and converts it into
// typed SDK-owned models.
func (c *Client) listVTXOsByStatus(ctx context.Context,
	status waverpc.VTXOStatus) ([]VTXOInfo, error) {

	resp, err := c.ListVTXOs(ctx, &waverpc.ListVTXOsRequest{
		StatusFilter: status,
	})
	if err != nil {
		return nil, err
	}

	vtxos := make([]VTXOInfo, 0, len(resp.GetVtxos()))
	for _, vtxo := range resp.GetVtxos() {
		info, err := newVTXOInfo(vtxo)
		if err != nil {
			return nil, err
		}

		vtxos = append(vtxos, *info)
	}

	return vtxos, nil
}

// RefreshVTXOs queues one or more VTXOs for refresh in the next round.
// Passing nil uses an empty request.
func (c *Client) RefreshVTXOs(ctx context.Context,
	req *waverpc.RefreshVTXOsRequest) (*waverpc.RefreshVTXOsResponse,
	error) {

	if req == nil {
		req = &waverpc.RefreshVTXOsRequest{}
	}

	resp, err := c.daemon.RefreshVTXOs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("refresh vtxos: %w", err)
	}

	return resp, nil
}

// RefreshCustomVTXOs queues caller-supplied custom-policy VTXOs for refresh in
// the next round. Passing nil uses an empty request and lets the daemon return
// the validation error.
func (c *Client) RefreshCustomVTXOs(ctx context.Context,
	req *waverpc.RefreshCustomVTXOsRequest) (
	*waverpc.RefreshCustomVTXOsResponse, error) {

	if req == nil {
		req = &waverpc.RefreshCustomVTXOsRequest{}
	}

	resp, err := c.daemon.RefreshCustomVTXOs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("refresh custom vtxos: %w", err)
	}

	return resp, nil
}

// SignVTXOForfeit asks the daemon wallet to sign one exact connector-bound
// forfeit transaction input.
func (c *Client) SignVTXOForfeit(ctx context.Context,
	req *waverpc.SignVTXOForfeitRequest) (*waverpc.SignVTXOForfeitResponse,
	error) {

	if req == nil {
		req = &waverpc.SignVTXOForfeitRequest{}
	}

	resp, err := c.daemon.SignVTXOForfeit(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("sign vtxo forfeit: %w", err)
	}

	return resp, nil
}

// Board tells the daemon to register any confirmed boarding UTXOs in the next
// round. Passing no request preserves the legacy single-VTXO board behavior.
func (c *Client) Board(ctx context.Context, reqs ...*waverpc.BoardRequest) (
	*waverpc.BoardResponse, error) {

	req := &waverpc.BoardRequest{}
	if len(reqs) > 0 && reqs[0] != nil {
		req = reqs[0]
	}

	resp, err := c.daemon.Board(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("board confirmed utxos: %w", err)
	}

	return resp, nil
}

// ListRounds returns the daemon's current round FSM snapshots. Passing nil
// uses the daemon defaults.
func (c *Client) ListRounds(ctx context.Context,
	req *waverpc.ListRoundsRequest) (*waverpc.ListRoundsResponse, error) {

	if req == nil {
		req = &waverpc.ListRoundsRequest{}
	}

	resp, err := c.daemon.ListRounds(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list rounds: %w", err)
	}

	return resp, nil
}

// WatchRounds subscribes to streaming round FSM updates from the daemon. This
// is a pre-1.0 passthrough stream and currently exposes the generated gRPC
// stream type directly.
func (c *Client) WatchRounds(ctx context.Context) (
	grpc.ServerStreamingClient[waverpc.WatchRoundsResponse], error) {

	stream, err := c.daemon.WatchRounds(
		ctx, &waverpc.WatchRoundsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("watch rounds: %w", err)
	}

	return stream, nil
}

// EstimateFee asks the daemon to proxy an operator fee estimate for the
// supplied operation parameters. Passing nil uses an empty request.
func (c *Client) EstimateFee(ctx context.Context,
	req *waverpc.EstimateFeeRequest) (*waverpc.EstimateFeeResponse, error) {

	if req == nil {
		req = &waverpc.EstimateFeeRequest{}
	}

	resp, err := c.daemon.EstimateFee(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("estimate fee: %w", err)
	}

	return resp, nil
}

// GetFeeHistory returns paginated fee history from the daemon's local ledger.
// Passing nil uses the daemon defaults.
func (c *Client) GetFeeHistory(ctx context.Context,
	req *waverpc.GetFeeHistoryRequest) (*waverpc.GetFeeHistoryResponse,
	error) {

	if req == nil {
		req = &waverpc.GetFeeHistoryRequest{}
	}

	resp, err := c.daemon.GetFeeHistory(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get fee history: %w", err)
	}

	return resp, nil
}

// parseHexPubKey decodes and parses one compressed public key encoded as hex.
func parseHexPubKey(pubKeyHex, field string) (*btcec.PublicKey, error) {
	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return nil, fmt.Errorf("decode %s public key hex: %w", field,
			err)
	}

	pubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s public key: %w", field, err)
	}

	return pubKey, nil
}

// newVTXOInfo converts one daemon protobuf VTXO into the SDK-owned typed
// model, decoding any hex-encoded binary fields along the way.
func newVTXOInfo(vtxo *waverpc.VTXO) (*VTXOInfo, error) {
	pkScript, err := hex.DecodeString(vtxo.GetPkScript())
	if err != nil {
		return nil, fmt.Errorf("decode vtxo pk_script: %w", err)
	}

	checkpoints := make([][]byte, 0, len(vtxo.GetOorFinalCheckpointPsbts()))
	for i := range vtxo.GetOorFinalCheckpointPsbts() {
		checkpoints = append(
			checkpoints,
			append(
				[]byte(nil),
				vtxo.GetOorFinalCheckpointPsbts()[i]...,
			),
		)
	}

	return &VTXOInfo{
		Outpoint:             vtxo.GetOutpoint(),
		AmountSat:            vtxo.GetAmountSat(),
		Status:               vtxo.GetStatus(),
		BatchExpiry:          vtxo.GetBatchExpiry(),
		RoundID:              vtxo.GetRoundId(),
		CreatedHeight:        vtxo.GetCreatedHeight(),
		RelativeExpiry:       vtxo.GetRelativeExpiry(),
		PkScript:             pkScript,
		CommitmentTxID:       vtxo.GetCommitmentTxid(),
		ChainDepth:           vtxo.GetChainDepth(),
		FinalCheckpointPSBTs: checkpoints,
		SpentByTxID:          vtxo.GetSpentByTxid(),
		ExpiryInfo:           newVTXOExpiryInfo(vtxo.GetExpiryInfo()),
	}, nil
}

// newVTXOExpiryInfo converts one daemon protobuf VTXO expiry info message into
// the SDK-owned typed model.
func newVTXOExpiryInfo(info *waverpc.VTXOExpiryInfo) *VTXOExpiryInfo {
	if info == nil {
		return nil
	}

	return &VTXOExpiryInfo{
		Status:                  info.GetStatus(),
		CurrentHeight:           info.GetCurrentHeight(),
		BatchExpiry:             info.GetBatchExpiry(),
		BlocksRemaining:         info.GetBlocksRemaining(),
		RefreshThresholdBlocks:  info.GetRefreshThresholdBlocks(),
		CriticalThresholdBlocks: info.GetCriticalThresholdBlocks(),
		RelativeExpiry:          info.GetRelativeExpiry(),
		MaxTreeDepth:            info.GetMaxTreeDepth(),
		ChainDepth:              info.GetChainDepth(),
	}
}

// newReceiveInfo converts one daemon protobuf receive-script response
// into the SDK-owned typed model.
func newReceiveInfo(resp *waverpc.NewReceiveScriptResponse) (*ReceiveInfo,
	error) {

	pkScript, err := hex.DecodeString(resp.GetPkScriptHex())
	if err != nil {
		return nil, fmt.Errorf("decode receive pk_script: %w", err)
	}

	pubKeyXOnly, err := hex.DecodeString(resp.GetPubkeyXonlyHex())
	if err != nil {
		return nil, fmt.Errorf("decode receive x-only pubkey: %w", err)
	}

	return &ReceiveInfo{
		PkScript:    pkScript,
		PubKeyXOnly: pubKeyXOnly,
		KeyFamily:   resp.GetKeyFamily(),
		KeyIndex:    resp.GetKeyIndex(),
		Label:       resp.GetLabel(),
	}, nil
}

// newIndexedOORSessionInfo converts one daemon protobuf indexed OOR session
// into the SDK-owned typed model.
func newIndexedOORSessionInfo(
	resp *waverpc.GetIndexedOORSessionByTxidResponse,
) *IndexedOORSessionInfo {

	checkpoints := make([][]byte, 0, len(resp.GetCheckpointPsbts()))
	for i := range resp.GetCheckpointPsbts() {
		checkpoints = append(
			checkpoints,
			append(
				[]byte(nil), resp.GetCheckpointPsbts()[i]...,
			),
		)
	}

	return &IndexedOORSessionInfo{
		ArkPSBT:         append([]byte(nil), resp.GetArkPsbt()...),
		CheckpointPSBTs: checkpoints,
	}
}

// parseSessionTxID converts a human-readable txid string into the raw byte
// order expected by the daemon's indexer RPC.
func parseSessionTxID(sessionTxID string) ([]byte, error) {
	hash, err := chainhash.NewHashFromStr(sessionTxID)
	if err != nil {
		return nil, err
	}

	return append([]byte(nil), hash[:]...), nil
}
