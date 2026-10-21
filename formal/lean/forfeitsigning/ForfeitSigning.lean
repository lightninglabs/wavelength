/-!
The receive-side forfeit signing authority model.

Opaque production values are represented by natural-number atoms. The proof
cares about identity equality, publication, and admission. Policy decoding,
transaction construction, and Schnorr signing remain implementation
obligations exercised outside this model.
-/
namespace ForfeitSigning

/-- FundingBinding is the immutable identity published after funding. -/
structure FundingBinding where
  paymentHash : Nat
  outpoint : Nat
  amount : Nat
  script : Nat
  policy : Nat
  policyPaymentHash : Nat
  deriving DecidableEq, Repr

/-- SigningRequest is the identity carried by mailbox transport. -/
structure SigningRequest where
  paymentHash : Nat
  outpoint : Nat
  amount : Nat
  script : Nat
  policy : Nat
  policyPaymentHash : Nat
  deriving DecidableEq, Repr

/-- Authority holds the funded binding only after its publication boundary. -/
structure Authority where
  published : Option FundingBinding
  deriving DecidableEq, Repr

/-- SigningContext is the exact authority/request pair admitted to the signer. -/
structure SigningContext where
  binding : FundingBinding
  request : SigningRequest
  deriving DecidableEq, Repr

/-- ExactBinding states every identity equality required before signing. -/
def ExactBinding (binding : FundingBinding) (request : SigningRequest) : Prop :=
  request.paymentHash = binding.paymentHash ∧
  request.outpoint = binding.outpoint ∧
  request.amount = binding.amount ∧
  request.script = binding.script ∧
  request.policy = binding.policy ∧
  request.policyPaymentHash = binding.policyPaymentHash ∧
  request.policyPaymentHash = request.paymentHash ∧
  binding.policyPaymentHash = binding.paymentHash

instance (binding : FundingBinding) (request : SigningRequest) :
    Decidable (ExactBinding binding request) := by
  unfold ExactBinding
  infer_instance

/-- admit returns a signing context only under published, exact authority. -/
def admit (authority : Authority) (request : SigningRequest) :
    Option SigningContext :=
  match authority.published with
  | none => none
  | some binding =>
      if ExactBinding binding request then
        some { binding, request }
      else
        none

/--
admit_sound is the load-bearing result: producing a signing context implies
that authority was published and every funded identity field matched.
-/
theorem admit_sound {authority : Authority} {request : SigningRequest}
    {context : SigningContext} (h : admit authority request = some context) :
    authority.published = some context.binding ∧
      context.request = request ∧
      ExactBinding context.binding context.request := by
  unfold admit at h
  split at h
  case h_1 => contradiction
  case h_2 binding authorityPublished =>
    split at h
    case isTrue exact =>
      cases h
      exact ⟨authorityPublished, rfl, exact⟩
    case isFalse => contradiction

/-- The unsafe request-derived rule models the historical failure mode. -/
def legacyAdmit (request : SigningRequest) : SigningContext :=
  {
    binding := {
      paymentHash := request.paymentHash
      outpoint := request.outpoint
      amount := request.amount
      script := request.script
      policy := request.policy
      policyPaymentHash := request.policyPaymentHash
    }
    request
  }

/-- exactBinding is the concrete baseline used by examples and bridge vectors. -/
def exactBinding : FundingBinding := {
  paymentHash := 1
  outpoint := 2
  amount := 3
  script := 4
  policy := 5
  policyPaymentHash := 1
}

/-- exactRequest matches exactBinding in every authority-bearing field. -/
def exactRequest : SigningRequest := {
  paymentHash := 1
  outpoint := 2
  amount := 3
  script := 4
  policy := 5
  policyPaymentHash := 1
}

/-- publishedAuthority is the safe funded baseline. -/
def publishedAuthority : Authority := { published := some exactBinding }

/-- unpublishedAuthority models delivery before funded authority exists. -/
def unpublishedAuthority : Authority := { published := none }

example : admit publishedAuthority exactRequest = some {
    binding := exactBinding, request := exactRequest
  } := by decide

example : admit unpublishedAuthority exactRequest = none := by decide

example : admit publishedAuthority { exactRequest with paymentHash := 9 } = none :=
  by decide

example : admit publishedAuthority { exactRequest with outpoint := 9 } = none :=
  by decide

example : admit publishedAuthority { exactRequest with amount := 9 } = none :=
  by decide

example : admit publishedAuthority { exactRequest with script := 9 } = none :=
  by decide

example : admit publishedAuthority { exactRequest with policy := 9 } = none :=
  by decide

example : admit publishedAuthority {
    exactRequest with policyPaymentHash := 9
  } = none := by decide

example : admit {
    published := some {
      exactBinding with policyPaymentHash := 9
    }
  } exactRequest = none := by decide

example : admit {
    published := some {
      exactBinding with policy := 9, policyPaymentHash := 9
    }
  } {
    exactRequest with policy := 9, policyPaymentHash := 9
  } = none := by decide

/--
The request-derived rule can manufacture apparently exact authority before
the receive session publishes anything. This witness fails the premise proved
for admit_sound and keeps the unsafe alternative explicit.
-/
theorem legacy_request_derived_counterexample :
    unpublishedAuthority.published = none ∧
      ExactBinding (legacyAdmit exactRequest).binding
        (legacyAdmit exactRequest).request := by
  decide

/-- BridgeCase is one executable admission decision shared with Go. -/
structure BridgeCase where
  name : String
  authority : Authority
  request : SigningRequest

/-- BridgeCase.accepted evaluates the proved admission function. -/
def BridgeCase.accepted (test : BridgeCase) : Bool :=
  (admit test.authority test.request).isSome

/-- bridgeCases covers publication and every individual binding equality. -/
def bridgeCases : List BridgeCase := [
  { name := "unpublished", authority := unpublishedAuthority,
    request := exactRequest },
  { name := "exact", authority := publishedAuthority,
    request := exactRequest },
  { name := "wrong_payment_hash", authority := publishedAuthority,
    request := { exactRequest with paymentHash := 9 } },
  { name := "wrong_outpoint", authority := publishedAuthority,
    request := { exactRequest with outpoint := 9 } },
  { name := "wrong_amount", authority := publishedAuthority,
    request := { exactRequest with amount := 9 } },
  { name := "wrong_script", authority := publishedAuthority,
    request := { exactRequest with script := 9 } },
  { name := "wrong_policy", authority := publishedAuthority,
    request := { exactRequest with policy := 9 } },
  { name := "wrong_policy_payment_hash",
    authority := {
      published := some {
        exactBinding with policy := 9, policyPaymentHash := 9
      }
    },
    request := {
      exactRequest with policy := 9, policyPaymentHash := 9
    } }
]

end ForfeitSigning
