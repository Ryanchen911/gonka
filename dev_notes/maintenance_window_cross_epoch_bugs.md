# Maintenance Window: Two Cross-Epoch Bugs

Notes on two related bugs in the maintenance-window feature (PR #998), both rooted in
multi-epoch maintenance windows. Failing regression tests for both are committed alongside
these notes.

## TL;DR

| | Bug 1 | Bug 2 |
|---|---|---|
| Where | `x/inference/keeper/msg_server_claim_rewards.go` — `hasSignificantMissedValidations` | `x/inference/keeper/maintenance_validation.go` — `checkEpochPhaseOverlap` |
| What | Claim-rewards exempts the missed-validation check only when `LastMaintenanceEpoch == claimedEpoch`, i.e. only for the window's *start* epoch. Mid/end epochs of a multi-epoch window are not exempt → claim is rejected with `ErrValidationsMissed`, `Amount=0`. | The PoC/DKG phase-overlap check only examines the effective epoch + a fixed `maxFutureEpochsToCheck = 5` future epochs. A window long enough to reach past that horizon can overlap a PoC commit / DKG phase in an unscanned epoch without being rejected. |
| Impact | High — directly costs the participant the rewards for the covered epochs, violating the proposal's "Maintenance does not pause ordinary reward eligibility". | Allows scheduling a window that overlaps an epoch-critical phase the proposal explicitly forbids (`ErrMaintenanceOverlapsPoCPhase` / `ErrMaintenanceOverlapsDKGPhase`). Also enables Bug 1 in practice (see below). |
| Tests | `TestMsgServer_ClaimRewards_MaintenanceWindowExemptsValidationAcrossEpochRange` | `TestScheduleMaintenance_RejectsPoCOverlapBeyondFixedFutureEpochScan` |

## Background: epoch geometry

An epoch is `EpochLength` blocks long (`DefaultEpochParams.EpochLength = 40`; production is much
larger). Each epoch (except the genesis epoch 0) has a block-aligned "busy" region at its start —
PoC generation/exchange, PoC validation, then `SetNewValidators` (the DKG / validator-set switch) —
followed by a large "free" region used for normal inference serving. Maintenance windows are only
meaningful in the free region; the proposal forbids overlapping the PoC commit/exchange phase and
the DKG phase.

Because an epoch's first block *is* its PoC start, the epoch boundary itself sits inside a busy
region. So a window that crosses an epoch boundary necessarily touches the next epoch's PoC start.
That means: under correct operation, a cross-epoch window should always be rejected by
`checkEpochPhaseOverlap`. A cross-epoch window getting *accepted* is itself the symptom of Bug 2.

## Bug 1 — claim-rewards validation exemption only covers the start epoch

### Symptom

A participant schedules a maintenance window spanning epochs N..N+k. Claiming rewards for epoch N
succeeds, but claiming epochs N+1..N+k fails with:

```
Inference validation missed significantly   /   ErrValidationsMissed   /   Amount=0
```

— because the participant was offline during those epochs, did none of its assigned validation
duties, the binomial missed-validation test fails, and the maintenance exemption did not apply.

### Root cause

`hasSignificantMissedValidations` exempts maintenance-covered participants like this:

```go
if found && state.LastMaintenanceEpoch == msg.EpochIndex && msg.EpochIndex != 0 {
    return false, nil   // exempt
}
```

`LastMaintenanceEpoch` is set at activation to the epoch the window *started* in and never
changes. The equality check therefore only matches the start epoch. `MaintenanceState` actually
tracks the full covered range (`LastMaintenanceEpoch` .. `LastMaintenanceEndEpoch`, the latter set
at completion) plus the in-progress overlap via `ActiveReservationId` + `activeReservationCoversEpoch`.

`GrantMaintenanceCredit` (in `maintenance.go`) already does the correct, full check — it suppresses
credit accrual for any epoch covered by the window, using both the active-reservation overlap and
the `[LastMaintenanceEpoch, LastMaintenanceEndEpoch]` range. `hasSignificantMissedValidations` does
neither beyond the single `==`. The two functions answer the same question — "is this epoch covered
by the participant's maintenance window?" — with inconsistent logic. That asymmetry is the bug.

Note: even switching `==` to the range check is not enough on its own. `LastMaintenanceEndEpoch` is
only written at completion (BeginBlock), so a claim that arrives while the window is still ACTIVE
needs the `ActiveReservationId` + `activeReservationCoversEpoch` branch too. The fix must mirror
`GrantMaintenanceCredit` exactly — ideally factor the check into a shared
`epochCoveredByMaintenance(ctx, addr, epochIndex)` used by both call sites.

Residual limitation (out of scope of the immediate fix): `MaintenanceState` only tracks the *most
recent* window, so an out-of-order claim for an old epoch covered by an *older* window still would
not be exempt. `GrantMaintenanceCredit`'s comment already acknowledges this. Fully fixing it would
require per-epoch tracking.

### Trigger conditions

- A multi-epoch maintenance window has been activated for the participant
  (`LastMaintenanceEpoch != LastMaintenanceEndEpoch`).
- There were inferences in the mid/end epochs of the window that the participant was assigned (by
  epoch-group weight) to validate. `getMustBeValidatedInferences` does *not* exclude maintenance-
  covered participants, so a participant in an epoch group still draws validation duty.
- `ClaimValidationEnabled` is on.

Whether a multi-epoch window can be *scheduled* in the first place is gated by `checkEpochPhaseOverlap`
— i.e. by Bug 2 (see below). Under default params with a correct overlap check, cross-epoch windows
are essentially unschedulable, so Bug 1 is dormant. With Bug 2's blind spot — or a governance change
to `EpochLength` after a window is scheduled, or future changes to epoch geometry — the precondition
becomes reachable and Bug 1 bites.

## Bug 2 — PoC/DKG overlap check uses a fixed future-epoch lookahead

### Symptom

`ScheduleMaintenance` accepts a window that overlaps an epoch-critical phase (PoC commit/exchange
or DKG), instead of rejecting it with `ErrMaintenanceOverlapsPoCPhase` / `ErrMaintenanceOverlapsDKGPhase`.

### Root cause

`checkEpochPhaseOverlap` builds the list of epochs to check as: the effective epoch, plus at most
`maxFutureEpochsToCheck = 5` next epochs, stopping early only if a next epoch starts after the
window ends. It then checks the window's `[start, end]` range against each of those (≤6) epochs'
PoC generation/exchange, PoC validation, and `SetNewValidators` heights.

The loop has two exits: (a) `next.StartOfPoC() > endHeight` — safe, the window can't reach further;
(b) `i >= maxFutureEpochsToCheck` — *unsafe*, the window may still extend into epoch +6, +7, ...
whose busy regions are never examined. A window positioned to sit entirely in the free regions of
the scanned epochs but whose tail lands on an unscanned epoch's PoC start is accepted.

The comment defends the fixed `5` with: "5 is sufficient because the maximum maintenance window
duration is governance-capped well below 5 full epoch lengths." But nothing enforces that:
`MaintenanceMaxWindowBlocks` is a governance param whose only validation is `> 1e15` → reject (the
overflow guard), which is astronomically larger than `5 * EpochLength`. Even the default
(`MaintenanceMaxWindowBlocks = 200`, `EpochLength = 40`) is exactly `5 * EpochLength` — right at the
edge of the claim.

### Trigger conditions

- A window long enough (and positioned such) that it overlaps a PoC/DKG phase in an epoch beyond
  `effectiveEpoch + maxFutureEpochsToCheck`. Easiest with a large `MaintenanceMaxWindowBlocks`, but
  also reachable by scheduling a window far in the future (there is no upper bound on `StartHeight`)
  whose covered epochs slide past the scan horizon.

### Suggested fix

Don't use a fixed lookahead. Either (a) keep extending the checked-epochs list until
`next.StartOfPoC() > endHeight` (the correct termination), with a sane hard cap purely for DoS
protection; and/or (b) enforce in `Params.Validate()` that `MaintenanceMaxWindowBlocks < N * EpochLength`
for a small N, so a fixed lookahead is genuinely sufficient and is a guarantee rather than a comment.
Doing both is safest.

## Relationship between the two bugs

They are related but not identical:

- **Bug 2 is the practical trigger for Bug 1.** A multi-epoch `MaintenanceState`
  (`LastMaintenanceEpoch != LastMaintenanceEndEpoch`) is the precondition for Bug 1, and the only
  realistic way to obtain one in production is via Bug 2's hole in `checkEpochPhaseOverlap` — because
  the epoch boundary is a PoC start, so a correct overlap check rejects every cross-epoch window.
  Fixing Bug 2 alone would make Bug 1 effectively unreachable in practice.

- **But Bug 1 is still a real, independent code-level bug.** It is an inconsistency between two
  functions that answer the same question (`GrantMaintenanceCredit` correct, `hasSignificantMissedValidations`
  wrong). Other paths can resurrect the precondition without Bug 2 — e.g. a governance change to
  `EpochLength` after a window is scheduled (the overlap check is not re-run), or future changes to
  epoch geometry. And the codebase already *intends* to handle multi-epoch windows: `GrantMaintenanceCredit`
  + `activeReservationCoversEpoch` exist precisely for that case, and the proposal's credit model
  speaks of "any epoch in which a maintenance window was activated". So Bug 1 should be fixed on its
  own merits, not just as a side effect of fixing Bug 2.

Conclusion: fix both. Bug 2 closes the door; Bug 1 fixes what happens if the door is ever opened
again. Defense in depth.

## What the proposal says (and doesn't)

`proposals/maintenance-windows/maintenance-windows.md` does **not** forbid cross-epoch windows. The
only epoch-related scheduling constraints are "must not overlap the PoC commit/exchange window" and
"must not overlap the DKG window". The Credit Model section explicitly talks about suppressing credit
for "any epoch in which a maintenance window was activated", which presumes multi-epoch windows are
possible and must be handled. So the *intent* is that multi-epoch windows are handled correctly; the
*implementation* mostly prevents them via epoch geometry; Bug 2 punches a hole in that prevention;
Bug 1 mishandles them when they slip through.

## Tests added (committed with these notes)

- `inference-chain/x/inference/keeper/msg_server_claim_rewards_test.go` —
  `TestMsgServer_ClaimRewards_MaintenanceWindowExemptsValidationAcrossEpochRange`: sets
  `MaintenanceState{LastMaintenanceEpoch: 5, LastMaintenanceEndEpoch: 7}`, claims epoch 6 with
  unvalidated inferences present, expects the claim to succeed (`Amount == WorkCoins + RewardCoins`).
  Currently RED — claim is rejected with `ErrValidationsMissed`, `Amount == 0`.
- `inference-chain/x/inference/keeper/maintenance_test.go` —
  `TestScheduleMaintenance_RejectsPoCOverlapBeyondFixedFutureEpochScan`: with a large
  `MaintenanceMaxWindowBlocks`, schedules a window that starts in the free region of the last
  scanned epoch and ends on the PoC start of the first *unscanned* epoch, expects
  `ErrMaintenanceOverlapsPoCPhase`. Currently RED — the window is accepted.

Both should turn GREEN once the corresponding fixes land.
