# VTXO AST: Doc-First Design

## Overview
- **Title:** VTXO AST: structural scripts, validation, codegen
- **Owners:** (fill)
- **Status:** Draft
- **Related:** PRs/issues (fill)

This PR introduces a VTXO scripting model built around a typed AST plus a
compiler-style pipeline: parse (optional) → validate → codegen. The key outcome
is that both the operator and receivers can mechanically verify that a VTXO’s
spend paths satisfy Ark requirements (checkpointing, OOR handling) without
executing scripts in a fake VM context.

## Problem & Motivation
Ark needs strong, auditable guarantees about what spend paths exist in a VTXO
and what they allow:
- **Checkpointing** requires a collaborative path that necessarily includes the
  operator/signer, otherwise operator-driven transitions can stall or be
  bypassed.
- **OOR flows** and safety exits require a provably present, CSV-gated exit path
  that cannot be “shadowed” or invalidated by additional ungated paths.

Past approaches that “evaluate conditions” by running scripts in an ad-hoc VM
are brittle: correct results depend on transaction context (sequence, locktime,
prevout amounts/scripts), and partial evaluation cannot prove protocol-level
properties (e.g., “the signer key is required on every collab path”).

### Non-goals
- No new on-chain protocol changes.
- No new covenant/introspection opcode support.
- No attempt to support arbitrary raw-script “escape hatches” that cannot be
  structurally validated.

## Scope
### In scope
- A new `lib/vtxoast` package for:
  - AST node types for the supported tapscript subset.
  - Validation rules for Ark-required invariants.
  - Codegen for tapscript leaves and taproot tree materialization helpers.
  - Spend metadata (`SpendInfo`) for signing/witness assembly.
- A default VTXO builder (exit + collab) backed by the AST.
- A structured API for adding custom paths while preserving Ark invariants.
- Tests that prove correctness via roundtrips, invariants, and known-good
  vectors.

### Out of scope
- Refactoring unrelated wallet/tx code.
- Broad, generic script execution or arbitrary script evaluation helpers.

## Requirements & Constraints
### Functional requirements
- Both operator and receivers can validate a VTXO script before accepting it.
- A VTXO always has:
  - at least one **exit** path, CSV-gated, owner-only keys.
  - at least one **collab** path that requires the operator/signer key.
- Custom paths are supported but cannot remove or bypass required Ark paths.

### Security and compatibility
- No keypath “escape hatch”: internal key is fixed to a NUMS/unspendable key so
  all spends go through validated script paths.
- Reject unknown/unsupported opcodes unless explicitly modeled in the AST.
- Ensure deterministic encoding and stable taproot output keys.

## High-Level Approach
Model VTXO scripts as role-tagged leaves in an AST. Validation is a structural
pass over the AST that enforces Ark invariants. Codegen materializes tapscript
and the taproot tree, returning spend metadata for each leaf so witness
construction is mechanical.

This replaces “try to interpret script behavior” with “prove the required
structure exists and cannot be bypassed”.

## Design Details

### Path roles
Every leaf declares a role:
- `exit`: a user safety exit (must be CSV-gated; owner-only keys).
- `collab`: a collaborative path (must include signer key).
- `custom`: an application-specific path. By default it must be CSV-wrapped and
  must not bypass required `exit`/`collab` invariants.

Role rules are strict:
- At least one `exit` and one `collab` leaf are required.
- Multiple exits are allowed only if each satisfies exit constraints.
- Any leaf missing a role or violating its role constraints fails validation.

### AST API (creation)
The AST models a restricted, explicit tapscript subset.

```go
package vtxoast

type Role uint8

const (
	RoleExit Role = iota
	RoleCollab
	RoleCustom
)

type Script struct {
	InternalKey *btcec.PublicKey
	Leaves      []Leaf
}

type Leaf struct {
	Name string
	Role Role
	Expr Expr
}

type Expr interface{ expr() }

// Gates.
type CSV struct {
	Locktime RelativeLocktime
	Inner    Expr
}

type CLTV struct {
	Locktime AbsoluteLocktime
	Inner    Expr
}

// Spend policy nodes.
type Multisig struct {
	Keys []*btcec.PublicKey
	Type MultisigType
}

type Checksig struct{ Key *btcec.PublicKey }

// Conditions are typed (no raw "evaluate to bool" VM helper).
// PreimageHash160 is an example condition node.
type PreimageHash160 struct{ Hash20 [20]byte }

// Default VTXO: CSV(owner) exit + (owner+signer) collab.
func NewDefaultVTXO(owner, signer *btcec.PublicKey,
	exitDelay RelativeLocktime) *Script

// Optional: parse a leaf script into an AST subtree for inspection.
func ParseExpr(script []byte) (Expr, error)

// Codegen for a leaf subtree.
func ScriptBytes(expr Expr) ([]byte, error)
```

Developer ergonomics:
- Provide helpers for common patterns (default VTXO, CSV-wrapped custom leaf).
- Custom leaves are built from typed nodes; if parsing existing scripts is
  needed, `ParseExpr` converts a supported subset into AST for validation.

### AST API (validation)
Validation is a separate pass that does not execute scripts.

```go
type ValidateParams struct {
	SignerKey    *btcec.PublicKey
	MinExitDelay RelativeLocktime
}

func (s *Script) Validate(params ValidateParams) error
```

Validation enforces:
- Required roles exist (≥1 exit, ≥1 collab).
- `exit` leaves:
  - must be CSV-gated.
  - must only use owner keys (no signer key).
  - must meet `MinExitDelay`.
- `collab` leaves:
  - must include `SignerKey` in their required keys.
- `custom` leaves:
  - must be CSV-gated by default.
  - must not create an ungated spend that bypasses required roles.
- Opcode policy: only AST-modeled opcode forms are allowed.

### Spend metadata for signing/witness generation
Codegen returns spend metadata per leaf so signing remains mechanical and
independent of any fake VM evaluation.

```go
type SpendInfo struct {
	Script       []byte
	ControlBlock []byte
	LeafHash     []byte
	LeafVersion  byte
	LeafIndex    int

	// Derived requirements to set on the spending tx/input.
	Sequence uint32 // if CSV applies
	LockTime uint32 // if CLTV applies
}

func (s *Script) SpendInfo(name string) (*SpendInfo, error)
```

Witness construction then becomes “fill tx context (sequence/locktime), compute
sighash, sign, assemble witness stack” using the returned `Script` and
`ControlBlock`.

### Operator/receiver verification UX
The operator and receiver should both be able to:
- Run `Validate(params)` and get a descriptive failure if the script violates
  Ark invariants.
- Produce a short, human-readable summary of the script roles/keys/locktimes
  (printer/inspector) for auditing.

## Implementation Strategy
1) Add `lib/vtxoast` types and minimal codegen for default VTXO.
2) Implement validation rules and error taxonomy.
3) Implement spend info materialization (taproot tree building + per-leaf
   control blocks + derived sequence/locktime).
4) Add inspector/printer output for audits.
5) Wire default-VTXO call sites to the AST builder and validator.
6) Add optional parsing for supported leaf subsets if needed for custom scripts.

If implementation learning changes scope or rules, update this document first
in a `[Document] ...` commit.

## Testing Strategy
Unit tests:
- AST → script determinism for known inputs.
- Validation:
  - missing exit/collab rejected.
  - exits must be CSV-gated and owner-only.
  - collab must include signer key.
  - custom must not create ungated bypasses.
- Spend info:
  - correct leaf hashes/control blocks for each leaf.
  - derived sequence/locktime matches the AST.

Property tests (rapid):
- ASTs that satisfy invariants roundtrip and validate.
- ASTs that violate invariants fail with the expected error class.

Integration tests:
- Existing tree/tx/wallet flows continue to work when using AST-backed helpers.

Commands:
- `make unit pkg=./lib/... timeout=5m`
- `make lint`

## Rollout & Ops
- No runtime flags expected; this is library-side validation/codegen.
- Backout: revert to prior builder if needed (documented by API boundaries).

## Risks & Open Questions
- What is the minimal supported script subset for custom paths (typed nodes
  only vs allow parsing an approved subset)?
- Do we require every `custom` path to include the signer key, or only those
  declared collaborative? (default: signer required if role is `collab`.)

## Acceptance Criteria
- `lib/vtxoast` exists with the creation/validation/codegen APIs above.
- All validation invariants are enforced and covered by tests.
- `SpendInfo` is sufficient to construct witnesses without ad-hoc script eval.
- Unit + property tests pass; integration tests remain green.
