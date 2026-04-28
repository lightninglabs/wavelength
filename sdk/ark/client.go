package ark

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/daemonrpc"
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
	daemon daemonrpc.DaemonServiceClient
	waitCh <-chan error

	closeFn   func(context.Context) error
	closeOnce sync.Once
	closeErr  error
}

// Info describes the current status of a running darepod instance.
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

	// WalletReady reports whether the daemon wallet subsystem is
	// initialized and ready to serve wallet-dependent RPCs.
	WalletReady bool

	// IdentityPubKey is the daemon identity public key derived from the
	// active wallet backend.
	IdentityPubKey string

	// ServerInfo contains cached operator terms when the daemon has
	// already connected to the Ark server and fetched them. It remains
	// nil until the daemon reaches the operator-bootstrap stage, and is
	// currently refreshed only during bootstrap.
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

	// ForfeitScript is the raw serialized scriptPubKey required in
	// forfeit transactions.
	ForfeitScript []byte

	// SweepKey is the compressed SEC-encoded operator public key used in
	// VTXO sweep paths when present.
	SweepKey []byte

	// SweepDelay is the batch-wide absolute timelock in blocks.
	SweepDelay uint32

	// DustLimit is the minimum output value accepted by the operator.
	DustLimit uint64

	// MinBoardingAmount is the smallest boarding amount accepted by the
	// operator.
	MinBoardingAmount uint64

	// MaxBoardingAmount is the largest boarding amount accepted by the
	// operator. A value of zero means no cap.
	MaxBoardingAmount uint64

	// FeeRate is the operator's target package feerate in sat/vbyte.
	FeeRate uint64

	// MinOperatorFee is the minimum operator fee in satoshis.
	MinOperatorFee uint64

	// MinConfirmations is the minimum confirmations required on boarding
	// inputs.
	MinConfirmations uint32
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

// VTXOInfo is the SDK-owned typed view of a daemon VTXO record.
type VTXOInfo struct {
	// Outpoint is the VTXO's outpoint in "txid:index" format.
	Outpoint string

	// AmountSat is the VTXO value in satoshis.
	AmountSat int64

	// Status is the daemon's current lifecycle state for the VTXO.
	Status daemonrpc.VTXOStatus

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

// OORSessionDirection filters local OOR session listing by transfer direction.
type OORSessionDirection int

const (
	// OORSessionDirectionAll includes outgoing and incoming local sessions.
	OORSessionDirectionAll OORSessionDirection = iota

	// OORSessionDirectionOutgoing includes locally initiated OOR sends.
	OORSessionDirectionOutgoing

	// OORSessionDirectionIncoming includes locally received OOR transfers.
	OORSessionDirectionIncoming
)

// ListOORSessionsRequest describes the local OOR session filters used by the
// typed SDK helper.
type ListOORSessionsRequest struct {
	// PendingOnly restricts the result to sessions that have not reached a
	// terminal state yet.
	PendingOnly bool

	// Direction restricts the result by local transfer direction.
	Direction OORSessionDirection

	// IdempotencyKey restricts the result to outgoing sessions created
	// with this caller-provided key. Empty disables the filter.
	IdempotencyKey string
}

// OORSessionInfo is the SDK-owned typed view of one locally persisted OOR
// session summary.
type OORSessionInfo struct {
	// SessionID is the stable OOR session identifier.
	SessionID string

	// Direction describes whether the local daemon initiated or received
	// the OOR transfer.
	Direction OORSessionDirection

	// Phase is the local actor phase name persisted for the session.
	Phase string

	// Pending reports whether the session still needs more work.
	Pending bool

	// RetryAfter is the actor's current retry delay for pending work.
	RetryAfter time.Duration

	// RetryReason explains why retrying is still pending when known.
	RetryReason string

	// IdempotencyKey is the caller-provided key used to create an outgoing
	// session, when one was provided.
	IdempotencyKey string

	// InputOutpoints are the VTXOs selected to fund an outgoing session.
	InputOutpoints []string

	// InputAmountSat is the total value of selected outgoing inputs.
	InputAmountSat int64

	// RecipientCount is the number of Ark transaction outputs.
	RecipientCount int32
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
}

// WrapDaemonClient creates an Ark SDK facade from an already-connected daemon
// gRPC client. The optional closeFn lets callers release the underlying
// transport when the SDK facade owns it; pass nil when another layer manages
// the daemon client's lifetime.
func WrapDaemonClient(daemon daemonrpc.DaemonServiceClient,
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
	resp, err := c.daemon.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
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
		WalletReady:       resp.WalletReady,
		IdentityPubKey:    resp.IdentityPubkey,
	}

	if resp.ServerInfo != nil {
		info.ServerInfo = &ServerInfo{
			OperatorPubKey: bytes.Clone(
				resp.ServerInfo.OperatorPubkey,
			),
			BoardingExitDelay: resp.ServerInfo.BoardingExitDelay,
			VTXOExitDelay:     resp.ServerInfo.VtxoExitDelay,
			ForfeitScript: bytes.Clone(
				resp.ServerInfo.ForfeitScript,
			),
			SweepKey: bytes.Clone(
				resp.ServerInfo.SweepKey,
			),
			SweepDelay:        resp.ServerInfo.SweepDelay,
			DustLimit:         resp.ServerInfo.DustLimit,
			MinBoardingAmount: resp.ServerInfo.MinBoardingAmount,
			MaxBoardingAmount: resp.ServerInfo.MaxBoardingAmount,
			FeeRate:           resp.ServerInfo.FeeRate,
			MinOperatorFee:    resp.ServerInfo.MinOperatorFee,
			MinConfirmations:  resp.ServerInfo.MinConfirmations,
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
func (c *Client) GenSeed(ctx context.Context,
	seedPassphrase []byte) (*Seed, error) {

	resp, err := c.daemon.GenSeed(ctx, &daemonrpc.GenSeedRequest{
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

	resp, err := c.daemon.InitWallet(ctx, &daemonrpc.InitWalletRequest{
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
func (c *Client) UnlockWallet(ctx context.Context,
	walletPassword []byte) (*WalletInitResult, error) {

	resp, err := c.daemon.UnlockWallet(ctx,
		&daemonrpc.UnlockWalletRequest{
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
func (c *Client) GetBalance(ctx context.Context) (
	*daemonrpc.GetBalanceResponse, error) {

	resp, err := c.daemon.GetBalance(ctx,
		&daemonrpc.GetBalanceRequest{})
	if err != nil {
		return nil, fmt.Errorf("get wallet balance: %w", err)
	}

	return resp, nil
}

// ListVTXOs returns the daemon's known VTXOs using the supplied filter
// request. Passing nil uses the daemon defaults with no extra filters.
func (c *Client) ListVTXOs(ctx context.Context,
	req *daemonrpc.ListVTXOsRequest) (*daemonrpc.ListVTXOsResponse, error) {

	if req == nil {
		req = &daemonrpc.ListVTXOsRequest{}
	}

	resp, err := c.daemon.ListVTXOs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	return resp, nil
}

// ListOORSessions returns the daemon's locally persisted OOR session progress
// using the supplied filters. Passing nil uses daemon defaults.
func (c *Client) ListOORSessions(ctx context.Context,
	req *daemonrpc.ListOORSessionsRequest) (
	*daemonrpc.ListOORSessionsResponse, error) {

	if req == nil {
		req = &daemonrpc.ListOORSessionsRequest{}
	}

	resp, err := c.daemon.ListOORSessions(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list oor sessions: %w", err)
	}

	return resp, nil
}

// NewAddress allocates a fresh boarding address from the daemon wallet.
func (c *Client) NewAddress(ctx context.Context) (
	*daemonrpc.NewAddressResponse, error) {

	resp, err := c.daemon.NewAddress(ctx, &daemonrpc.NewAddressRequest{})
	if err != nil {
		return nil, fmt.Errorf("create boarding address: %w", err)
	}

	return resp, nil
}

// NewReceiveScript allocates and registers a fresh receive script with the
// daemon and indexer.
func (c *Client) NewReceiveScript(ctx context.Context, label string) (
	*daemonrpc.NewReceiveScriptResponse, error) {

	resp, err := c.daemon.NewReceiveScript(ctx,
		&daemonrpc.NewReceiveScriptRequest{
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
func (c *Client) AllocateReceiveScript(ctx context.Context,
	label string) (*ReceiveInfo, error) {

	resp, err := c.NewReceiveScript(ctx, label)
	if err != nil {
		return nil, err
	}

	return newReceiveInfo(resp)
}

// GetIndexedVTXOByPkScript asks the daemon's indexer client for the first VTXO
// matching the supplied script and status filters. Passing nil uses the
// daemon defaults.
func (c *Client) GetIndexedVTXOByPkScript(ctx context.Context,
	req *daemonrpc.GetIndexedVTXOByPkScriptRequest) (
	*daemonrpc.GetIndexedVTXOByPkScriptResponse, error) {

	if req == nil {
		req = &daemonrpc.GetIndexedVTXOByPkScriptRequest{}
	}

	resp, err := c.daemon.GetIndexedVTXOByPkScript(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get indexed vtxo by script: %w", err)
	}

	return resp, nil
}

// GetIndexedOORSessionByTxid asks the daemon's indexer client for the indexed
// OOR session matching the supplied spent-script proof and session txid.
// Passing nil uses the daemon defaults.
func (c *Client) GetIndexedOORSessionByTxid(ctx context.Context,
	req *daemonrpc.GetIndexedOORSessionByTxidRequest) (
	*daemonrpc.GetIndexedOORSessionByTxidResponse, error) {

	if req == nil {
		req = &daemonrpc.GetIndexedOORSessionByTxidRequest{}
	}

	resp, err := c.daemon.GetIndexedOORSessionByTxid(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("get indexed oor session: %w", err)
	}

	return resp, nil
}

// FindLiveVTXOByPkScript returns the first live indexed VTXO matching the
// supplied output script.
func (c *Client) FindLiveVTXOByPkScript(ctx context.Context,
	pkScript []byte) (*VTXOInfo, error) {

	return c.findIndexedVTXOByPkScript(
		ctx, pkScript, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
}

// FindSpentVTXOByPkScript returns the first spent indexed VTXO matching the
// supplied output script.
func (c *Client) FindSpentVTXOByPkScript(ctx context.Context,
	pkScript []byte) (*VTXOInfo, error) {

	return c.findIndexedVTXOByPkScript(
		ctx, pkScript, daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
}

// findIndexedVTXOByPkScript queries the daemon's authoritative indexer view
// for one VTXO matching the supplied script and status.
func (c *Client) findIndexedVTXOByPkScript(ctx context.Context,
	pkScript []byte, status daemonrpc.VTXOStatus) (*VTXOInfo, error) {

	resp, err := c.GetIndexedVTXOByPkScript(ctx,
		&daemonrpc.GetIndexedVTXOByPkScriptRequest{
			PkScript: append([]byte(nil), pkScript...),
			StatusFilter: []daemonrpc.VTXOStatus{
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
		&daemonrpc.GetIndexedOORSessionByTxidRequest{
			PkScript:    append([]byte(nil), pkScript...),
			SessionTxid: sessionHash,
		},
	)
	if err != nil {
		return nil, err
	}

	return newIndexedOORSessionInfo(resp), nil
}

// ListLocalOORSessions returns typed local OOR session summaries using the
// supplied filters.
func (c *Client) ListLocalOORSessions(ctx context.Context,
	req ListOORSessionsRequest) ([]OORSessionInfo, error) {

	direction, err := oorSessionDirectionToProto(req.Direction)
	if err != nil {
		return nil, err
	}

	resp, err := c.ListOORSessions(ctx, &daemonrpc.ListOORSessionsRequest{
		PendingOnly:    req.PendingOnly,
		Direction:      direction,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}

	sessions := make([]OORSessionInfo, 0, len(resp.GetSessions()))
	for _, rpcSession := range resp.GetSessions() {
		session, err := newOORSessionInfo(rpcSession)
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, *session)
	}

	return sessions, nil
}

// ListPendingOORSessions returns all locally pending OOR session summaries.
func (c *Client) ListPendingOORSessions(
	ctx context.Context) ([]OORSessionInfo, error) {

	return c.ListLocalOORSessions(ctx, ListOORSessionsRequest{
		PendingOnly: true,
	})
}

// ListOORSessionsByIdempotencyKey returns locally persisted OOR sessions
// tagged with the supplied idempotency key.
func (c *Client) ListOORSessionsByIdempotencyKey(ctx context.Context,
	idempotencyKey string) ([]OORSessionInfo, error) {

	if idempotencyKey == "" {
		return nil, fmt.Errorf("idempotency key must be provided")
	}

	return c.ListLocalOORSessions(ctx, ListOORSessionsRequest{
		IdempotencyKey: idempotencyKey,
	})
}

// SendVTXO submits an in-round send request through the daemon. Passing nil
// uses an empty request.
func (c *Client) SendVTXO(ctx context.Context,
	req *daemonrpc.SendVTXORequest) (*daemonrpc.SendVTXOResponse, error) {

	if req == nil {
		req = &daemonrpc.SendVTXORequest{}
	}

	resp, err := c.daemon.SendVTXO(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("send vtxo: %w", err)
	}

	return resp, nil
}

// SendOOR submits an out-of-round send request through the daemon. Passing nil
// uses an empty request.
func (c *Client) SendOOR(ctx context.Context,
	req *daemonrpc.SendOORRequest) (*daemonrpc.SendOORResponse, error) {

	if req == nil {
		req = &daemonrpc.SendOORRequest{}
	}

	resp, err := c.daemon.SendOOR(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("send oor transfer: %w", err)
	}

	return resp, nil
}

// SendOORWithPolicy sends one OOR transfer to a semantic policy-backed
// destination and returns the resulting OOR session id.
func (c *Client) SendOORWithPolicy(ctx context.Context, amountSat int64,
	recipientPolicyTemplate []byte) (string, error) {

	return c.SendOORWithPolicyAndKey(
		ctx, amountSat, recipientPolicyTemplate, "",
	)
}

// SendOORWithPolicyAndKey sends one OOR transfer to a semantic policy-backed
// destination using the supplied idempotency key and returns the resulting OOR
// session id.
func (c *Client) SendOORWithPolicyAndKey(ctx context.Context,
	amountSat int64, recipientPolicyTemplate []byte,
	idempotencyKey string) (string, error) {

	resp, err := c.SendOOR(ctx, &daemonrpc.SendOORRequest{
		Recipient: &daemonrpc.Output{
			Destination: &daemonrpc.Output_PolicyTemplate{
				PolicyTemplate: append(
					[]byte(nil), recipientPolicyTemplate...,
				),
			},
			AmountSat: amountSat,
		},
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return "", err
	}

	return resp.GetSessionId(), nil
}

// SendOORWithCustomInputs sends one OOR transfer using caller-specified
// inputs and a standard x-only pubkey destination.
func (c *Client) SendOORWithCustomInputs(ctx context.Context,
	recipientPubKey []byte, amountSat int64,
	inputs []CustomOORInput) (string, error) {

	rpcInputs := make([]*daemonrpc.CustomOORInput, len(inputs))
	for i := range inputs {
		rpcInputs[i] = &daemonrpc.CustomOORInput{
			Outpoint: inputs[i].Outpoint,
			VtxoPolicyTemplate: append(
				[]byte(nil), inputs[i].VTXOPolicyTemplate...,
			),
			SpendPath: append([]byte(nil), inputs[i].SpendPath...),
			AmountSat: inputs[i].AmountSat,
			PkScript:  append([]byte(nil), inputs[i].PkScript...),
		}
	}

	resp, err := c.SendOOR(ctx, &daemonrpc.SendOORRequest{
		Recipient: &daemonrpc.Output{
			Destination: &daemonrpc.Output_Pubkey{
				Pubkey: append([]byte(nil), recipientPubKey...),
			},
			AmountSat: amountSat,
		},
		CustomInputs: rpcInputs,
	})
	if err != nil {
		return "", err
	}

	return resp.GetSessionId(), nil
}

// ListLiveVTXOs returns the daemon's live spendable VTXOs as typed SDK-owned
// models.
func (c *Client) ListLiveVTXOs(ctx context.Context) ([]VTXOInfo, error) {
	return c.listVTXOsByStatus(
		ctx, daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)
}

// ListSpentVTXOs returns the daemon's locally tracked spent VTXOs as typed
// SDK-owned models.
func (c *Client) ListSpentVTXOs(ctx context.Context) ([]VTXOInfo, error) {
	return c.listVTXOsByStatus(
		ctx, daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
}

// listVTXOsByStatus loads one daemon VTXO status bucket and converts it into
// typed SDK-owned models.
func (c *Client) listVTXOsByStatus(ctx context.Context,
	status daemonrpc.VTXOStatus) ([]VTXOInfo, error) {

	resp, err := c.ListVTXOs(ctx, &daemonrpc.ListVTXOsRequest{
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
	req *daemonrpc.RefreshVTXOsRequest) (
	*daemonrpc.RefreshVTXOsResponse, error) {

	if req == nil {
		req = &daemonrpc.RefreshVTXOsRequest{}
	}

	resp, err := c.daemon.RefreshVTXOs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("refresh vtxos: %w", err)
	}

	return resp, nil
}

// Board tells the daemon to register any confirmed boarding UTXOs in the next
// round.
func (c *Client) Board(ctx context.Context) (
	*daemonrpc.BoardResponse, error) {

	resp, err := c.daemon.Board(ctx, &daemonrpc.BoardRequest{})
	if err != nil {
		return nil, fmt.Errorf("board confirmed utxos: %w", err)
	}

	return resp, nil
}

// ListRounds returns the daemon's current round FSM snapshots. Passing nil
// uses the daemon defaults.
func (c *Client) ListRounds(ctx context.Context,
	req *daemonrpc.ListRoundsRequest) (
	*daemonrpc.ListRoundsResponse, error) {

	if req == nil {
		req = &daemonrpc.ListRoundsRequest{}
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
	grpc.ServerStreamingClient[daemonrpc.WatchRoundsResponse], error) {

	stream, err := c.daemon.WatchRounds(ctx,
		&daemonrpc.WatchRoundsRequest{})
	if err != nil {
		return nil, fmt.Errorf("watch rounds: %w", err)
	}

	return stream, nil
}

// EstimateFee asks the daemon to proxy an operator fee estimate for the
// supplied operation parameters. Passing nil uses an empty request.
func (c *Client) EstimateFee(ctx context.Context,
	req *daemonrpc.EstimateFeeRequest) (
	*daemonrpc.EstimateFeeResponse, error) {

	if req == nil {
		req = &daemonrpc.EstimateFeeRequest{}
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
	req *daemonrpc.GetFeeHistoryRequest) (
	*daemonrpc.GetFeeHistoryResponse, error) {

	if req == nil {
		req = &daemonrpc.GetFeeHistoryRequest{}
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
		return nil, fmt.Errorf(
			"decode %s public key hex: %w", field, err,
		)
	}

	pubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse %s public key: %w", field, err)
	}

	return pubKey, nil
}

// newVTXOInfo converts one daemon protobuf VTXO into the SDK-owned typed
// model, decoding any hex-encoded binary fields along the way.
func newVTXOInfo(vtxo *daemonrpc.VTXO) (*VTXOInfo, error) {
	pkScript, err := hex.DecodeString(vtxo.GetPkScript())
	if err != nil {
		return nil, fmt.Errorf("decode vtxo pk_script: %w", err)
	}

	checkpoints := make([][]byte, 0, len(vtxo.GetOorFinalCheckpointPsbts()))
	for i := range vtxo.GetOorFinalCheckpointPsbts() {
		checkpoints = append(checkpoints, append(
			[]byte(nil), vtxo.GetOorFinalCheckpointPsbts()[i]...,
		))
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
	}, nil
}

// newReceiveInfo converts one daemon protobuf receive-script response
// into the SDK-owned typed model.
func newReceiveInfo(resp *daemonrpc.NewReceiveScriptResponse) (
	*ReceiveInfo, error) {

	pkScript, err := hex.DecodeString(resp.GetPkScriptHex())
	if err != nil {
		return nil, fmt.Errorf("decode receive pk_script: %w", err)
	}

	pubKeyXOnly, err := hex.DecodeString(resp.GetPubkeyXonlyHex())
	if err != nil {
		return nil, fmt.Errorf(
			"decode receive x-only pubkey: %w", err,
		)
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
	resp *daemonrpc.GetIndexedOORSessionByTxidResponse,
) *IndexedOORSessionInfo {

	checkpoints := make([][]byte, 0, len(resp.GetCheckpointPsbts()))
	for i := range resp.GetCheckpointPsbts() {
		checkpoints = append(checkpoints, append(
			[]byte(nil), resp.GetCheckpointPsbts()[i]...,
		))
	}

	return &IndexedOORSessionInfo{
		ArkPSBT:         append([]byte(nil), resp.GetArkPsbt()...),
		CheckpointPSBTs: checkpoints,
	}
}

// newOORSessionInfo converts one daemon protobuf local OOR session summary
// into the SDK-owned typed model.
func newOORSessionInfo(
	session *daemonrpc.OORSessionSummary) (*OORSessionInfo, error) {

	direction, err := oorSessionDirectionFromProto(session.GetDirection())
	if err != nil {
		return nil, err
	}

	retryAfterMS := session.GetRetryAfterMs()
	retryAfter := time.Duration(retryAfterMS) * time.Millisecond
	inputOutpoints := append([]string(nil), session.GetInputOutpoints()...)

	return &OORSessionInfo{
		SessionID:      session.GetSessionId(),
		Direction:      direction,
		Phase:          session.GetPhase(),
		Pending:        session.GetPending(),
		RetryAfter:     retryAfter,
		RetryReason:    session.GetRetryReason(),
		IdempotencyKey: session.GetIdempotencyKey(),
		InputOutpoints: inputOutpoints,
		InputAmountSat: session.GetInputAmountSat(),
		RecipientCount: session.GetRecipientCount(),
	}, nil
}

// oorSessionDirectionToProto converts the SDK direction filter to the daemon
// protobuf enum.
func oorSessionDirectionToProto(
	direction OORSessionDirection) (daemonrpc.OORSessionDirection, error) {

	allDirection := daemonrpc.OORSessionDirection_OOR_SESSION_DIRECTION_ALL
	outgoingDirection := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incomingDirection := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING

	switch direction {
	case OORSessionDirectionAll:
		return allDirection, nil

	case OORSessionDirectionOutgoing:
		return outgoingDirection, nil

	case OORSessionDirectionIncoming:
		return incomingDirection, nil

	default:
		return 0, fmt.Errorf(
			"unknown OOR session direction %d", direction,
		)
	}
}

// oorSessionDirectionFromProto converts the daemon protobuf direction enum to
// the SDK direction value.
func oorSessionDirectionFromProto(
	direction daemonrpc.OORSessionDirection) (OORSessionDirection, error) {

	allDirection := daemonrpc.OORSessionDirection_OOR_SESSION_DIRECTION_ALL
	outgoingDirection := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	incomingDirection := daemonrpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING

	switch direction {
	case allDirection:
		return OORSessionDirectionAll, nil

	case outgoingDirection:
		return OORSessionDirectionOutgoing, nil

	case incomingDirection:
		return OORSessionDirectionIncoming, nil

	default:
		return 0, fmt.Errorf("unknown daemon OOR session direction %d",
			direction,
		)
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
