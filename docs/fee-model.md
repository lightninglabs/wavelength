# Ark Fee Model

This document specifies the economic model used by the darepo operator to price
round operations. There are two price tiers:

1. **Capital-committing operations** (refresh, offboarding) — priced by the
   full formula below, including a liquidity fee that compensates the operator
   for the double-commitment window between refresh and old-round sweep.
2. **Non-committing operations** (boarding) — priced only by the on-chain
   share and operator margin. Boarding does not deploy new operator capital
   (the user brings on-chain BTC), so it does not incur a liquidity fee. See
   "Boarding vs Refresh" below and tracking issue lightninglabs/darepo#225
   for the design rationale.

The capital-committing case treats each operation as a zero-coupon bond: the
operator advances liquid BTC now and reclaims it later when the old round can
be swept.

## Fee Formula

For **refresh** and **offboard** (capital-committing operations), the
per-operation fee is:

```
F(A, B, δ; r_eff, ε) = F_round/B  +  A · (max(δ, δ_min)/365) · r_eff  +  ε
                       ──────────     ─────────────────────────────────     ─
                       on-chain       liquidity cost (with floor)            operator
                       share                                                 margin
```

For **boarding**, only the on-chain share and margin apply:

```
F_boarding(B; ε) = F_round/B  +  ε
```

In both cases, `r_eff = r + Δ(u)` is the effective annual rate including
any congestion spread (see "Congestion Pricing" below). `δ_min` is the
refresh liquidity fee floor (see "Fee floor `δ_min`").

| Variable | Definition | Unit |
|----------|-----------|------|
| A | Amount being refreshed or boarded | sats |
| B | Batch size (participants in the round) | count |
| F_round | Total on-chain cost of the round transaction | sats |
| δ | Remaining lifetime of forfeited VTXO (refresh/offboard only; not used for boarding) | days |
| r_eff | Effective annual rate (base rate + congestion spread) | fraction |
| ε | Fixed operator margin (`base_margin_sat`) | sats |

## Components

### 1. On-Chain Share: `F_round / B`

The round commitment transaction cost is amortized across all participants.
Larger batches reduce the per-user share. This component does **not** scale with
amount; it is a fixed cost per participant.

The round cost is estimated from transaction weight:

```
F_round ≈ feeRate × (baseWeight + B × perOutputWeight + B/2 × connectorWeight + inputWeight + changeWeight)
```

Here `baseWeight` is the fixed transaction overhead (~42 WU for version,
locktime, segwit marker/flag, varint counts), `perOutputWeight` is the
weight contributed by each participant's VTXO tree root output, and the
`B/2 × connectorWeight` term accounts for the connector outputs used in the
forfeit covenant.

### 2. Liquidity Cost: `A · (δ/365) · r_eff`

This is the time-value-of-money cost. For a **refresh**, the user forfeits an
old VTXO `V1` (backed by the old round's shared output `O1`) in exchange for a
new VTXO `V2` in a fresh shared output `O2`. `δ` is the remaining lifetime of
the forfeited VTXO, computed from block heights:

```
remainingBlocks = confirmationHeight + csvDelay - currentHeight
δ = remainingBlocks × 10 minutes / (60 × 24)  [in days]
```

**Why δ and not L?** Between the refresh moment and the old round's sweep at
`now + δ`, the operator is doubly committed: they still hold `A` in `O1` (now
fully forfeited to the operator) **and** `A` in `O2` (backing `V2`). The old
round is only reclaimable after `δ` days, so the refresh costs the operator an
extra `A · δ` capital-days of lock-up compared to letting `V1` simply expire.
Charging `A · (δ/365) · r` exactly compensates for this double-commitment
window. After `O1` is swept, the operator recycles that capital back into the
wallet and the ongoing `V2` exposure is "paid for" when the user next
refreshes.

For **boarding**, there is no forfeited VTXO and no operator capital lockup.
The user brings their own on-chain BTC into the shared output; the operator's
wallet is unchanged. Boarding therefore pays **no liquidity fee** — only the
on-chain share `F_round/B` and the flat margin `ε`. See "Boarding vs Refresh"
below for the full design rationale.

### Fee floor `δ_min`

To prevent a "lazy refresh" bypass (refreshing at `δ ≈ 0` just before
expiry for a near-zero fee), the refresh liquidity fee is computed against
a floored effective remaining lifetime:

```
liquidity_fee_refresh(A, δ) = A · max(δ, δ_min) / 365 · r_eff
```

where `δ_min` is configured in blocks as `min_refresh_delta_blocks` (default
`144` ≈ 1 day at 10-minute blocks).

**Important**: this is a *pricing floor, not an admission rule*. The
operator still accepts refreshes at `δ < δ_min` — users near expiry
always retain the option to refresh rather than being forced into a more
expensive unilateral on-chain exit. The floor only affects how the
liquidity fee is computed, so the minimum refresh fee a user can pay is
`A · δ_min / 365 · r_eff` regardless of how close to expiry they wait.

Race-safety concerns (a refresh that cannot complete before the old
round's sweep deadline) are handled separately at the implementation
level, not via the fee floor.

**Annualized liquidity cost**: A user who holds a VTXO of value `A` across a
full year and refreshes at a fixed cadence such that the old VTXO has `x` days
remaining at each refresh (refresh interval `L - x`) pays a total annual
liquidity cost of:

```
C_liq_annual(x) = (365 / (L - x)) · A · (x / 365) · r  =  A · r · x / (L - x)
```

This is **not** constant across refresh strategies:

| Refresh strategy         | `x`        | `C_liq_annual`  |
|--------------------------|------------|-----------------|
| Eagerly (refresh early)  | `x → L`    | `→ ∞`           |
| Half-lifetime            | `x = L/2`  | `A · r`         |
| Lazily (refresh at expiry) | `x → 0`  | `→ 0`           |

The headline `A · r` figure is the cost at the **half-lifetime refresh point**,
which is also the cadence that minimizes the total annual cost (liquidity +
on-chain + margin) when on-chain and margin fees dominate for small
amounts. Lazier cadences reduce liquidity cost but increase the number of
rounds a user must join; the operator may also cap how close to expiry a
refresh can be scheduled.

### 3. Operator Margin: `ε`

A flat per-operation margin (in sats) covering operational overhead. Set via
`base_margin_sat` in the fee schedule.

## Boarding vs Refresh

Boarding and refresh pay different fee components:

| Operation | On-chain share | Liquidity fee | Margin | `δ` used |
|-----------|---------------|---------------|--------|----------|
| Boarding  | ✅ | ❌ (not charged) | ✅ | — |
| Refresh   | ✅ | ✅ `A · max(δ, δ_min) / 365 · r_eff` | ✅ | remaining lifetime of forfeited VTXO |
| Offboard  | ✅ | ✅ same as refresh | ✅ | remaining lifetime of forfeited VTXO |

**Why boarding is free of liquidity fee**: boarding brings on-chain BTC
from the user into the shared output. The operator's own wallet is
unchanged and no new operator capital is locked up. The double-commitment
rationale that justifies the refresh liquidity fee (operator has `A` in
both the old and new shared outputs for `δ` days) has no analog in
boarding — there is no prior UTXO the operator is waiting to recover.
Charging a liquidity fee at boarding would be double-counting relative to
the refresh fees the user will pay over the VTXO's lifetime.

This matches the framing in tracking issue lightninglabs/darepo#163 and
is tracked as an explicit design decision in
lightninglabs/darepo#225, which also documents the arguments for the
alternative (service-obligation framing, where every user-day of live
VTXO would be priced uniformly).

**Anti-bypass**: without a boarding fee, a user could theoretically refresh
at `δ ≈ 0` (just before expiry) for a near-zero liquidity fee, getting
essentially free VTXO service. The `δ_min` fee floor on refresh
(`liquidity_fee = A · max(δ, δ_min) / 365 · r_eff`) closes that bypass
without locking users out: users can still refresh at any `δ`, but the
liquidity component never drops below the floor value.

## Congestion Pricing

When the operator's treasury utilization exceeds a threshold, a spread is added
to the base rate:

```
r_eff = r + Δ(u)

Δ(u) = 0                        if u ≤ u*
Δ(u) = Δ₀ + Δ₁ · (u - u*)       if u > u*
```

| Parameter | Config Field | Description |
|-----------|-------------|-------------|
| r | `annual_rate` | Base annual cost of capital (e.g. 0.05 = 5%) |
| Δ₀ | `util_spread_delta0_bps` | Base spread step at threshold (basis points) |
| Δ₁ | `util_spread_delta1_bps` | Linear spread coefficient past threshold (bps) |
| u* | `util_threshold_bps` | Threshold (basis points, e.g. 7000 = 70%) |

**Discontinuity at `u = u*`**: Crossing the threshold causes `r_eff` to jump
by `Δ₀` (1% with defaults). This step is intentional — it gives the operator
a clear "in congestion" signal that is easy to reason about operationally and
that dominates noise in the utilization estimate. The tradeoff is that
refreshing at `u = u* − ε` is materially cheaper than refreshing at
`u = u* + ε`, which can incentivize users to race across the boundary.
Operators who want a smooth curve can configure `Δ₀ = 0`, in which case
`Δ(u)` is a continuous linear ramp rooted at the threshold.

### Treasury Utilization

```
u       = K / K_total
K       = deployed capital (sum of live VTXO amounts)
K_total = K + K_pending + confirmed wallet balance
```

`K_total` is the operator's total accessible BTC at the current instant: the
portion currently committed to live VTXOs, plus capital in transition
(`K_pending` — forfeited but not yet swept), plus the portion still idle in
the wallet. It is recomputed on every round confirmation, forfeit, sweep,
and wallet balance change.

`K_pending` exists to prevent a transient utilization spike during the
window between VTXO forfeit and sweep confirmation. When VTXOs are
forfeited, their value moves from `K` (deployed) to `K_pending` (pending
sweep) rather than disappearing from `K_total`. The sweep clears
`K_pending`; the confirmed wallet balance catches up asynchronously.

A separate `K_cap` config field (planned) can be used to bound `K_total`
from above — the operator will refuse new capital commitments beyond that
ceiling.

Utilization tracking is implemented by `TreasuryTracker` in the `fees`
package.

## Minimum Viable VTXO

A VTXO is "economically viable" when its fee does not exceed `MinViableVTXOPct`
percent of its value. Below this threshold, the VTXO is effectively dust from a
fee perspective.

The minimum viable amount is the `A_min` threshold above which the policy
holds:

```
A_min = (F_round/B + ε) / (pct/100 − δ/365 · r_eff)
```

**Precondition**: `(δ/365) · r_eff < pct/100`. If the liquidity rate alone
already exceeds the viability percentage (e.g. `δ = 365`, `r_eff = 0.6`, or
`δ = 180`, `r_eff = 1.0` at `pct = 50%`), the denominator is ≤ 0 and **no**
amount is economically viable — the operator should blanket-reject all
VTXOs in that regime regardless of size, and the fee estimator should
surface this as a `RateTooHigh` condition rather than producing a negative
`A_min`.

A separate "security dust" threshold exists based on unilateral exit cost
(tree depth scales with the chosen radix `R`):

```
ExitCost(B, R) ≈ feerate × (ceil(log_R(B)) × branchVBytes(R) + claimVBytes)
```

where `branchVBytes(R)` increases with `R` because each internal branch
transaction reveals `R−1` sibling hashes as witness data. There is a
`U`-shaped tradeoff: low `R` means many levels with small branch txs; high
`R` means few levels with large branch txs. The explorer's Chart 5 shows
this curve and the optimal radix that minimizes exit cost for a given
`(B, feerate)`.

If `A < ExitCost(B, R)`, the user cannot profitably exit on-chain. The two
dust thresholds are enforced independently:

- **Security dust** (`A < ExitCost`) is **always hard-rejected** by the
  operator at registration time, regardless of `MinViableVTXOPolicy`.
  Accepting a security-dust VTXO would force the user into permanent
  custody of the operator, which the protocol does not allow.
- **Economic dust** (`A < A_min` but `A ≥ ExitCost`) is governed by
  `MinViableVTXOPolicy`:
  - **reject** (default): rejects the VTXO at registration.
  - **warn**: accepts but flags the fee estimate with `BelowMinViable` so
    the client can warn the user.

## Out-of-Round (OOR) Transfers

OOR transfers are **free** (zero fee) in the current model. The operator
co-signs the transfer but does not deploy new capital: the shared output and
the operator's already-committed reserves continue to back the same total
value, just with a different split of claims. The user's fee obligation was
established when the anchor VTXO was created (boarding/refresh), and the
recipient will eventually pay a fee when they refresh.

### Known open questions

Zero-fee OOR has two known pricing gaps that are tracked separately and are
likely to change the model:

1. **OOR dust trap**: an OOR transfer can create a recipient VTXO below
   `max(A_min, ExitCost)` — the recipient can neither refresh (economic
   loss) nor exit (security loss), and the operator carries the latent
   obligation with no fee revenue.
2. **OOR chain depth/size**: a long OOR spend chain descending from one
   anchor increases operator sweep, checkpoint, and state-tracking costs
   even though no new capital is deployed. The flat zero fee does not
   signal this.

Both are tracked in issue lightninglabs/darepo#224 and should be considered
open design questions for the fee model, not settled behavior.

## Fee Schedule Configuration

Defaults (suitable for regtest/development):

```toml
[fees]
annual_rate              = 0.05      # 5% annual cost of capital
base_margin_sat          = 100       # 100 sat margin per operation
util_threshold_bps       = 7000      # 70% utilization threshold
util_spread_delta0_bps   = 100       # 1% base congestion spread step
util_spread_delta1_bps   = 500       # 5% linear spread coefficient
min_refresh_delta_blocks = 144       # ~1 day fee floor on refresh δ
min_viable_policy        = "reject"  # reject economic-dust VTXOs
min_viable_pct           = 50        # fee must be < 50% of amount
```

### Hot Reload

Fee parameters can be updated at runtime via the admin RPC `UpdateFeeSchedule`
without restarting the daemon. Changes take effect on the next round. Each
change is logged to the `fee_schedule_history` table for audit.

## Worked Examples

### Example 1: Boarding 1,000,000 sats

Parameters: `B = 64`, `feerate = 10 sat/vB`, `ε = 100`. Boarding does
**not** charge a liquidity fee — only on-chain share and margin:

```
Liquidity fee = 0 sats (boarding is not capital-committing)
On-chain share ≈ EstimateRoundCost(64, 10 sat/vB) / 64 ≈ 437 sats
Margin = 100 sats
Total fee ≈ 537 sats (0.054% of amount)
```

### Example 2: Refreshing 100,000 sats at congestion

Parameters: `r = 5%`, `B = 64`, `remainingBlocks = 500 (3.47 days)`,
`feerate = 20 sat/vB`, `ε = 100`, `u = 0.85` (above 70% threshold),
`δ_min = 144 blocks (1.0 day)`.

```
r_eff = 0.05 + 0.01 + 0.05 × (0.85 - 0.70) = 0.0675
δ = 3.47 days (> δ_min, floor inactive)
Liquidity fee = 100,000 × (3.47/365) × 0.0675 = 64 sats
On-chain share ≈ EstimateRoundCost(64, 20 sat/vB) / 64 ≈ 874 sats
Margin = 100 sats
Total fee ≈ 1,038 sats (1.04% of amount)
```

### Example 3: Lazy refresh against the δ_min floor

Same user as Example 2 but waits until the VTXO has only 50 blocks
(~0.35 days) left. `δ_min = 144 blocks = 1 day` kicks in:

```
actual δ = 0.35 days
effective δ = max(0.35, 1.0) = 1.0 day (floor)
r_eff = 0.0675 (same as Example 2)
Liquidity fee = 100,000 × (1.0/365) × 0.0675 ≈ 18 sats (floor-priced)
On-chain share ≈ 874 sats
Margin = 100 sats
Total fee ≈ 992 sats (0.99% of amount)
```

Without the floor, the liquidity fee would have been
`100,000 × (0.35/365) × 0.0675 ≈ 6 sats` and the total fee ~980 sats.
The floor adds ~12 sats of liquidity revenue that the operator would
otherwise have forgone.

## Double-Entry Accounting

All capital movements and fee events are recorded in a double-entry ledger.
The chart of accounts follows the standard asset / liability / equity /
revenue / expense split; every entry has balanced debits and credits.

| Account              | Type      | Purpose                                       |
|----------------------|-----------|-----------------------------------------------|
| `treasury_wallet`    | asset     | Confirmed on-chain wallet balance the operator fully controls |
| `deployed_capital`   | asset     | Capital committed to live shared outputs (backs user VTXOs)  |
| `user_vtxo_claims`   | liability | Outstanding off-chain claims held by users (sum of live VTXO values) |
| `operator_revenue`   | revenue   | Cumulative fees earned                        |
| `mining_fees`        | expense   | Cumulative on-chain mining fees paid          |

`treasury_wallet + deployed_capital` is the operator's total BTC. From the
user side, `user_vtxo_claims` is the aggregate liability the operator owes
them. The difference is operator equity (not a separately tracked account
here — it is computed as total assets minus total liabilities).

### Ledger Entries by Event

| Event                   | Debit                  | Credit                 | Amount            |
|-------------------------|------------------------|------------------------|-------------------|
| Boarding — user deposit | `deployed_capital`     | `user_vtxo_claims`     | boarded value − fee |
| Boarding — fee          | `deployed_capital`     | `operator_revenue`     | fee amount        |
| Refresh — forfeit       | `user_vtxo_claims`     | `deployed_capital`     | old VTXO value    |
| Refresh — new VTXO      | `deployed_capital`     | `user_vtxo_claims`     | new VTXO value    |
| Refresh — fee           | `user_vtxo_claims`     | `operator_revenue`     | fee amount        |
| Offboard                | `user_vtxo_claims`     | `treasury_wallet`      | payout amount     |
| Mining fee paid         | `mining_fees`          | `treasury_wallet`      | miner fee         |
| Round sweep             | `treasury_wallet`      | `deployed_capital`     | swept value       |
| Capital committed       | `deployed_capital`     | `treasury_wallet`      | new round value   |

Notes:

- Boarding is split into two entries because the user's deposit simultaneously
  increases `deployed_capital` (the shared output backs the new VTXO) and
  creates a new `user_vtxo_claims` liability net of the fee, while the fee
  portion is recognized as revenue.
- Refresh is three entries: the forfeit retires the old claim, the new VTXO
  issues a fresh one, and the fee reduces the user's claim by the fee amount
  in favor of `operator_revenue`.
- `operator_revenue` is a revenue account; recognizing fees against it
  increases equity. The debit side draws from an asset account
  (`deployed_capital`) on boarding (the fee is carved out of the deposit
  that lands in the shared output) and from the liability side
  (`user_vtxo_claims`) on refresh (the user's outstanding claim is reduced
  by the fee).

## Admin RPCs

| RPC | Purpose |
|-----|---------|
| `GetFeeSchedule` | Returns current fee parameters |
| `UpdateFeeSchedule` | Hot-reload fee parameters |
| `GetTreasuryStatus` | Deployed capital, utilization, K_total |
| `ListFeeEvents` | Paginated ledger entries |

## Client RPCs

| RPC | Purpose |
|-----|---------|
| `EstimateFee` | Returns fee breakdown for a given amount |

## Interactive Explorer

See `docs/fee-model-explorer.html` for an interactive visualization with
adjustable parameters and live-updating charts.
