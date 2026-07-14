package unrollpolicy

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vhtlcrecovery"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// estimatedVHTLCExitVBytes is a conservative virtual-size estimate for
	// one final vHTLC exit spend. The signed transaction is persisted
	// before broadcast, so exact fee accounting can be refined in a later
	// worker pass without changing restart safety.
	estimatedVHTLCExitVBytes = 260
)

// RecoveryJobLoader loads a durable recovery job by id.
type RecoveryJobLoader interface {
	// GetRecovery loads one recovery job row.
	GetRecovery(ctx context.Context,
		id string) (*vhtlcrecovery.RecoveryJob, error)
}

// PreimageResolver loads the raw preimage owned by the swap row.
type PreimageResolver interface {
	// ResolvePreimage returns the preimage for a recovery job. The
	// resolver must verify the returned preimage matches preimageHash.
	ResolvePreimage(ctx context.Context, swapID []byte,
		preimageHash lntypes.Hash) (lntypes.Preimage, error)
}

// PreimageResolverRegistry is a concurrency-safe indirection point for the
// daemon's optional swap runtime. The unroll subsystem is initialized before
// the swapruntime subserver registers its swap store, so the recovery policy
// resolver holds this registry and the subserver installs the concrete
// swap-owned preimage resolver later in startup.
type PreimageResolverRegistry struct {
	mu       sync.RWMutex
	resolver PreimageResolver
}

// SetResolver installs or replaces the concrete swap-owned preimage resolver.
// Passing nil deliberately disables claim recovery resolution until another
// resolver is registered.
func (r *PreimageResolverRegistry) SetResolver(resolver PreimageResolver) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.resolver = resolver
}

// ResolvePreimage delegates to the currently registered resolver. If no
// resolver has been installed, claim recovery fails closed instead of building
// a spend without the swap-owned secret.
func (r *PreimageResolverRegistry) ResolvePreimage(ctx context.Context,
	swapID []byte, preimageHash lntypes.Hash) (lntypes.Preimage, error) {

	if r == nil {
		return lntypes.Preimage{}, fmt.Errorf("preimage resolver " +
			"registry must be provided")
	}

	r.mu.RLock()
	resolver := r.resolver
	r.mu.RUnlock()
	if resolver == nil {
		return lntypes.Preimage{}, fmt.Errorf("preimage resolver is " +
			"not registered")
	}

	return resolver.ResolvePreimage(ctx, swapID, preimageHash)
}

// ExitSpendPolicyResolver resolves durable vHTLC recovery policy refs for
// unroll. It is deliberately small: the resolver loads the recovery row by id,
// verifies that the requested policy kind matches the row, and then constructs
// the concrete script-path spend policy that the generic unroll actor can use.
type ExitSpendPolicyResolver struct {
	Jobs     RecoveryJobLoader
	Preimage PreimageResolver
}

// SupportsKind reports the durable recovery policy kinds this resolver
// covers. The unroll registry consults this at boot so a persisted vHTLC
// recovery row never starts the FSM under a resolver that cannot reconstruct
// it.
func (r ExitSpendPolicyResolver) SupportsKind(kind unroll.ExitPolicyKind) bool {
	switch string(kind) {
	case vhtlcrecovery.ExitPolicyKindClaim,
		vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver:
		return true
	}

	return false
}

// ResolveExitSpendPolicy reconstructs the vHTLC exit spend policy for unroll.
// Claim policies additionally resolve the swap-owned preimage and verify it
// against the row's preimage hash before returning the spend policy. Refund
// without receiver does not need a preimage resolver, which keeps sender-side
// recovery independent from claim-only secret material.
func (r ExitSpendPolicyResolver) ResolveExitSpendPolicy(ctx context.Context,
	req unroll.ExitSpendPolicyRequest) (unroll.ExitSpendPolicy, error) {

	if r.Jobs == nil {
		return nil, fmt.Errorf("recovery job loader must be provided")
	}
	if req.Ref == "" {
		return nil, fmt.Errorf("vhtlc recovery policy ref is required")
	}

	job, err := r.Jobs.GetRecovery(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	if job.ExitPolicyKind != string(req.Kind) {
		return nil, fmt.Errorf("recovery job policy kind %q does not "+
			"match request kind %q", job.ExitPolicyKind, req.Kind)
	}

	switch string(req.Kind) {
	case vhtlcrecovery.ExitPolicyKindClaim:
		preimageHash, err := preimageHashFromJob(*job)
		if err != nil {
			return nil, err
		}

		preimage, err := r.resolveClaimPreimage(
			ctx, *job, preimageHash,
		)
		if err != nil {
			return nil, err
		}

		return NewClaimExitSpendPolicy(*job, preimage)

	case vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver:
		return NewRefundWithoutReceiverExitSpendPolicy(*job)

	default:
		return nil, fmt.Errorf("unknown vhtlc exit policy kind: %s",
			req.Kind)
	}
}

// resolveClaimPreimage returns the secret needed for the unilateral claim
// witness. Cross-process callers may persist it on the recovery row during
// escalation; in-process swap runtimes keep it in their swap store and provide
// it through the registered resolver.
func (r ExitSpendPolicyResolver) resolveClaimPreimage(ctx context.Context,
	job vhtlcrecovery.RecoveryJob, preimageHash lntypes.Hash) (
	lntypes.Preimage, error) {

	if len(job.ClaimPreimage) != 0 {
		preimage, err := lntypes.MakePreimage(job.ClaimPreimage)
		if err != nil {
			return lntypes.Preimage{}, fmt.Errorf("decode claim "+
				"preimage: %w", err)
		}
		if !preimage.Matches(preimageHash) {
			return lntypes.Preimage{}, fmt.Errorf("claim " +
				"preimage does not match recovery hash")
		}

		return preimage, nil
	}
	if r.Preimage == nil {
		return lntypes.Preimage{}, fmt.Errorf("preimage resolver " +
			"must be provided for vhtlc claim recovery")
	}

	return r.Preimage.ResolvePreimage(ctx, job.SwapID, preimageHash)
}

// VHTLCExitSpendPolicy builds the final script-path spend for one recovery
// action. The policy is reconstructed from explicit recovery-row columns, then
// handed to unroll as a generic final-spend strategy. It does not persist or
// broadcast transactions; unroll owns those restart-safety responsibilities.
type VHTLCExitSpendPolicy struct {
	job       vhtlcrecovery.RecoveryJob
	policy    *arkscript.VHTLCPolicy
	spendPath *arkscript.SpendPath
	keyDesc   keychain.KeyDescriptor
}

// NewClaimExitSpendPolicy creates a preimage claim exit policy. The caller
// supplies the raw preimage from swap-owned state, and this constructor
// verifies it against the durable preimage hash before deriving the unilateral
// claim spend path. The raw preimage is only kept in the witness material owned
// by the spend path; it is not written back into the recovery row.
func NewClaimExitSpendPolicy(job vhtlcrecovery.RecoveryJob,
	preimage lntypes.Preimage) (*VHTLCExitSpendPolicy, error) {

	policy, err := policyFromJob(job)
	if err != nil {
		return nil, err
	}
	if !preimage.Matches(policy.PreimageHash) {
		return nil, fmt.Errorf("preimage does not match recovery hash")
	}

	spendPath, err := policy.CompiledPolicy.SpendPathForNode(
		policy.UnilateralClaimClosure, [][]byte{
			preimage[:],
		},
	)
	if err != nil {
		return nil, fmt.Errorf("unilateral claim spend path: %w", err)
	}

	return newVHTLCExitSpendPolicy(job, policy, spendPath)
}

// NewRefundWithoutReceiverExitSpendPolicy creates a sender-only refund policy.
// This path models the unilateral leaf used after on-chain materialization when
// receiver cooperation is unavailable. The resulting spend must satisfy both
// the Ark CSV delay and the vHTLC refund CLTV locktime.
func NewRefundWithoutReceiverExitSpendPolicy(job vhtlcrecovery.RecoveryJob) (
	*VHTLCExitSpendPolicy, error) {

	policy, err := policyFromJob(job)
	if err != nil {
		return nil, err
	}

	spendPath, err := policy.CompiledPolicy.SpendPathForNode(
		policy.UnilateralRefundWithoutReceiverClosure, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("unilateral refund without receiver "+
			"spend path: %w", err)
	}

	return newVHTLCExitSpendPolicy(job, policy, spendPath)
}

// newVHTLCExitSpendPolicy validates action-specific context and returns a
// concrete policy. It is shared by both public constructors so direct callers
// get the same action/policy-kind checks as resolver-driven callers.
func newVHTLCExitSpendPolicy(job vhtlcrecovery.RecoveryJob,
	policy *arkscript.VHTLCPolicy,
	spendPath *arkscript.SpendPath) (*VHTLCExitSpendPolicy, error) {

	if err := spendPath.Validate(); err != nil {
		return nil, err
	}

	key, err := signingKey(job)
	if err != nil {
		return nil, err
	}

	return &VHTLCExitSpendPolicy{
		job:       job,
		policy:    policy,
		spendPath: spendPath,
		keyDesc: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamily(job.SignerKeyFamily),
				Index:  uint32(job.SignerKeyIndex),
			},
			PubKey: key,
		},
	}, nil
}

// Kind returns the durable policy kind persisted on the recovery and unroll
// rows. Returning the durable value keeps unroll's stored `(kind, ref)`
// identity aligned with the concrete policy that was reconstructed from it.
func (p *VHTLCExitSpendPolicy) Kind() unroll.ExitPolicyKind {
	if p == nil {
		return ""
	}

	return unroll.ExitPolicyKind(p.job.ExitPolicyKind)
}

// CSVDelay returns the raw block delay unroll waits before spending. The value
// is used for scheduler/waiting logic only: the final transaction still uses
// spendPath.RequiredSequence so the script-derived sequence is the on-wire
// source of truth.
func (p *VHTLCExitSpendPolicy) CSVDelay() uint32 {
	if p == nil {
		return 0
	}

	switch p.job.Action {
	case vhtlcrecovery.ActionClaim:
		return uint32(p.job.UnilateralClaimDelay)

	case vhtlcrecovery.ActionRefundWithoutReceiver:
		return uint32(p.job.UnilateralRefundWithoutReceiverDelay)
	}

	// Policy construction validates the action, so reaching this point
	// means a new action was added without updating the policy adapter.
	panic(fmt.Sprintf("unhandled vhtlc recovery action %s", p.job.Action))
}

// RequiredLockTime returns the absolute nLockTime the spend path demands. For
// the refund-without-receiver action the value is the durable refund locktime
// from the recovery row; for the claim action it is zero because the claim
// leaf is gated only by CSV plus preimage knowledge.
func (p *VHTLCExitSpendPolicy) RequiredLockTime() uint32 {
	if p == nil || p.spendPath == nil {
		return 0
	}

	return p.spendPath.RequiredLockTime
}

// ValidateTarget verifies the materialized output matches the vHTLC policy.
// Recovery must fail closed if unroll materializes a different script, because
// spending a mismatched target would mean the durable recovery row no longer
// describes the on-chain output being swept.
func (p *VHTLCExitSpendPolicy) ValidateTarget(target *wire.TxOut) error {
	switch {
	case p == nil:
		return fmt.Errorf("vhtlc exit policy must be provided")

	case p.policy == nil:
		return fmt.Errorf("vhtlc policy must be provided")

	case target == nil:
		return fmt.Errorf("target output must be provided")

	case target.Value <= 0:
		return fmt.Errorf("target output value must be positive")
	}

	pkScript, err := p.policy.PkScript()
	if err != nil {
		return fmt.Errorf("vhtlc pkscript: %w", err)
	}
	if !bytes.Equal(target.PkScript, pkScript) {
		return fmt.Errorf("target output pkscript does not match " +
			"vhtlc policy")
	}

	return nil
}

// BuildSpendTx builds and signs the final vHTLC exit spend. It validates the
// target output, enforces the recovery fee cap, applies the script-derived
// sequence and locktime, and returns a fully witnessed transaction for unroll
// to persist before broadcast.
func (p *VHTLCExitSpendPolicy) BuildSpendTx(ctx context.Context,
	req unroll.ExitSpendRequest) (*wire.MsgTx, error) {

	// Unused today; kept for interface symmetry with policies that may
	// need cancellable wallet or store work.
	_ = ctx

	if err := p.ValidateTarget(req.TargetOutput); err != nil {
		return nil, err
	}
	if req.Signer == nil {
		return nil, fmt.Errorf("signer must be provided")
	}
	if len(req.DestinationPkScript) == 0 {
		return nil, fmt.Errorf("destination pkscript must be provided")
	}
	if req.FeeRateSatPerVByte <= 0 {
		return nil, fmt.Errorf("fee rate must be positive")
	}
	if err := p.validateFeeRate(req.FeeRateSatPerVByte); err != nil {
		return nil, err
	}

	// Refuse to construct a transaction the network would reject as
	// non-final. The actor plumbs CurrentHeight from its last persisted
	// height notification so this fires deterministically rather than
	// burning broadcast retries on a tx whose nLockTime is in the future.
	if p.spendPath.RequiredLockTime > 0 &&
		uint32(req.CurrentHeight) < p.spendPath.RequiredLockTime {
		return nil, fmt.Errorf("%w: locktime %d > current height %d",
			unroll.ErrExitSpendNotMatured,
			p.spendPath.RequiredLockTime, req.CurrentHeight)
	}

	tx := wire.NewMsgTx(arktx.TxVersion)
	tx.LockTime = p.spendPath.RequiredLockTime
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: req.TargetOutpoint,
		Sequence:         p.spendPath.RequiredSequence,
	})

	inputValue := btcutil.Amount(req.TargetOutput.Value)
	fee := btcutil.Amount(
		req.FeeRateSatPerVByte * estimatedVHTLCExitVBytes,
	)
	exitValue := inputValue - fee
	if exitValue <= 0 {
		return nil, fmt.Errorf("exit value %d not positive "+
			"after fee %d", exitValue, fee)
	}

	tx.AddTxOut(&wire.TxOut{
		Value:    int64(exitValue),
		PkScript: append([]byte(nil), req.DestinationPkScript...),
	})

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		req.TargetOutput.PkScript, req.TargetOutput.Value,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	signDesc := p.spendPath.BuildSignDescriptor(
		p.keyDesc, req.TargetOutput, sigHashes, prevFetcher, 0,
	)

	sig, err := req.Signer.SignOutputRaw(tx, signDesc)
	if err != nil {
		return nil, fmt.Errorf("sign vhtlc exit spend: %w", err)
	}

	witness, err := p.spendPath.SingleSigWitness(
		sig, txscript.SigHashDefault,
	)
	if err != nil {
		return nil, fmt.Errorf("vhtlc exit witness: %w", err)
	}
	tx.TxIn[0].Witness = witness

	return tx, nil
}

// validateFeeRate enforces the recovery row's fee cap. The unroll actor passes
// fee rates in sat/vByte, while recovery rows store the operator policy as
// sat/kw, so this converts before comparing to avoid unit drift at the
// boundary.
func (p *VHTLCExitSpendPolicy) validateFeeRate(feeRateSatPerVByte int64) error {
	feeRateSatPerKWeight := feeRateSatPerVByte * 250
	if feeRateSatPerKWeight <= int64(p.job.MaxFeeRateSatPerKWeight) {
		return nil
	}

	return fmt.Errorf("fee rate %d sat/kw exceeds cap %d sat/kw",
		feeRateSatPerKWeight, p.job.MaxFeeRateSatPerKWeight)
}

// policyFromJob reconstructs the vHTLC policy from durable row columns. Each
// field is decoded and range-checked before calling into arkscript so malformed
// SQL state fails with a recovery-specific error instead of a later script
// construction surprise.
func policyFromJob(job vhtlcrecovery.RecoveryJob) (*arkscript.VHTLCPolicy,
	error) {

	sender, err := parsePubKey(job.SenderPubkey, "sender")
	if err != nil {
		return nil, err
	}
	receiver, err := parsePubKey(job.ReceiverPubkey, "receiver")
	if err != nil {
		return nil, err
	}
	server, err := parsePubKey(job.ServerPubkey, "server")
	if err != nil {
		return nil, err
	}

	preimageHash, err := preimageHashFromJob(job)
	if err != nil {
		return nil, err
	}

	refundLocktime, err := uint32Field(
		job.RefundLocktime, "refund locktime",
	)
	if err != nil {
		return nil, err
	}
	claimDelay, err := uint32Field(
		job.UnilateralClaimDelay, "unilateral claim delay",
	)
	if err != nil {
		return nil, err
	}
	refundDelay, err := uint32Field(
		job.UnilateralRefundDelay, "unilateral refund delay",
	)
	if err != nil {
		return nil, err
	}
	rwrDelay, err := uint32Field(
		job.UnilateralRefundWithoutReceiverDelay,
		"unilateral refund without receiver delay",
	)
	if err != nil {
		return nil, err
	}

	return arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               server,
		PreimageHash:                         preimageHash,
		RefundLocktime:                       refundLocktime,
		UnilateralClaimDelay:                 claimDelay,
		UnilateralRefundDelay:                refundDelay,
		UnilateralRefundWithoutReceiverDelay: rwrDelay,
	})
}

// signingKey returns the key that signs the selected unilateral leaf. It also
// re-checks the action-to-policy-kind mapping so direct constructor callers
// cannot accidentally build a claim spend using a refund policy row, or the
// other way around.
func signingKey(job vhtlcrecovery.RecoveryJob) (*btcec.PublicKey, error) {
	var keyBytes []byte
	switch job.Action {
	case vhtlcrecovery.ActionClaim:
		if job.ExitPolicyKind != vhtlcrecovery.ExitPolicyKindClaim {
			return nil, fmt.Errorf("claim action has "+
				"policy kind %q", job.ExitPolicyKind)
		}
		keyBytes = job.ReceiverPubkey

	case vhtlcrecovery.ActionRefundWithoutReceiver:
		if job.ExitPolicyKind !=
			vhtlcrecovery.ExitPolicyKindRefundWithoutReceiver {
			return nil, fmt.Errorf("refund action has "+
				"policy kind %q", job.ExitPolicyKind)
		}
		keyBytes = job.SenderPubkey

	default:
		return nil, fmt.Errorf("unknown vhtlc recovery action: %s",
			job.Action)
	}

	return parsePubKey(keyBytes, "signing")
}

// parsePubKey parses a compressed public key from durable storage. The name is
// included in the error so operators and tests can distinguish which recovery
// column is malformed.
func parsePubKey(raw []byte, name string) (*btcec.PublicKey, error) {
	key, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s pubkey: %w", name, err)
	}

	return key, nil
}

// preimageHashFromJob decodes the stored payment hash. Recovery stores only
// the hash, not the raw preimage, so this helper enforces the exact fixed-size
// encoding before any claim path can ask the swap store for secret material.
func preimageHashFromJob(job vhtlcrecovery.RecoveryJob) (lntypes.Hash, error) {
	var hash lntypes.Hash
	if len(job.PreimageHash) != len(hash) {
		return hash, fmt.Errorf("preimage hash must be %d "+
			"bytes, got %d", len(hash), len(job.PreimageHash))
	}

	copy(hash[:], job.PreimageHash)

	return hash, nil
}

// uint32Field converts a signed SQL field into its script-domain value. SQL
// stores the values as signed integers for portability, while arkscript expects
// unsigned locktime/delay values; non-positive values are rejected before the
// cast to avoid accidental wraparound.
func uint32Field(value int32, name string) (uint32, error) {
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}

	return uint32(value), nil
}

// Compile-time interface checks.
var _ unroll.ExitSpendPolicy = (*VHTLCExitSpendPolicy)(nil)
var _ unroll.ExitSpendPolicyResolver = (*ExitSpendPolicyResolver)(nil)
